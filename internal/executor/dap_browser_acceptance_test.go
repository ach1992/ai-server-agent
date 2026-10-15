package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in, unprivileged proof against a real pinned Microsoft js-debug and
// Chromium pipe. NOT proof of root Executor cgroups or installed MCP launch.
func TestRealMicrosoftBrowserDAPLocalHTMLPipe(t *testing.T) {
	node := os.Getenv("AGENT_BROWSER_DAP_PROOF_NODE")
	adapter := os.Getenv("AGENT_BROWSER_DAP_PROOF_CLI")
	chromium := os.Getenv("AGENT_BROWSER_DAP_PROOF_CHROMIUM")
	if node == "" && adapter == "" && chromium == "" {
		t.Skip("requires explicit trusted offline Browser DAP proof fixture")
	}
	if node == "" || adapter == "" || chromium == "" {
		t.Fatal("incomplete Browser DAP proof fixture")
	}
	if os.Geteuid() == 0 {
		t.Skip("unprivileged Browser fixture must not launch under root")
	}
	for _, exe := range []string{node, adapter, chromium} {
		fi, err := os.Lstat(exe)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("native/browser proof fixture must be regular: %s (%v)", exe, err)
		}
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.workspaceHelperBinary = buildNodeProofWorkspaceHelper(t)
	program := filepath.Join(workspace, "browser_fixture.html")
	script := filepath.Join(workspace, "browser_fixture.js")
	html := "<!doctype html><html><body><h1>Local Browser DAP</h1><script src=\"browser_fixture.js\"></script></body></html>"
	js := "function twice(v) {\n const output = v * 2;\n return output;\n}\nsetTimeout(()=>twice(21),2300);\n"
	if err := os.WriteFile(program, []byte(html), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(js), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"browser_fixture.html", "browser_fixture.js"} {
		response := s.workerWorkspaceFile(t.Context(), Request{
			Action: "workspace_stat", Workspace: workspace, Path: name,
		})
		if !response.OK || response.FileVersion == "" {
			t.Fatalf("source identity unavailable: %s %+v", name, response)
		}
	}
	socket := filepath.Join(t.TempDir(), "browser-dap.sock")
	profile := filepath.Join(t.TempDir(), "disposable-browser-profile")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	id, err := s.workerStdioSession(owner, "dap", workspace, node, adapter, socket)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			if err := s.workerStdioClose(owner, id); err != nil {
				t.Errorf("original private adapter broker cleanup: %v", err)
			}
		}
	}()
	entry, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := waitPinnedNodeDAPPeer(ctx, entry, s.workerUID, socket)
	if err != nil {
		t.Fatalf("original Browser adapter identity: %v", err)
	}
	defer parent.Close()
	d := &dapState{conn: parent, browserMode: true, adapter: "javascript/js-debug", workspace: workspace, stage: "starting"}
	init := map[string]any{
		"clientID": "ai-server-agent", "adapterID": "pwa-chrome",
		"linesStartAt1": true, "columnsStartAt1": true,
		"pathFormat": "path", "supportsRunInTerminalRequest": false,
	}
	if _, err := d.call(ctx, "initialize", init); err != nil {
		t.Fatalf("real Browser parent initialize: %v", err)
	}
	if _, err := d.callDeferredLaunch(ctx, map[string]any{
		"request": "launch", "type": "pwa-chrome", "name": "Agent local Browser",
		"file": program, "cwd": workspace, "runtimeExecutable": chromium,
		"runtimeArgs": []string{"--headless=new", "--disable-dev-shm-usage"},
		"port":        0, "webRoot": workspace, "userDataDir": profile,
		"cleanUp": "wholeBrowser", "sourceMaps": true,
	}); err != nil {
		t.Fatalf("real Chromium-pipe launch: %v", err)
	}
	if _, err := d.call(ctx, "configurationDone", map[string]any{}); err != nil {
		t.Fatalf("parent configuration: %v", err)
	}
	target, err := d.receiveTypedNodeChild(ctx)
	if err != nil {
		stage, events, _ := d.peek()
		var diag []string
		for _, raw := range events {
			var event dapPacket
			if json.Unmarshal(raw, &event) == nil {
				body := string(event.Body)
				if len(body) > 350 {
					body = body[:350]
				}
				diag = append(diag, event.Event+":"+body)
			}
		}
		t.Fatalf("real typed Browser child rejected: %v stage=%s events=%v", err, stage, diag)
	}
	child, err := waitPinnedNodeDAPPeer(ctx, entry, s.workerUID, socket)
	if err != nil {
		t.Fatalf("Browser child not authenticated to original adapter: %v", err)
	}
	defer child.Close()
	d.mu.Lock()
	d.parentConn, d.conn = d.conn, child
	d.pending, d.seq, d.initialized, d.pendingLaunchSeq = nil, 0, false, 0
	d.events, d.eventBytes, d.stage = nil, 0, "starting"
	d.mu.Unlock()
	if _, err := d.call(ctx, "initialize", init); err != nil {
		t.Fatalf("Browser child initialize: %v", err)
	}
	if _, err := d.callDeferredLaunch(ctx, map[string]any{
		"request": "launch", "type": "pwa-chrome",
		"name": "Agent local HTML target", "__pendingTargetId": target,
	}); err != nil {
		t.Fatalf("real Browser child launch: %v", err)
	}
	set, err := d.call(ctx, "setBreakpoints", map[string]any{
		"source":      map[string]string{"path": script},
		"breakpoints": []map[string]int{{"line": 3}}, "sourceModified": false,
	})
	if err != nil || !strings.Contains(string(set), "breakpoints") {
		t.Fatalf("JS source breakpoints: %v %s", err, set)
	}
	if _, err := d.call(ctx, "configurationDone", map[string]any{}); err != nil {
		t.Fatalf("Browser child configure: %v", err)
	}
	verified, hit := false, false
	for until := time.Now().Add(15 * time.Second); time.Now().Before(until); {
		d.drain(ctx)
		d.mu.Lock()
		for _, data := range d.events {
			var packet dapPacket
			if json.Unmarshal(data, &packet) != nil {
				continue
			}
			if packet.Event == "breakpoint" && strings.Contains(string(packet.Body), "\"verified\":true") {
				verified = true
			}
			if packet.Event == "stopped" && strings.Contains(string(packet.Body), "\"reason\":\"breakpoint\"") {
				hit = true
			}
		}
		stage := d.stage
		d.mu.Unlock()
		if verified && hit {
			break
		}
		if stage == "failed" || stage == "terminated" || stage == "uncertain" {
			t.Fatalf("real Browser debug session failed: %s", stage)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !verified || !hit {
		t.Fatalf("real Browser breakpoint missing verification/stop: verified=%v hit=%v", verified, hit)
	}
	threads, err := d.call(ctx, "threads", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var threadBody map[string]any
	if err := json.Unmarshal(threads, &threadBody); err != nil {
		t.Fatal(err)
	}
	list, ok := threadBody["threads"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("real Browser thread missing: %s", threads)
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		t.Fatal("malformed Browser thread")
	}
	threadNumber, ok := first["id"].(float64)
	if !ok {
		t.Fatalf("missing Browser thread ID: %v", first)
	}
	stack, err := d.call(ctx, "stackTrace", map[string]any{"threadId": int(threadNumber), "startFrame": 0, "levels": 10})
	if err != nil || !strings.Contains(string(stack), "browser_fixture.js") {
		t.Fatalf("real JS stack/source mapping missing: %v %s", err, stack)
	}
	var stackBody map[string]any
	if err := json.Unmarshal(stack, &stackBody); err != nil {
		t.Fatal(err)
	}
	frames, ok := stackBody["stackFrames"].([]any)
	if !ok || len(frames) == 0 {
		t.Fatalf("real Browser frames missing: %s", stack)
	}
	frame, ok := frames[0].(map[string]any)
	if !ok {
		t.Fatal("malformed Browser frame")
	}
	frameID, ok := frame["id"].(float64)
	if !ok {
		t.Fatal("missing Browser frame ID")
	}
	scopes, err := d.call(ctx, "scopes", map[string]any{"frameId": int(frameID)})
	if err != nil || !strings.Contains(string(scopes), "variablesReference") {
		t.Fatalf("real Browser scopes missing: %v %s", err, scopes)
	}
	if _, err := d.call(ctx, "next", map[string]any{"threadId": int(threadNumber)}); err != nil {
		t.Fatalf("real Browser step failed: %v", err)
	}
	if _, err := d.call(ctx, "disconnect", map[string]any{"terminateDebuggee": true}); err != nil {
		t.Fatalf("real Browser explicit disconnect failed: %v", err)
	}
	if err := s.workerStdioClose(owner, id); err != nil {
		t.Fatalf("original browser adapter broker stop failed: %v", err)
	}
	closed = true
	// The isolated browser uses an independently supervised child process.
	// The public Agent uses cgroup.kill; this unprivileged fixture can only
	// wait for the exact test-owned profile process to exit after disconnect.
	cleaned := false
	for until := time.Now().Add(7 * time.Second); time.Now().Before(until); {
		count, err := browserDAPFixtureProfileUsers(profile)
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			if err := os.RemoveAll(profile); err == nil {
				cleaned = true
				break
			}
		}
		time.Sleep(70 * time.Millisecond)
	}
	if !cleaned {
		t.Fatal("disposable Browser profile/descendant cleanup not verified")
	}
	t.Log("real Chromium pipe, original js-debug authenticated broker, child token, verified source breakpoint, JS stack/scopes/step/detach and disposable profile cleanup; no root-cgroup/installed MCP claim")
}

func browserDAPFixtureProfileUsers(profile string) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue // unrelated or vanished processes are not test-owned
		}
		if bytes.Contains(cmdline, []byte("chrome")) && bytes.Contains(cmdline, []byte(profile)) {
			count++
		}
	}
	return count, nil
}
