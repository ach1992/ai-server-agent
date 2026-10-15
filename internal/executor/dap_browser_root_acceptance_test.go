package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

// Opt-in root-Executor integration: real source debug_* calls with a fixture
// js-debug adapter and Chromium, atomically contained by the production #66
// broker. No installed agent/executor binary or service is modified. Fixture
// paths are source test overrides, not production resource-provenance evidence.
func TestApprovedRootBrowserDAPBrokerCgroupEndToEnd(t *testing.T) {
	const host = "CLY581741"
	actual, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 || actual != host || os.Getenv("ASA_ROOT_BROWSER_DAP_APPROVED_HOST") != host {
		t.Skip("requires explicit authorized root development-host execution")
	}
	const node = "/opt/ai-server-agent/browser/node/bin/node"
	const cli = "/srv/ai-workspace/asa140-js-debug-proof/bundle/js-debug/src/dapDebugServer.js"
	const chrome = "/opt/ai-server-agent/browser/browsers/chromium-1228/chrome-linux64/chrome"
	const helper = "/srv/ai-workspace/asa140-root-e2e-20261015/worker-helper"
	const base = "/srv/ai-workspace/asa140-root-e2e-20261015"
	for _, p := range []string{node, cli, chrome, helper} {
		fi, e := os.Lstat(p)
		if e != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("missing fixture %s: %v", p, e)
		}
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(worker.Uid)
	if err != nil || uid == 0 {
		t.Fatalf("invalid worker user: %v", err)
	}
	gid, err := strconv.Atoi(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "browser-owned-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("test-owned fixture cleanup failed: %v", err)
		}
	})
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(root, "sources")
	ws := filepath.Join(sourceRoot, "workspace")
	state := filepath.Join(root, "state")
	home := filepath.Join(state, "worker-home")
	if err := os.Mkdir(sourceRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourceRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(ws, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, uid, gid); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"browser_fixture.html": "<!doctype html><h1>Root Browser DAP</h1><script src=\"browser_fixture.js\"></script>",
		"browser_fixture.js":   "function twice(v) {\n const result = v * 2;\n return result;\n}\nsetTimeout(()=>twice(21),2400);\n",
	} {
		f := filepath.Join(ws, name)
		if err := os.WriteFile(f, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(f, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{
		cfg:       config.Config{WorkspaceDir: sourceRoot, StateDir: state},
		workerUID: uint32(uid), workerGID: uint32(gid),
		sessions: newStdioSessionBroker(), runs: newRunLimiterWith(2, 1),
		workspaceHelperBinary: helper, javascriptDAPNode: node,
		javascriptDAPCLI: cli, javascriptDAPBrowserBinary: chrome,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	version := func(name string) string {
		t.Helper()
		r := s.workerWorkspaceFile(ctx, Request{Action: "workspace_stat", Workspace: ws, Path: name})
		if !r.OK || r.FileVersion == "" {
			t.Fatalf("actual unprivileged source identity missing for %s: %+v", name, r)
		}
		return r.FileVersion
	}
	htmlVersion, jsVersion := version("browser_fixture.html"), version("browser_fixture.js")
	req := Request{
		Action: "debug_launch", PrincipalID: "root-browser-proof",
		PrincipalClass: "direct", RequestID: "root-browser-proof",
		Workspace: ws, Language: "javascript", DebugMode: "browser",
		DebugProgram: "browser_fixture.html", FileVersion: htmlVersion,
	}
	launched := s.debugAction(ctx, req)
	if !launched.OK || launched.SessionID == "" {
		t.Fatalf("real root-owned Browser launch failed: %+v", launched)
	}
	id := launched.SessionID
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			if err := s.workerStdioClose(req, id); err != nil {
				t.Errorf("root Broker recovery cleanup uncertain: %v", err)
			}
		}
	})
	e, err := s.sessions.get(req, id)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	d, pid := e.dap, e.pid
	e.mu.Unlock()
	if d == nil || d.containment == nil || !d.containment.verified ||
		!d.containment.containsPID(pid) {
		t.Fatal("actual adapter process lacks atomic original private cgroup proof")
	}
	cg, profile, socket := d.containment.path, d.browserProfileDir, d.socketDir
	if !strings.Contains(cg, "/ai-server-agent-executor.service/asa-dap-") || profile == "" || socket == "" {
		t.Fatalf("invalid test-owned cgroup/profile/socket: %q %q %q", cg, profile, socket)
	}
	descendant := false
	for until := time.Now().Add(4 * time.Second); time.Now().Before(until); {
		data, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.Fields(string(data))) >= 2 {
			descendant = true
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if !descendant {
		t.Fatal("real Chromium descendants not verified in original Broker cgroup")
	}
	t.Logf("source candidate launched root-contained adapter pid=%d with real Chromium descendants", pid)
	invoke := func(method string) Response {
		r := req
		r.Action, r.SessionID, r.DebugMethod = "debug_action", id, method
		return s.debugAction(ctx, r)
	}
	bp := req
	bp.Action, bp.SessionID, bp.DebugMethod = "debug_action", id, "breakpoints"
	bp.Path, bp.FileVersion, bp.DebugLines = "browser_fixture.js", jsVersion, []int{3}
	if r := s.debugAction(ctx, bp); !r.OK {
		t.Fatalf("versioned source breakpoint failed: %+v", r)
	}
	if r := invoke("configure"); !r.OK {
		t.Fatalf("contained Browser configuration failed: %+v", r)
	}
	hit := false
	for until := time.Now().Add(9 * time.Second); time.Now().Before(until); {
		r := req
		r.Action, r.SessionID = "debug_status", id
		out := s.debugAction(ctx, r)
		if !out.OK {
			t.Fatalf("Browser debug_status failed: %+v", out)
		}
		if strings.Contains(out.Output, "\"reason\":\"breakpoint\"") {
			hit = true
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if !hit {
		t.Fatal("real root-owned Chromium JS breakpoint was not observed")
	}
	if r := invoke("threads"); !r.OK || !strings.Contains(r.Output, "threads") {
		t.Fatalf("Chromium DAP threads missing: %+v", r)
	}
	stack := invoke("stackTrace") // Chromium DAP thread zero is legal.
	if !stack.OK || !strings.Contains(stack.Output, "browser_fixture.js") {
		t.Fatalf("real Chromium source stack missing: %+v", stack)
	}
	var stackObj map[string]any
	if err := json.Unmarshal([]byte(stack.Output), &stackObj); err != nil {
		t.Fatalf("real Chromium stack not structured: %v %s", err, stack.Output)
	}
	result, ok := stackObj["result"].(map[string]any)
	if !ok {
		t.Fatalf("DAP stack lacks normalized result: %s", stack.Output)
	}
	frames, ok := result["stackFrames"].([]any)
	if !ok || len(frames) == 0 {
		t.Fatalf("real Chromium frame missing: %s", stack.Output)
	}
	frame, ok := frames[0].(map[string]any)
	if !ok {
		t.Fatal("invalid Chromium frame")
	}
	frameID, ok := frame["id"].(float64)
	if !ok {
		t.Fatal("missing Chromium frame id")
	}
	scopesReq := req
	scopesReq.Action, scopesReq.SessionID, scopesReq.DebugMethod, scopesReq.DebugFrameID = "debug_action", id, "scopes", int(frameID)
	if scopes := s.debugAction(ctx, scopesReq); !scopes.OK || !strings.Contains(scopes.Output, "variablesReference") {
		t.Fatalf("real Chromium local scopes missing: %+v", scopes)
	}
	if r := invoke("next"); !r.OK {
		t.Fatalf("root Chromium step failed: %+v", r)
	}
	stop := req
	stop.Action, stop.SessionID = "debug_stop", id
	end := s.debugAction(ctx, stop)
	if !end.OK {
		t.Fatalf("root Broker cleanup/stop uncertain: %+v", end)
	}
	stopped = true
	for _, p := range []string{cg, profile, socket} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("private cgroup, profile or socket leaked %s: %v", p, err)
		}
	}
	if _, err := s.sessions.get(req, id); err == nil {
		t.Fatal("closed Browser session remained addressable")
	}
	t.Logf("PASS source debug_* full Browser DAP real Chromium, atomically contained cgroup + verified descendants and resource cleanup")
}
