package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLSPFrameBounds(t *testing.T) {
	term := string([]byte{13, 10, 13, 10})
	frame := []byte("Content-Length: 11" + term + "{\"ok\":true}" + "extra")
	body, used, err := readLSPFrame(frame)
	if err != nil || string(body) != "{\"ok\":true}" || used != len(frame)-5 {
		t.Fatalf("valid frame: body=%q used=%d err=%v", body, used, err)
	}
	if _, n, err := readLSPFrame(frame[:used-1]); err != nil || n != 0 {
		t.Fatalf("partial frame: used=%d err=%v", n, err)
	}
	for name, raw := range map[string]string{
		"missing_length":   "Content-Type: application/vscode-jsonrpc; charset=utf-8" + term + "{}",
		"duplicate_length": "Content-Length: 2" + string([]byte{13, 10}) + "Content-Length: 2" + term + "{}",
		"oversize":         "Content-Length: " + strconv.Itoa(lspMaxFrameBytes+1) + term + "{}",
		"bad_length":       "Content-Length: nope" + term + "{}",
	} {
		if _, _, err := readLSPFrame([]byte(raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, _, err := readLSPFrame([]byte(strings.Repeat("A", lspMaxHeaderBytes+1))); err == nil {
		t.Fatal("unbounded header accepted")
	}
}

func fixtureLSPPacket(reader *bufio.Reader) (lspPacket, error) {
	length := -1
	for {
		header, err := reader.ReadString(byte(10))
		if err != nil {
			return lspPacket{}, err
		}
		header = strings.TrimSpace(header)
		if header == "" {
			break
		}
		field, value, ok := strings.Cut(header, ":")
		if ok && strings.EqualFold(field, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return lspPacket{}, err
			}
		}
	}
	if length <= 0 || length > lspMaxFrameBytes {
		return lspPacket{}, errors.New("invalid fixture frame")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return lspPacket{}, err
	}
	var packet lspPacket
	err := json.Unmarshal(payload, &packet)
	return packet, err
}

func fixtureLSPSend(writer io.Writer, message any) error {
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "Content-Length: %d%s%s", len(b), string([]byte{13, 10, 13, 10}), b)
	return err
}

// Runs only as an explicitly selected child test binary. It simulates a real
// long-lived language server with a server-initiated configuration request.
func TestLSPFixtureProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "agent-lsp-fixture" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	haveOpen := false
	for {
		msg, err := fixtureLSPPacket(reader)
		if err != nil {
			os.Exit(11)
		}
		switch msg.Method {
		case "initialize":
			if err := fixtureLSPSend(os.Stdout, map[string]any{
				"jsonrpc": "2.0", "id": "server-1", "method": "workspace/configuration",
				"params": map[string]any{"items": []any{map[string]any{"section": "gopls"}}},
			}); err != nil {
				os.Exit(12)
			}
			reply, err := fixtureLSPPacket(reader)
			if err != nil || string(reply.ID) != "\"server-1\"" || string(reply.Result) != "[null]" {
				os.Exit(13)
			}
			if err := fixtureLSPSend(os.Stdout, map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{"capabilities": map[string]any{"definitionProvider": true}},
			}); err != nil {
				os.Exit(14)
			}
		case "initialized":
			_ = fixtureLSPSend(os.Stdout, map[string]any{
				"jsonrpc": "2.0", "method": "window/logMessage",
				"params": map[string]any{"type": 3, "message": "initialized"},
			})
		case "textDocument/didOpen":
			haveOpen = strings.Contains(string(msg.Params), "Hello")
		case "textDocument/definition":
			if !haveOpen || !strings.Contains(string(msg.Params), "\"line\":2") {
				os.Exit(15)
			}
			var params map[string]any
			_ = json.Unmarshal(msg.Params, &params)
			doc, _ := params["textDocument"].(map[string]any)
			uri, _ := doc["uri"].(string)
			_ = fixtureLSPSend(os.Stdout, map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{
					"uri": uri,
					"range": map[string]any{
						"start": map[string]int{"line": 1, "character": 5},
						"end":   map[string]int{"line": 1, "character": 10},
					},
				},
			})
		default:
			os.Exit(16)
		}
	}
}

func TestWorkerGoDefinitionUsesSharedStdioBroker(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	script := filepath.Join(workspace, "fixture-gopls")
	// A helper subprocess exercises real pipes, framing, ordered notification,
	// server->client request/reply and the bounded executor-owned session.
	if err := os.WriteFile(script, []byte(
		fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestLSPFixtureProcess$ -- agent-lsp-fixture\n", os.Args[0]),
	), 0700); err != nil {
		t.Fatal(err)
	}
	source := "package demo\nfunc Hello() {}\nfunc main() { Hello() }\n"
	file := filepath.Join(workspace, "main.go")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	location, err := s.workerGoDefinition(ctx, owner, script, file, source, 2, 14)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(location, &result); err != nil {
		t.Fatalf("invalid definition JSON: %v", err)
	}
	loc, _ := result["uri"].(string)
	rng, _ := result["range"].(map[string]any)
	start, _ := rng["start"].(map[string]any)
	if !strings.HasSuffix(loc, "/main.go") || start["line"] != float64(1) {
		t.Fatalf("unexpected location: %s", location)
	}
	_, err = s.workerGoDefinition(ctx, owner, script, filepath.Join(workspace, "..", "other.go"), source, 2, 14)
	if err == nil || !strings.Contains(err.Error(), "outside_workspace") {
		t.Fatalf("outside workspace accepted: %v", err)
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("session leaked after consumer: %d", active)
	}
}

// Run explicitly with AGENT_GOPLS_PROOF_BIN to verify the same private consumer
// against real gopls, without making gopls a new core/runtime dependency.
func TestWorkerGoDefinitionRealGopls(t *testing.T) {
	gopls := os.Getenv("AGENT_GOPLS_PROOF_BIN")
	if gopls == "" {
		t.Skip("set AGENT_GOPLS_PROOF_BIN for a real-gopls acceptance run")
	}
	info, err := os.Stat(gopls)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("missing pinned proof binary %q: %v", gopls, err)
	}
	s, owner, workspace := testStdioBroker(t)
	source := "package demo\nfunc Hello() {}\nfunc main() { Hello() }\n"
	file := filepath.Join(workspace, "main.go")
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.org/lsp-proof\n\ngo 1.26.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(workspace, "gopls-runtime")
	if err := os.WriteFile(wrapper, []byte(
		fmt.Sprintf("#!/bin/sh\nPATH=/srv/ai-workspace/.toolchains/go1.27.1/bin:$PATH\nexport PATH\nexec %q \"$@\"\n", gopls),
	), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	location, err := s.workerGoDefinition(ctx, owner, wrapper, file, source, 2, 14)
	if err != nil {
		t.Fatalf("real gopls definition: %v", err)
	}
	if !strings.Contains(string(location), "\"line\":1") && !strings.Contains(string(location), "\"line\": 1") {
		t.Fatalf("real gopls did not locate Go declaration: %s", location)
	}
	t.Logf("real gopls definition resolved: %s", location)
}
