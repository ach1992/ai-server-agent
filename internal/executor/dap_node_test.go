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

	"github.com/ach1992/ai-server-agent/internal/systemexec"
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

func TestNodeDAPStatusRequiresPinnedBrowserRuntime(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	owner.Action = "debug_adapter_status"
	owner.Language = "javascript"
	result := s.debugAction(context.Background(), owner)
	if !result.OK || !strings.Contains(result.Output, `"adapter":"javascript/js-debug"`) ||
		!strings.Contains(result.Output, `"attach_supported":false`) ||
		strings.Contains(result.Output, "extension") {
		t.Fatalf("Node DAP fabricated unverified attach/extension: %+v", result)
	}
	_, browserErr := systemexec.JavaScriptBrowser()
	mode := `"modes":["node"]`
	if browserErr == nil {
		mode = `"modes":["node","browser"]`
	}
	if !strings.Contains(result.Output, mode) {
		t.Fatalf("Browser readiness not tied to exact pinned Chromium artifact: %+v", result)
	}
	if result.Status == "ready" {
		t.Fatalf("adapter provenance conflated with actual installed cgroup readiness: %+v", result)
	}
	owner.Language = "typescript"
	tsResult := s.debugAction(context.Background(), owner)
	if !tsResult.OK || !strings.Contains(tsResult.Output, `"modes":["node"]`) {
		t.Fatalf("unproven browser TypeScript source mapping promised: %+v", tsResult)
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

func TestJavaScriptBrowserDAPLocalOnlyAndNoUncontainedLaunch(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	owner.Action, owner.Language, owner.DebugMode = "debug_launch", "javascript", "browser"
	owner.DebugProgram, owner.FileVersion = "local.html", "v1:expected"
	for _, name := range []string{"../outside.html", "/etc/passwd", "remote.js", "local.ts", "x.php"} {
		owner.DebugProgram = name
		r := s.debugAction(t.Context(), owner)
		if r.OK || r.ErrorCode != "javascript_dap_invalid_program" || r.SessionID != "" {
			t.Fatalf("external/non-HTML Browser debug admitted: %+v", r)
		}
	}
	owner.DebugProgram = "local.html"
	owner.Language = "typescript"
	if r := s.debugAction(t.Context(), owner); r.OK || r.ErrorCode != "typescript_browser_source_map_unverified" {
		t.Fatalf("unverified browser TypeScript debug allowed: %+v", r)
	}
	owner.Language = "javascript"
	owner.DebugMode = "extension"
	if r := s.debugAction(t.Context(), owner); r.OK || r.ErrorCode != "javascript_dap_unsupported_mode" {
		t.Fatalf("unreviewed extension host launched: %+v", r)
	}
	if os.Geteuid() == 0 {
		t.Skip("worker cgroup denial requires unprivileged test")
	}
	requireWorkerLandlockV2(t)
	owner.DebugMode = "browser"
	s.workspaceHelperBinary = buildNodeProofWorkspaceHelper(t)
	source := filepath.Join(workspace, "local.html")
	if err := os.WriteFile(source, []byte("<!doctype html><script src=local.js></script>"), 0600); err != nil {
		t.Fatal(err)
	}
	v := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: "local.html"})
	if !v.OK {
		t.Fatalf("worker HTML version identity unavailable: %+v", v)
	}
	owner.FileVersion = v.FileVersion
	denied := s.debugAction(t.Context(), owner)
	if denied.OK || denied.ErrorCode != "dap_containment_unavailable" || denied.SessionID != "" {
		t.Fatalf("uncontained Browser/Chromium debug admitted: %+v", denied)
	}
	if err := os.WriteFile(source, []byte("<!doctype html><h1>changed</h1>"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := s.debugAction(t.Context(), owner)
	if stale.OK || stale.ErrorCode != "debug_program_changed" {
		t.Fatalf("stale Browser HTML accepted: %+v", stale)
	}
	s.sessions.mu.Lock()
	count := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if count != 0 {
		t.Fatalf("invalid Browser launch left %d sessions", count)
	}
}

func TestJavaScriptBrowserDAPProfileIdentityNotUserDeletable(t *testing.T) {
	outsider := t.TempDir()
	if err := removeBrowserDAPProfile(outsider); err == nil {
		t.Fatalf("browser cleanup accepted arbitrary selected directory")
	}
	if _, err := os.Stat(outsider); err != nil {
		t.Fatalf("Browser cleanup deleted a user directory: %v", err)
	}
	if os.Geteuid() != 0 {
		if _, _, err := createBrowserDAPProfile(uint32(os.Getuid()), uint32(os.Getgid())); err == nil {
			t.Fatal("unprivileged runtime created root-owned Browser profile")
		}
	}
}

func TestJavaScriptBrowserDAPTypedUppercaseChildReverseNoEscape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  map[string]any
		valid bool
	}{
		{"valid_browser_uppercase", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-chrome", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C"}}, true},
		{"browser_remote_attach", map[string]any{"request": "attach", "configuration": map[string]any{"type": "pwa-chrome", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C"}}, false},
		{"untrusted_browser_url", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-chrome", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C", "url": "https://remote"}}, false},
		{"untrusted_browser_port", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-chrome", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C", "port": 9222}}, false},
		{"untrusted_browser_executable", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-chrome", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C", "runtimeExecutable": "/bin/sh"}}, false},
		{"node_target_in_browser_mode", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C"}}, false},
		{"bad_browser_token", map[string]any{"request": "launch", "configuration": map[string]any{"type": "pwa-chrome", "__pendingTargetId": "x$()"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, provider := net.Pipe()
			defer client.Close()
			defer provider.Close()
			d := &dapState{conn: client, adapter: "javascript/js-debug", browserMode: true}
			done := make(chan error, 1)
			go func() {
				err := dapFrame(provider, map[string]any{"seq": 1, "type": "request", "command": "startDebugging", "arguments": tc.args})
				if err != nil {
					done <- err
					return
				}
				if tc.valid {
					_ = provider.SetReadDeadline(time.Now().Add(time.Second))
					packet, e := dapFixtureRead(bufio.NewReader(provider))
					if e != nil {
						done <- e
						return
					}
					if packet.Type != "response" || packet.RequestSeq != 1 || !packet.Success {
						done <- os.ErrInvalid
						return
					}
				}
				done <- nil
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			token, err := d.receiveTypedNodeChild(ctx)
			if tc.valid && (err != nil || token != "9FFA7743642C2F40E6F77F4A4E89E83C") {
				t.Fatalf("typed pinned browser request wrongly rejected: %s %v", token, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("untrusted browser command admitted: %+v", tc.args)
			}
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("browser reverse fixture did not settle")
			}
		})
	}
}
