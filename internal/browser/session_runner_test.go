package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This is a pinned-runtime process proof, not an installed-agent acceptance.
// It proves that the EXISTING Playwright flow can retain exact ElementHandles
// between independently transmitted stdio calls in one Chromium process.
func TestManagedBrowserSessionRunnerPinnedRuntime(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("set AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME for pinned Chromium acceptance")
	}
	node := filepath.Join(engine, "node", "bin", "node")
	if _, err := os.Stat(node); err != nil {
		t.Fatalf("pinned Node: %v", err)
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><button onclick="document.querySelector('#state').textContent='clicked'">Submit</button><p id="state">Ready</p></body></html>`)
	}))
	defer web.Close()
	dir := t.TempDir()
	script, err := managedSessionRunner(engine, filepath.Join(dir, "profile"), filepath.Join(dir, "downloads"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "downloads"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "session.mjs")
	if err = os.WriteFile(file, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, file)
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	reader := bufio.NewReader(stdout)
	read := func() map[string]any {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("session frame: %v; stderr=%s", err, stderr.String())
		}
		if !strings.HasPrefix(line, "ASA_BROWSER_SESSION ") {
			t.Fatalf("unexpected runner output %q", line)
		}
		var reply map[string]any
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "ASA_BROWSER_SESSION ")), &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if first := read(); first["event"] != "ready" {
		t.Fatalf("ready=%v", first)
	}
	call := func(nonce string, steps []FlowStep) map[string]any {
		t.Helper()
		for _, s := range steps {
			if err := validateFlowStep(s); err != nil {
				t.Fatal(err)
			}
		}
		wire, _ := json.Marshal(map[string]any{"type": "flow", "nonce": nonce, "steps": steps})
		if len(wire) > 16384 {
			t.Fatalf("input exceeds runner limit %d", len(wire))
		}
		if _, err := stdin.Write(append(wire, '\n')); err != nil {
			t.Fatal(err)
		}
		got := read()
		if got["event"] != "result" || got["nonce"] != nonce {
			t.Fatalf("result=%v; stderr=%s", got, stderr.String())
		}
		return got["result"].(map[string]any)
	}
	get := func(r map[string]any) []any { return r["results"].([]any) }
	one := call(strings.Repeat("1", 32), []FlowStep{{Action: "goto", URL: web.URL}, {Action: "snapshot"}})
	if one["ok"] != true {
		t.Fatalf("first flow=%v", one)
	}
	snap := get(one)[1].(map[string]any)
	if snap["refs_unavailable"] != false {
		t.Fatalf("refs=%v", snap)
	}
	if snap["refs_scope"] != "session_until_invalidated" {
		t.Fatalf("managed refs_scope=%v, want session_until_invalidated", snap["refs_scope"])
	}
	refs := snap["refs"].([]any)
	if len(refs) == 0 {
		t.Fatalf("missing refs: %v", snap)
	}
	ref := ""
	for _, raw := range refs {
		v := raw.(map[string]any)
		if v["role"] == "button" {
			ref = v["ref"].(string)
			break
		}
	}
	if ref == "" {
		t.Fatalf("button ref missing: %v", refs)
	}
	two := call(strings.Repeat("2", 32), []FlowStep{{Action: "click", Ref: ref}, {Action: "assert_text", Selector: "#state", Expected: "clicked"}})
	if two["ok"] != true {
		t.Fatalf("cross-call click failed: %v", two)
	}
	// A NEW snapshot invalidates refs issued by the earlier call, even if
	// the same element remains in the same URL/document. A new ref identity
	// is never allowed to alias an older published model-visible ref.
	fresh := call(strings.Repeat("a", 32), []FlowStep{{Action: "snapshot"}})
	if fresh["ok"] != true {
		t.Fatalf("fresh snapshot failed: %v", fresh)
	}
	freshSnap := get(fresh)[0].(map[string]any)
	freshRefs := freshSnap["refs"].([]any)
	newRef := ""
	for _, raw := range freshRefs {
		v := raw.(map[string]any)
		if v["role"] == "button" {
			newRef = v["ref"].(string)
			break
		}
	}
	if newRef == "" || newRef == ref {
		t.Fatalf("old ref aliased after snapshot: old=%q new=%q", ref, newRef)
	}
	old := call(strings.Repeat("b", 32), []FlowStep{{Action: "click", Ref: ref}})
	if old["ok"] != false || get(old)[0].(map[string]any)["error"] != "invalid_ref" {
		t.Fatalf("old ref survived new snapshot: %v", old)
	}
	newAction := call(strings.Repeat("c", 32), []FlowStep{{Action: "click", Ref: newRef}})
	if newAction["ok"] != true {
		t.Fatalf("fresh ref stopped working: %v", newAction)
	}
	three := call(strings.Repeat("3", 32), []FlowStep{{Action: "goto", URL: web.URL}})
	if three["ok"] != true {
		t.Fatalf("reload=%v", three)
	}
	four := call(strings.Repeat("4", 32), []FlowStep{{Action: "click", Ref: ref}})
	if four["ok"] != false || get(four)[0].(map[string]any)["error"] != "invalid_ref" {
		t.Fatalf("stale ref improperly worked: %v", four)
	}
	if _, err := stdin.Write([]byte("{\"type\":\"close\"}\n")); err != nil {
		t.Fatal(err)
	}
	// EOF is part of the bounded worker protocol: a readline stream is
	// otherwise allowed to keep the Node event loop alive after close.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("browser close: %v; stderr=%s", err, stderr.String())
	}
}
