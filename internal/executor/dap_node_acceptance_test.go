package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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
	proveRealMicrosoftNodeDAP(t, false)
}

func TestRealMicrosoftTypeScriptSourceMapDAP(t *testing.T) {
	proveRealMicrosoftNodeDAP(t, true)
}

func proveRealMicrosoftNodeDAP(t *testing.T, typescript bool) {
	t.Helper()
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
	breakpointSource := program
	if typescript {
		tsc := os.Getenv("AGENT_TS_DAP_PROOF_TSC")
		if tsc == "" {
			t.Skip("requires immutable TypeScript compiler fixture for real source-map acceptance")
		}
		breakpointSource = filepath.Join(workspace, "main.ts")
		source = "function doubled(n: number): number {\n const result: number = n*2;\n return result;\n}\nconst answer: number=doubled(21);\nconsole.log(answer);\nsetTimeout(()=>{},12000);\n"
		if err := os.WriteFile(breakpointSource, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		// Compiler is a pinned out-of-workspace fixture. No npm scripts,
		// ts-node, project configuration or install hooks execute here.
		compiler := exec.Command(node, tsc, "--target", "ES2022", "--module", "commonjs", "--sourceMap", "--outDir", workspace, breakpointSource)
		compiler.Dir = workspace
		if output, err := compiler.CombinedOutput(); err != nil {
			t.Fatalf("real pinned tsc fixture failed: %v %s", err, output)
		}
		if info, err := os.Lstat(program + ".map"); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("compiled TypeScript must emit regular Node .js.map: %v", err)
		}
		// Check the ACTUAL TypeScript compiler output using precisely the
		// worker-only source-map graph validation used by production launch.
		s.workspaceHelperBinary = buildNodeProofWorkspaceHelper(t)
		version := s.workerWorkspaceFile(context.Background(), Request{
			Action: "workspace_stat", Workspace: workspace, Path: "main.js",
		})
		if !version.OK || version.FileVersion == "" {
			t.Fatalf("compiled executable version not available under worker identity: %+v", version)
		}
		graph, err := s.inspectPrecompiledTypeScriptMap(context.Background(), owner, "main.js", version.FileVersion)
		if err != nil || graph["main.ts"] == "" || graph["main.js.map"] == "" ||
			graph["main.js"] != version.FileVersion {
			t.Fatalf("real TypeScript source-map graph not trustworthy: %+v %v", graph, err)
		}
	} else if err := os.WriteFile(program, []byte(source), 0600); err != nil {
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
		"source":         map[string]string{"path": breakpointSource},
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
	if err != nil || !strings.Contains(string(stack), filepath.Base(breakpointSource)) {
		t.Fatalf("Node target stack/source identity not proven for selected %s: %v %s", breakpointSource, err, stack)
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
	// A provisional setBreakpoints response is NOT breakpoint proof: the
	// adapter may accept a source line without ever binding it. Continue
	// from stopOnEntry and require a separate genuine "breakpoint" stop
	// with a stack mapped back to the original JS/TS source.
	if _, err := d.call(ctx, "continue", map[string]any{"threadId": threadID}); err != nil {
		t.Fatalf("Node continue to source breakpoint failed: %v", err)
	}
	hit := false
	for until := time.Now().Add(6 * time.Second); time.Now().Before(until); {
		d.drain(ctx)
		d.mu.Lock()
		for _, event := range d.events {
			var packet dapPacket
			if json.Unmarshal(event, &packet) == nil && packet.Event == "stopped" &&
				strings.Contains(string(packet.Body), `"reason":"breakpoint"`) {
				hit = true
				break
			}
		}
		stage := d.stage
		d.mu.Unlock()
		if hit {
			break
		}
		if stage == "failed" || stage == "uncertain" || stage == "terminated" {
			t.Fatalf("Node did not stop at real source breakpoint; stage=%s", stage)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !hit {
		t.Fatalf("Node never hit requested real %s source breakpoint", breakpointSource)
	}
	mappedStack, err := d.call(ctx, "stackTrace", map[string]any{
		"threadId": threadID, "startFrame": 0, "levels": 10,
	})
	if err != nil || !strings.Contains(string(mappedStack), filepath.Base(breakpointSource)) {
		t.Fatalf("Node breakpoint stack not mapped back to original %s: %v %s", breakpointSource, err, mappedStack)
	}
	step, err := d.call(ctx, "next", map[string]any{"threadId": threadID})
	if err != nil {
		t.Fatalf("Node real source stepping failed: %v %s", err, step)
	}
	stop, err := d.call(ctx, "disconnect", map[string]any{"terminateDebuggee": true})
	if err != nil {
		t.Fatalf("Node disconnect failed: %v %s", err, stop)
	}
	t.Logf("real js-debug Node with typescript_source_maps=%t: original broker two peer-pinned sockets, stopped, source breakpoint, actual mapped stack/scopes, step, stop PASS", typescript)
}
