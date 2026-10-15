package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Child fixture: a push-only LSP adapter with no pull diagnostics capability.
// It deliberately attempts an unsolicited workspace mutation before sending
// valid or stale push diagnostics, verifying that read-only clients deny it.
func TestLSPAsyncFixtureProcess(t *testing.T) {
	if len(os.Args) == 0 {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode != "agent-lsp-push" && mode != "agent-lsp-stale" &&
		mode != "agent-lsp-silent" && mode != "agent-lsp-other" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		msg, err := fixtureLSPPacket(reader)
		if err != nil {
			return
		}
		switch msg.Method {
		case "initialize":
			if err := fixtureLSPSend(os.Stdout, map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{"capabilities": map[string]any{
					"positionEncoding":       "utf-16",
					"documentSymbolProvider": true,
				}},
			}); err != nil {
				os.Exit(31)
			}
		case "initialized":
		case "textDocument/didOpen":
			var opened struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
				} `json:"textDocument"`
			}
			if json.Unmarshal(msg.Params, &opened) != nil || opened.TextDocument.URI == "" || opened.TextDocument.Version != 1 {
				os.Exit(32)
			}
			if err := fixtureLSPSend(os.Stdout, map[string]any{
				"jsonrpc": "2.0", "id": 101, "method": "workspace/applyEdit",
				"params": map[string]any{"edit": map[string]any{"changes": map[string]any{
					opened.TextDocument.URI: []any{map[string]any{"newText": "malicious mutation"}},
				}}},
			}); err != nil {
				os.Exit(33)
			}
			denied, err := fixtureLSPPacket(reader)
			if err != nil || string(denied.ID) != "101" || denied.Error == nil || denied.Error.Code != -32601 {
				os.Exit(34)
			}
			if mode == "agent-lsp-silent" {
				continue
			}
			version := 1
			if mode == "agent-lsp-stale" {
				version = 2
			}
			uri := opened.TextDocument.URI
			if mode == "agent-lsp-other" {
				// An unsolicited report from another file cannot satisfy the
				// caller's exact-document readiness or expose its diagnostics.
				uri += ".unopened"
			}
			if err := fixtureLSPSend(os.Stdout, map[string]any{
				"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": uri, "version": version,
					"diagnostics": []any{map[string]any{
						"message": "actual pushed syntax error", "severity": 1,
						"range": map[string]any{
							"start": map[string]int{"line": 1, "character": 0},
							"end":   map[string]int{"line": 1, "character": 5},
						},
					}},
				},
			}); err != nil {
				os.Exit(35)
			}
		default:
			// The client may send a best-effort cancellation on timeout.
			if msg.Method != "$/cancelRequest" {
				os.Exit(36)
			}
		}
	}
}

func TestLSPPushDiagnosticsAndReadOnlyServerRequests(t *testing.T) {
	for _, tc := range []struct {
		name, mode, wantErr string
	}{
		{"push", "agent-lsp-push", ""},
		{"stale", "agent-lsp-stale", "lsp_diagnostics_document_version_mismatch"},
		{"absent", "agent-lsp-silent", "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, owner, workspace := testStdioBroker(t)
			fixture := filepath.Join(workspace, "lsp-test-adapter")
			script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestLSPAsyncFixtureProcess$ -- %s\n", os.Args[0], tc.mode)
			if err := os.WriteFile(fixture, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(workspace, "main.go")
			source := "package main\nfunc main() { missing() }\n"
			if err := os.WriteFile(file, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			timeout := 5 * time.Second
			if tc.name == "absent" {
				timeout = 400 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			result, err := s.workerGoInspection(ctx, owner, fixture, file, source, "diagnostics", 0, 0)
			if tc.wantErr == "" {
				if err != nil || !strings.Contains(string(result), "actual pushed syntax error") ||
					!strings.Contains(string(result), `"kind":"full"`) {
					t.Fatalf("push diagnostics not correlated: result=%s err=%v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("unexpected push failure: result=%s err=%v", result, err)
			}
			content, err := os.ReadFile(file)
			if err != nil || string(content) != source {
				t.Fatal("read-only LSP accepted a server-originated workspace write")
			}
			s.sessions.mu.Lock()
			active := len(s.sessions.sessions)
			s.sessions.mu.Unlock()
			if active != 0 {
				t.Fatalf("LSP session leak after %s: %d", tc.name, active)
			}
		})
	}
}

func TestLSPCapabilitiesFailClosedOnIncompatibleEncoding(t *testing.T) {
	for _, tc := range []struct {
		reply string
		pull  bool
		err   bool
	}{
		{`{"capabilities":{"diagnosticProvider":{"interFileDependencies":true},"positionEncoding":"utf-16"}}`, true, false},
		{`{"capabilities":{"positionEncoding":"utf-16"}}`, false, false},
		{`{"capabilities":{"diagnosticProvider":false}}`, false, false},
		{`{"capabilities":{"positionEncoding":"utf-8"}}`, false, true},
		{`{"capabilities":{"documentSymbolProvider":"unsupported"}}`, false, true},
	} {
		caps, err := parseLSPServerCapabilities(json.RawMessage(tc.reply))
		if (err != nil) != tc.err || (err == nil && caps.pullDiagnostics() != tc.pull) {
			t.Errorf("capability negotiation: %s => %+v, %v", tc.reply, caps, err)
		}
	}
	if _, err := parseLSPServerCapabilities(json.RawMessage(`not-json`)); err == nil {
		t.Fatal("invalid capability response accepted")
	}
	capabilities, err := parseLSPServerCapabilities(json.RawMessage(`{"capabilities":{"definitionProvider":true,"referencesProvider":false,"documentSymbolProvider":{"label":"symbols"}}}`))
	if err != nil || !capabilities.supports("definition") || capabilities.supports("references") ||
		!capabilities.supports("symbols") || capabilities.supports("runWorkspaceCommand") {
		t.Fatalf("unsafe server capability interpretation: %+v / %v", capabilities, err)
	}
}

// A push-only adapter can advertise semantic capability before it has
// finished indexing didOpen. Do not treat an empty premature result or a
// notification for an unopened document as proof of readiness.
func TestLSPInitialPushReadinessFailsClosed(t *testing.T) {
	for _, mode := range []string{"agent-lsp-silent", "agent-lsp-other"} {
		t.Run(mode, func(t *testing.T) {
			s, owner, workspace := testStdioBroker(t)
			fixture := filepath.Join(workspace, "ready-fixture")
			script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestLSPAsyncFixtureProcess$ -- %s\n", os.Args[0], mode)
			if err := os.WriteFile(fixture, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(workspace, "main.sh")
			source := "#!/bin/sh\necho ready\n"
			if err := os.WriteFile(file, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			profile := lspInspectionProfile{
				extensions:         map[string]string{".sh": "shellscript"},
				waitForInitialPush: true,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
			defer cancel()
			value, err := s.workerLSPInspection(ctx, owner, profile, fixture, file, source, "symbols", 0, 0)
			if err == nil || !strings.Contains(err.Error(), "lsp_initial_document_not_ready") ||
				!strings.Contains(err.Error(), "context deadline exceeded") {
				t.Fatalf("unproven push readiness accepted: value=%s err=%v", value, err)
			}
			s.sessions.mu.Lock()
			active := len(s.sessions.sessions)
			s.sessions.mu.Unlock()
			if active != 0 {
				t.Fatalf("pending adapter session leaked: %d", active)
			}
		})
	}
}

// Stale diagnostics from Rust's synthetic version 1 never stand in for
// worker-authorized version 2; unversioned reports cannot masquerade as
// current evidence either. Standard adapters retain strict mismatch errors.
func TestRustLSPDocumentVersionAndWorkspaceStatus(t *testing.T) {
	for _, sample := range []struct {
		status string
		ready  bool
	}{
		{"No workspaces\n\nAnalysis:", false},
		{"", false},
		{"Workspaces:\nLoaded 11 packages across 1 workspace.", true},
		{"Workspaces: Loaded 1 package across 1 workspace.", true},
		{"Workspaces:\nLoading 11 packages.", false},
		{"No workspaces\nWorkspaces: Loaded 1 package", false},
	} {
		if got := rustWorkspaceStatusReady(sample.status); got != sample.ready {
			t.Fatalf("Rust workspace readiness %q: got %t want %t", sample.status, got, sample.ready)
		}
	}
	uri := "file:///workspace/src/lib.rs"
	makeNotice := func(v any) lspPacket {
		raw, err := json.Marshal(map[string]any{
			"uri": uri, "version": v, "diagnostics": []any{
				map[string]any{"message": "real analyzer issue", "severity": 1},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return lspPacket{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: raw}
	}
	w := &lspWire{
		opened:                  map[string]int{uri: 2},
		published:               map[string]json.RawMessage{},
		initialSyntheticVersion: map[string]int{uri: 1},
	}
	if err := w.recordDiagnostics(makeNotice(1)); err != nil {
		t.Fatalf("stale pre-replacement Rust notification must be discarded: %v", err)
	}
	if len(w.published) != 0 {
		t.Fatal("stale Rust diagnostics entered user result")
	}
	if err := w.recordDiagnostics(makeNotice(nil)); err == nil {
		t.Fatal("versionless Rust report accepted after synthetic source replacement")
	}
	if err := w.recordDiagnostics(makeNotice(3)); err == nil {
		t.Fatal("future Rust diagnostic version accepted")
	}
	if err := w.recordDiagnostics(makeNotice(2)); err != nil || len(w.published[uri]) == 0 {
		t.Fatalf("current Rust diagnostic version rejected: %v", err)
	}
	w.initialSyntheticVersion = map[string]int{}
	if err := w.recordDiagnostics(makeNotice(1)); err == nil {
		t.Fatal("default adapter stale version rejection regressed")
	}
}
