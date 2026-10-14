package executor

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildNodeProofWorkspaceHelper(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go compiler unavailable for real workspace version helper")
	}
	helper := filepath.Join(t.TempDir(), "asa-node-workspace-helper")
	cmd := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	cmd.Dir = filepath.Join("..", "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot build worker workspace helper: %v %s", err, output)
	}
	return helper
}

// The real Microsoft adapter reports threadId=0. This is a valid DAP thread
// but the original Go-only Delve contract rejected non-positive values.
// Exercise the actual Executor debug_action surface, not only the wire parser.
func TestNodeDAPZeroThreadIDThroughExistingDebugTool(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	id, err := s.workerStdioSession(owner, "dap", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.workerStdioClose(owner, id) }()
	e, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	client, provider := net.Pipe()
	defer provider.Close()
	d := &dapState{
		conn: client, workspace: workspace, adapter: "javascript/js-debug",
		stage: "stopped", configured: true,
	}
	e.mu.Lock()
	e.dap = d
	e.mu.Unlock()
	complete := make(chan error, 1)
	go func() {
		req, readErr := dapFixtureRead(bufio.NewReader(provider))
		if readErr != nil {
			complete <- readErr
			return
		}
		if req.Command != "stackTrace" {
			complete <- os.ErrInvalid
			return
		}
		complete <- dapFrame(provider, map[string]any{
			"seq": 2, "type": "response", "request_seq": req.Seq,
			"command": "stackTrace", "success": true,
			"body": map[string]any{"stackFrames": []any{map[string]any{
				"id": 0, "name": "main", "line": 2, "column": 1, "source": map[string]any{
					"path": filepath.Join(workspace, "main.js"),
				},
			}}},
		})
	}()
	owner.Action = "debug_action"
	owner.SessionID = id
	owner.DebugMethod = "stackTrace"
	owner.DebugThreadID = 0
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := s.debugAction(ctx, owner)
	if !result.OK || !strings.Contains(result.Output, "main.js") {
		t.Fatalf("Node debug_stack failed for real zero thread ID: %+v", result)
	}
	select {
	case err := <-complete:
		if err != nil {
			t.Fatalf("fake DAP transport: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DAP fake responder hung")
	}
}

func TestNodeDAPSocketRequiresOriginalPinnedAdapterPeer(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	id, err := s.workerStdioSession(owner, "dap", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.workerStdioClose(owner, id) }()
	entry, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "dap.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = waitPinnedNodeDAPPeer(ctx, entry, s.workerUID, socket)
	if err == nil || !strings.Contains(err.Error(), "peer_mismatch") {
		t.Fatalf("same-UID but wrong-PID DAP Unix listener accepted: %v", err)
	}
}

func TestNodeDAPUncontainedConfigureFailsBeforeAnyWireWrite(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	id, err := s.workerStdioSession(owner, "dap", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.workerStdioClose(owner, id) }()
	entry, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	client, adapter := net.Pipe()
	defer client.Close()
	defer adapter.Close()
	d := &dapState{conn: client, parentConn: adapter, adapter: "javascript/js-debug",
		workspace: workspace, stage: "starting"}
	entry.mu.Lock()
	entry.dap = d
	entry.mu.Unlock()
	owner.Action = "debug_action"
	owner.SessionID = id
	owner.DebugMethod = "configure"
	result := s.debugAction(context.Background(), owner)
	if result.OK || result.ErrorCode != "javascript_dap_containment_unverified" {
		t.Fatalf("Node configure without original cgroup was allowed: %+v", result)
	}
	if err := adapter.SetReadDeadline(time.Now().Add(70 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var buf [32]byte
	if n, err := adapter.Read(buf[:]); n != 0 || err == nil {
		t.Fatalf("uncontained Node configured on the adapter wire: n=%d err=%v", n, err)
	}
}

func TestNodeDAPNoRepeatedOrUnreconciledParentReverse(t *testing.T) {
	l, r := net.Pipe()
	defer l.Close()
	defer r.Close()
	d := &dapState{conn: l, adapter: "javascript/js-debug", pendingLaunchSeq: 10}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := d.receiveTypedNodeChild(ctx); err == nil ||
		!strings.Contains(err.Error(), "parent_launch_result_unproven") {
		t.Fatalf("Node accepted child before parent launch outcome: %v", err)
	}
	d.pendingLaunchSeq = 0
	d.parentConn = r
	if _, err := d.receiveTypedNodeChild(ctx); err == nil ||
		!strings.Contains(err.Error(), "repeated_child") {
		t.Fatalf("Node accepted second target connection: %v", err)
	}
}

func TestJavaScriptDAPRejectsUntrustedModesAndSourcesBeforeSpawning(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	owner.Action = "debug_launch"
	owner.Language = "javascript"
	owner.DebugProgram = "demo.js"
	owner.FileVersion = "v1:expected"
	owner.DebugMode = "remote"
	if r := s.debugAction(context.Background(), owner); r.OK || r.ErrorCode != "javascript_dap_unsupported_mode" {
		t.Fatalf("javascript debugger granted arbitrary remote mode: %+v", r)
	}
	owner.DebugMode = "node"
	for _, path := range []string{"../outside.js", "main.ts", "package.json", "../../root.js"} {
		owner.DebugProgram = path
		result := s.debugAction(context.Background(), owner)
		if result.OK || result.ErrorCode != "javascript_dap_invalid_program" {
			t.Fatalf("unreviewed JS program %q admitted: %+v", path, result)
		}
	}
	owner.DebugProgram = "demo.js"
	owner.Root = true
	owner.Approval = true
	if got := s.debugAction(context.Background(), owner); got.OK || got.ErrorCode != "debug_worker_only" {
		t.Fatalf("privileged JS debugger admitted: %+v", got)
	}
	s.sessions.mu.Lock()
	sessions := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("invalid Javascript requests spawned %d sessions", sessions)
	}
}

func TestNodeDAPAdapterStatusPreservesUnsupportedBrowserModes(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	owner.Action = "debug_adapter_status"
	owner.Language = "javascript"
	result := s.debugAction(context.Background(), owner)
	if !result.OK || !strings.Contains(result.Output, `"adapter":"javascript/js-debug"`) ||
		!strings.Contains(result.Output, `"modes":["node"]`) ||
		!strings.Contains(result.Output, `"attach_supported":false`) ||
		strings.Contains(result.Output, "browser") ||
		strings.Contains(result.Output, "extension") {
		t.Fatalf("Node DAP fabricated unverified mode: %+v", result)
	}
	if result.Status == "ready" {
		t.Fatalf("adapter provenance conflated with actual installed cgroup readiness: %+v", result)
	}
}

func TestNodeDAPUncontainedRealSourceFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires ordinary unprivileged worker identity")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	helper := buildNodeProofWorkspaceHelper(t)
	s.workspaceHelperBinary = helper
	path := workspace + "/demo.js"
	if err := os.WriteFile(path, []byte("console.log(42)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	version := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "demo.js"})
	if !version.OK {
		t.Fatalf("worker source identity unavailable: %+v", version)
	}
	owner.Action = "debug_launch"
	owner.Language = "javascript"
	owner.DebugMode = "node"
	owner.DebugProgram = "demo.js"
	owner.FileVersion = version.FileVersion
	out := s.debugAction(context.Background(), owner)
	if out.OK || out.ErrorCode != "dap_containment_unavailable" || out.SessionID != "" {
		t.Fatalf("Node target spawned without executor-owned cgroup: %+v", out)
	}
}

func TestNodeDAPTrustedTypedStartDebuggingRejectsArbitraryReverse(t *testing.T) {
	tests := []struct {
		name    string
		args    any
		command string
		valid   bool
	}{
		{"valid", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": "abcdef1234abcdef1234"}}, "startDebugging", true},
		{"run_terminal", map[string]any{"kind": "integrated", "args": []string{"/bin/sh"}}, "runInTerminal", false},
		{"remote", map[string]any{"request": "attach", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": "abcdef1234abcdef1234"}}, "startDebugging", false},
		{"injected_port", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": "abcdef1234abcdef1234", "port": 4444}}, "startDebugging", false},
		{"injected_executable", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": "abcdef1234abcdef1234", "runtimeExecutable": "/bin/sh"}}, "startDebugging", false},
		{"browser", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-chrome", "__pendingTargetId": "abcdef1234abcdef1234"}}, "startDebugging", false},
		{"injected_target", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": "../evil;curl"}}, "startDebugging", false},
		{"empty_target", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": ""}}, "startDebugging", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			consumer, provider := net.Pipe()
			defer consumer.Close()
			defer provider.Close()
			d := &dapState{conn: consumer, adapter: "javascript/js-debug"}
			done := make(chan error, 1)
			go func() {
				if err := dapFrame(provider, map[string]any{"seq": 5, "type": "request", "command": tt.command, "arguments": tt.args}); err != nil {
					done <- err
					return
				}
				if tt.valid {
					_ = provider.SetReadDeadline(time.Now().Add(time.Second))
					resp, err := dapFixtureRead(bufio.NewReader(provider))
					if err != nil {
						done <- err
						return
					}
					if resp.Type != "response" || resp.Command != "startDebugging" || resp.RequestSeq != 5 || !resp.Success {
						done <- os.ErrInvalid
						return
					}
				}
				done <- nil
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			token, err := d.receiveTypedNodeChild(ctx)
			if tt.valid {
				if err != nil || token != "abcdef1234abcdef1234" {
					t.Fatalf("typed Node child denied: %q %v", token, err)
				}
			} else if err == nil {
				t.Fatalf("untrusted reverse %s allowed", tt.name)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("fixture transport failed: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("reverse DAP handler hung")
			}
		})
	}
}
