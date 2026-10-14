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
	lspMaxFrameBytes  = 128 << 10
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
	// An LSP notification is asynchronous: retain only the latest bounded
	// diagnostics for documents explicitly opened by this request. Never
	// reuse diagnostics between principals, worktrees or adapter sessions.
	opened    map[string]int
	published map[string]json.RawMessage
}

func newLSPWire(server *Server, owner Request, sessionID string) *lspWire {
	return &lspWire{server: server, owner: owner, sessionID: sessionID,
		opened: make(map[string]int), published: make(map[string]json.RawMessage)}
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
			if packet.Method == "textDocument/publishDiagnostics" {
				if err := w.recordDiagnostics(packet); err != nil {
					return lspPacket{}, err
				}
			}
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
			// A request may already have changed language-server state. Best
			// effort LSP cancellation precedes broker teardown on deadline.
			if ctx.Err() != nil {
				_ = w.notify("$/cancelRequest", map[string]any{"id": id})
			}
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

// recordDiagnostics does not accept server-invented source versions or
// unopened/out-of-scope document URIs. The caller still proves the worker
// file_version after receiving the result, including for versionless push.
func (w *lspWire) recordDiagnostics(packet lspPacket) error {
	if len(packet.ID) != 0 || len(packet.Params) > 16<<10 {
		return errors.New("lsp_invalid_diagnostics_notification")
	}
	var payload struct {
		URI         string          `json:"uri"`
		Version     *int            `json:"version"`
		Diagnostics json.RawMessage `json:"diagnostics"`
	}
	if json.Unmarshal(packet.Params, &payload) != nil {
		return errors.New("lsp_invalid_diagnostics_notification")
	}
	openedVersion, ok := w.opened[payload.URI]
	if !ok {
		return nil // Other files are not read authority or caller results.
	}
	if payload.Version != nil && *payload.Version != openedVersion {
		return errors.New("lsp_diagnostics_document_version_mismatch")
	}
	if len(payload.Diagnostics) == 0 || len(payload.Diagnostics) > 16<<10 ||
		payload.Diagnostics[0] != '[' || !json.Valid(payload.Diagnostics) {
		return errors.New("lsp_invalid_diagnostics_items")
	}
	report, err := json.Marshal(map[string]any{"kind": "full", "items": payload.Diagnostics})
	if err != nil || len(report) > 16<<10 {
		return errors.New("lsp_diagnostics_result_too_large")
	}
	w.published[payload.URI] = report
	return nil
}

// waitPublishedDiagnostics handles server-initiated requests while waiting.
// A missing notification is a failure, never an invented clean result.
func (w *lspWire) waitPublishedDiagnostics(ctx context.Context, uri string) (json.RawMessage, error) {
	for {
		if report, ok := w.published[uri]; ok {
			return report, nil
		}
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
		if packet.Method == "" {
			return nil, errors.New("lsp_unexpected_diagnostics_response")
		}
	}
}

// Use UTF-16 as the sole advertised position encoding until conversions
// have substantive adapter fixtures; reject a server's incompatible choice.
type lspServerCapabilities struct {
	PositionEncoding   string          `json:"positionEncoding"`
	DiagnosticProvider json.RawMessage `json:"diagnosticProvider"`
}

func parseLSPServerCapabilities(raw json.RawMessage) (lspServerCapabilities, error) {
	var reply struct {
		Capabilities lspServerCapabilities `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return lspServerCapabilities{}, err
	}
	if reply.Capabilities.PositionEncoding != "" && reply.Capabilities.PositionEncoding != "utf-16" {
		return lspServerCapabilities{}, errors.New("lsp_unsupported_position_encoding")
	}
	return reply.Capabilities, nil
}

func (c lspServerCapabilities) pullDiagnostics() bool {
	value := strings.TrimSpace(string(c.DiagnosticProvider))
	return value != "" && value != "false" && value != "null"
}

func (w *lspWire) notify(method string, params any) error {
	if err := w.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		return err
	}
	if method == "textDocument/didOpen" {
		var opened struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
		}
		value, err := json.Marshal(params)
		if err != nil || json.Unmarshal(value, &opened) != nil ||
			opened.TextDocument.URI == "" || opened.TextDocument.Version <= 0 ||
			len(w.opened) >= codeMaxTargetFiles+1 {
			return errors.New("lsp_invalid_document_open")
		}
		w.opened[opened.TextDocument.URI] = opened.TextDocument.Version
	}
	return nil
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
	if len(source) > codeMaxDocumentBytes {
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
