package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real Microsoft v1.140.0 adapter protocol acceptance over private AF_UNIX
// with the ORIGINAL #66 broker process. The test is unprivileged and manually
// disconnects its synthetic Node debuggee; it does NOT simulate or replace
// the mandatory root-cgroup installed-Agent launch/cleanup acceptance.
func TestRealMicrosoftNodeDAPTwoConnectionsOneBroker(t *testing.T) {
	node := os.Getenv("AGENT_JS_DAP_PROOF_NODE")
	cli := os.Getenv("AGENT_JS_DAP_PROOF_CLI")
	if node == "" && cli == "" {
		t.Skip("requires optional official pinned Microsoft js-debug proof")
	}
	if node == "" || cli == "" {
		t.Fatal("partial JS DAP proof fixture")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	source := "function doubled(n) {\n const result=n*2;\n return result;\n}\nconst answer=doubled(21);\nconsole.log(answer);\nsetTimeout(()=>{},12000);\n"
	program := filepath.Join(workspace, "main.js")
	if err := os.WriteFile(program, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "dap.sock")
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	id, err := s.workerStdioSession(owner, "dap", workspace, node, cli, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.workerStdioClose(owner, id); err != nil {
			t.Errorf("original adapter broker cleanup: %v", err)
		}
	}()
	e, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := waitPinnedNodeDAPPeer(ctx, e, s.workerUID, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	d := &dapState{conn: parent, adapter: "javascript/js-debug", workspace: workspace, stage: "starting"}
	init := map[string]any{
		"adapterID": "pwa-node", "clientID": "ai-server-agent", "linesStartAt1": true,
		"columnsStartAt1": true, "pathFormat": "path", "supportsRunInTerminalRequest": false,
	}
	if _, err := d.call(ctx, "initialize", init); err != nil {
		t.Fatalf("parent initialize: %v", err)
	}
	_, err = d.callDeferredLaunch(ctx, map[string]any{
		"request": "launch", "type": "pwa-node", "name": "Agent isolated acceptance",
		"program": program, "runtimeExecutable": node, "cwd": workspace,
		"autoAttachChildProcesses": false, "sourceMaps": true, "stopOnEntry": true, "console": "internalConsole",
	})
	if err != nil {
		t.Fatalf("parent launch/initialized: %v", err)
	}
	if _, err := d.call(ctx, "configurationDone", map[string]any{}); err != nil {
		t.Fatalf("parent configure: %v", err)
	}
	pending, err := d.receiveTypedNodeChild(ctx)
	if err != nil {
		t.Fatalf("trusted typed Node reverse: %v", err)
	}
	child, err := waitPinnedNodeDAPPeer(ctx, e, s.workerUID, sock)
	if err != nil {
		t.Fatalf("same original adapter child socket: %v", err)
	}
	defer child.Close()
	d.mu.Lock()
	d.parentConn = d.conn
	d.conn = child
	d.pending = nil
	d.seq = 0
	d.initialized = false
	d.pendingLaunchSeq = 0
	d.mu.Unlock()
	if _, err := d.call(ctx, "initialize", init); err != nil {
		t.Fatalf("child initialize: %v", err)
	}
	if _, err := d.callDeferredLaunch(ctx, map[string]any{
		"request": "launch", "type": "pwa-node", "name": "Agent child target", "__pendingTargetId": pending,
	}); err != nil {
		t.Fatalf("child launch/initialized: %v", err)
	}
	set, err := d.call(ctx, "setBreakpoints", map[string]any{
		"source":         map[string]string{"path": program},
		"breakpoints":    []map[string]int{{"line": 3}},
		"sourceModified": false,
	})
	if err != nil || !strings.Contains(string(set), "breakpoints") {
		t.Fatalf("real Node source breakpoint setup: %v %s", err, set)
	}
	if _, err := d.call(ctx, "configurationDone", map[string]any{}); err != nil {
		t.Fatalf("child configure: %v", err)
	}
	// The DAP configuration reply arrives BEFORE the real stopped event.
	// A synthetic successful threads response does not prove a paused
	// debugger. Correlate a genuine stopped notification before stack.
	for until := time.Now().Add(3 * time.Second); ; {
		stage, _, _ := d.peek()
		if stage == "stopped" {
			break
		}
		if stage == "failed" || stage == "uncertain" || time.Now().After(until) {
			t.Fatalf("Node child never reached a proven stopped state: %s", stage)
		}
		d.drain(ctx)
		time.Sleep(20 * time.Millisecond)
	}
	threads, err := d.call(ctx, "threads", map[string]any{})
	if err != nil {
		t.Fatalf("Node stopped threads: %v", err)
	}
	var list struct {
		Threads []struct {
			ID int `json:"id"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(threads, &list); err != nil || len(list.Threads) == 0 {
		t.Fatalf("real Node target thread not found: %s %v", threads, err)
	}
	threadID := list.Threads[0].ID
	if threadID != 0 {
		t.Logf("Node adapter returned thread id %d (0 is also valid)", threadID)
	}
	stack, err := d.call(ctx, "stackTrace", map[string]any{
		"threadId": threadID, "startFrame": 0, "levels": 10,
	})
	if err != nil || !strings.Contains(string(stack), "main.js") {
		t.Fatalf("Node target stack/source identity not proven: %v %s", err, stack)
	}
	var frames struct {
		Frames []struct {
			ID int `json:"id"`
		} `json:"stackFrames"`
	}
	if err := json.Unmarshal(stack, &frames); err != nil || len(frames.Frames) == 0 {
		t.Fatalf("Node target has no real stack frames: %v %s", err, stack)
	}
	scopes, err := d.call(ctx, "scopes", map[string]any{"frameId": frames.Frames[0].ID})
	if err != nil || !strings.Contains(string(scopes), "variablesReference") {
		t.Fatalf("Node stack scopes unavailable: %v %s", err, scopes)
	}
	step, err := d.call(ctx, "next", map[string]any{"threadId": threadID})
	if err != nil {
		t.Fatalf("Node real source stepping failed: %v %s", err, step)
	}
	stop, err := d.call(ctx, "disconnect", map[string]any{"terminateDebuggee": true})
	if err != nil {
		t.Fatalf("Node disconnect failed: %v %s", err, stop)
	}
	t.Log("real pinned js-debug Node parent+child Unix sockets: peer PID/UID, typed reverse, breakpoint, stack/source, scopes, step, disconnect PASS")
}
