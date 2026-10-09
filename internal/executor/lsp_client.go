package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The LSP framing/semantic consumer belongs to #56; it uses #66's executor-
// private stdio broker instead of opening a second process/control boundary.
// This is deliberately not a public generic JSON-RPC or MCP session API.
const (
	lspMaxFrameBytes  = 64 << 10
	lspMaxHeaderBytes = 1024
	lspPollInterval   = 20 * time.Millisecond
)

type lspWire struct {
	server    *Server
	owner     Request
	sessionID string
	cursor    uint64
	input     []byte
	nextID    int
}

func newLSPWire(server *Server, owner Request, sessionID string) *lspWire {
	return &lspWire{server: server, owner: owner, sessionID: sessionID}
}

func (w *lspWire) send(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > lspMaxFrameBytes {
		return errors.New("lsp_request_too_large")
	}
	frame := make([]byte, 0, len(body)+80)
	frame = append(frame, "Content-Length: "...)
	frame = strconv.AppendInt(frame, int64(len(body)), 10)
	frame = append(frame, "\r\n\r\n"...)
	frame = append(frame, body...)
	for len(frame) > 0 {
		n := len(frame)
		if n > maxStdioInputBytes {
			n = maxStdioInputBytes
		}
		if err := w.server.workerStdioWrite(w.owner, w.sessionID, frame[:n]); err != nil {
			return err
		}
		frame = frame[n:]
	}
	return nil
}

type lspPacket struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func readLSPFrame(buffer []byte) (body []byte, consumed int, err error) {
	end := bytes.Index(buffer, []byte("\r\n\r\n"))
	if end < 0 {
		if len(buffer) > lspMaxHeaderBytes {
			return nil, 0, errors.New("lsp_header_too_large")
		}
		return nil, 0, nil
	}
	if end > lspMaxHeaderBytes {
		return nil, 0, errors.New("lsp_header_too_large")
	}
	length := -1
	for _, line := range strings.Split(string(buffer[:end]), "\r\n") {
		label, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, 0, errors.New("lsp_invalid_header")
		}
		if strings.EqualFold(strings.TrimSpace(label), "Content-Length") {
			if length >= 0 {
				return nil, 0, errors.New("lsp_duplicate_content_length")
			}
			n, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil || n <= 0 || n > lspMaxFrameBytes {
				return nil, 0, errors.New("lsp_invalid_content_length")
			}
			length = n
		}
	}
	if length < 0 {
		return nil, 0, errors.New("lsp_missing_content_length")
	}
	start := end + 4
	if len(buffer)-start < length {
		return nil, 0, nil
	}
	return buffer[start : start+length], start + length, nil
}

func (w *lspWire) next(ctx context.Context) (lspPacket, error) {
	for {
		if body, consumed, err := readLSPFrame(w.input); err != nil {
			return lspPacket{}, err
		} else if consumed > 0 {
			var packet lspPacket
			if err := json.Unmarshal(body, &packet); err != nil || packet.JSONRPC != "2.0" {
				return lspPacket{}, errors.New("lsp_invalid_jsonrpc_frame")
			}
			w.input = w.input[consumed:]
			return packet, nil
		}
		state, err := w.server.sessions.read(w.owner, w.sessionID, w.cursor, maxStdioOutputBytes)
		if err != nil {
			return lspPacket{}, err
		}
		if state.Truncated {
			return lspPacket{}, errors.New("lsp_output_retention_lost")
		}
		for _, event := range state.Events {
			w.cursor = event.Sequence
			if event.Stream != "stdout" {
				continue // Diagnostic stderr is never interpreted as JSON-RPC.
			}
			if len(w.input)+len(event.Data) > lspMaxFrameBytes+lspMaxHeaderBytes+4 {
				return lspPacket{}, errors.New("lsp_output_frame_too_large")
			}
			w.input = append(w.input, event.Data...)
		}
		if len(state.Events) != 0 {
			continue
		}
		if !state.Running {
			return lspPacket{}, fmt.Errorf("lsp_server_exited: %d", state.ExitCode)
		}
		select {
		case <-ctx.Done():
			return lspPacket{}, ctx.Err()
		case <-time.After(lspPollInterval):
		}
	}
}

func (w *lspWire) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	w.nextID++
	id := w.nextID
	if err := w.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		packet, err := w.next(ctx)
		if err != nil {
			return nil, err
		}
		if packet.Method != "" && len(packet.ID) != 0 {
			if err := w.replyToServer(packet); err != nil {
				return nil, err
			}
			continue
		}
		if packet.Method != "" {
			continue // An asynchronous notification, not a response.
		}
		if string(packet.ID) != strconv.Itoa(id) {
			return nil, errors.New("lsp_unexpected_response_id")
		}
		if packet.Error != nil {
			return nil, fmt.Errorf("lsp_response_error(%d): %s", packet.Error.Code, packet.Error.Message)
		}
		if len(packet.Result) == 0 {
			return nil, errors.New("lsp_missing_result")
		}
		return packet.Result, nil
	}
}

func (w *lspWire) replyToServer(packet lspPacket) error {
	var result any
	if packet.Method == "workspace/configuration" {
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(packet.Params, &params); err != nil {
			return errors.New("lsp_invalid_configuration_request")
		}
		result = make([]any, len(params.Items))
	} else if packet.Method != "window/workDoneProgress/create" && packet.Method != "client/registerCapability" {
		return w.send(map[string]any{
			"jsonrpc": "2.0", "id": packet.ID,
			"error": map[string]any{"code": -32601, "message": "Method not found"},
		})
	}
	return w.send(map[string]any{"jsonrpc": "2.0", "id": packet.ID, "result": result})
}

func (w *lspWire) notify(method string, params any) error {
	return w.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// workerGoDefinition is the first bounded, read-only consumer of the shared
// broker. Its caller must supply version-consistent source via #55's worker
// file read; this private helper never reads workspace files as root. No
// public tool or untrusted caller interface is published by this slice.
func (s *Server) workerGoDefinition(ctx context.Context, req Request, goplsPath, file string, source string, line, character int) (json.RawMessage, error) {
	if line < 0 || character < 0 || line > 1<<20 || character > 1<<20 {
		return nil, errors.New("lsp_invalid_position")
	}
	if !utf8.ValidString(source) {
		return nil, errors.New("lsp_invalid_document_utf8")
	}
	if len(source) > lspMaxFrameBytes/2 {
		return nil, errors.New("lsp_document_too_large")
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(file) || filepath.Ext(file) != ".go" {
		return nil, errors.New("lsp_file_must_be_absolute_go_source")
	}
	relative, err := filepath.Rel(workspace, filepath.Clean(file))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("lsp_file_outside_workspace")
	}
	file = filepath.Clean(file)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	id, err := s.workerStdioSession(req, "lsp", workspace, goplsPath, "serve")
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.workerStdioClose(req, id) }()
	wire := newLSPWire(s, req, id)
	root := (&url.URL{Scheme: "file", Path: workspace}).String()
	uri := (&url.URL{Scheme: "file", Path: file}).String()
	if _, err := wire.call(ctx, "initialize", map[string]any{
		"processId": nil, "rootUri": root,
		"workspaceFolders": []map[string]any{{"uri": root, "name": filepath.Base(workspace)}},
		"capabilities":     map[string]any{"workspace": map[string]any{"configuration": true}},
	}); err != nil {
		return nil, err
	}
	if err := wire.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	if err := wire.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": source},
	}); err != nil {
		return nil, err
	}
	return wire.call(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]string{"uri": uri},
		"position":     map[string]int{"line": line, "character": character},
	})
}
