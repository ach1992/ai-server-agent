package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBrowserFlowValidationAndBounds(t *testing.T) {
	valid := []FlowStep{
		{Action: "goto", URL: "https://example.test/demo"},
		{Action: "snapshot"},
		{Action: "fill", Role: "textbox", Name: "Name", Value: "X"},
		{Action: "click", Selector: "#submit"},
		{Action: "assert_text", Selector: "#result", Expected: "Done"},
		{Action: "assert_url", Expected: "https://example.test/demo"},
		{Action: "console"}, {Action: "network"},
	}
	script, err := flowScript(valid)
	if err != nil || !strings.Contains(script, "const __asaSteps = ") ||
		!strings.Contains(script, "ASA_BROWSER_E2E_RESULT") {
		t.Fatalf("valid browser flow rejected: %v", err)
	}
	for name, steps := range map[string][]FlowStep{
		"empty":             nil,
		"too-many":          make([]FlowStep, maxFlowSteps+1),
		"javascript":        {{Action: "eval", Value: "process.exit(0)"}},
		"javascript-url":    {{Action: "goto", URL: "javascript:alert(1)"}},
		"file-url":          {{Action: "goto", URL: "file:///etc/passwd"}},
		"credentials":       {{Action: "goto", URL: "https://user:password@example.test"}},
		"selector-and-role": {{Action: "click", Selector: "#a", Role: "button"}},
		"missing-target":    {{Action: "click"}},
		"missing-expected":  {{Action: "assert_text", Selector: "#a"}},
		"oversized-value":   {{Action: "fill", Selector: "#a", Value: strings.Repeat("x", maxFlowValueBytes+1)}},
		"oversized-timeout": {{Action: "click", Selector: "#a", TimeoutMS: maxFlowStepTimeoutMS + 1}},
		"ref-and-selector":  {{Action: "click", Selector: "#a", Ref: "e1"}},
		"ref-and-role":      {{Action: "fill", Role: "textbox", Ref: "e1", Value: "X"}},
		"bad-ref-zero":      {{Action: "click", Ref: "e0"}},
		"bad-ref-leading":   {{Action: "click", Ref: "e01"}},
		"bad-ref-format":    {{Action: "click", Ref: "a1"}},
		"bad-ref-suffix":    {{Action: "click", Ref: "e1x"}},
		"ref-on-snapshot":   {{Action: "snapshot", Ref: "e1"}},
		"ref-on-goto":       {{Action: "goto", URL: "https://example.test", Ref: "e1"}},
		"ref-on-events":     {{Action: "console", Ref: "e1"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := flowScript(steps); err == nil {
				t.Fatal("invalid browser flow accepted")
			}
		})
	}
}

func TestBrowserFlowEscapesActionValuesAsJSON(t *testing.T) {
	// This value must be treated as inert input, not executable JavaScript.
	value := "'); process.exit(47); //   braces } and \" quotes"
	script, err := flowScript([]FlowStep{{Action: "fill", Selector: "#input", Value: value}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(script, "const __asaSteps = ") != 1 {
		t.Fatal("unexpected script composition")
	}
	if !strings.Contains(script, "process.exit(47)") {
		t.Fatal("test payload missing")
	}
	// json.Marshal escapes unsafe quotes and separators. The full script is
	// syntax-checked and executed by the optional pinned Chromium acceptance.
	if strings.Contains(script, "Value: ") || strings.Contains(script, "eval(") {
		t.Fatal("flow introduced an unstructured evaluation path")
	}
}

func TestBrowserFlowRunFailsBeforeRuntimeOnInvalidSteps(t *testing.T) {
	m := testBrowserManager(t)
	resp, err := m.Flow(context.Background(), FlowOptions{Steps: []FlowStep{{Action: "goto", URL: "javascript:alert(1)"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.ErrorCode != "invalid_browser_flow" || resp.ErrorClass != "validation" {
		t.Fatalf("invalid input was not rejected before runtime: %+v", resp)
	}
}

// This is intentionally opt-in: no installs, root, Agent service or existing
// browser profile. It validates that the generated production flow JS works
// with the pinned Chromium while operating on a private worker-owned profile.
func TestBrowserFlowPinnedRuntimeAcceptance(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("set AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME to an installed pinned browser engine")
	}
	engine, err := filepath.Abs(engine)
	if err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(engine, "node/bin/node")
	binaryRel := "chrome-linux64/chrome"
	if runtime.GOARCH == "arm64" {
		binaryRel = "chrome-linux/chrome"
	}
	chrome := filepath.Join(engine, "browsers", "chromium-"+browserChromiumRevision, binaryRel)
	for _, file := range []string{node, chrome, filepath.Join(engine, "node_modules/playwright/index.mjs")} {
		if fi, e := os.Lstat(file); e != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("pinned browser runtime file unavailable or unsafe: %s: %v", file, e)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "Done")
			return
		}
		if req.URL.Path == "/large" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><p>", strings.Repeat("گزارش فارسی ", 4500), "</p>")
			return
		}
		if req.URL.Path == "/nodes" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><main id='app'>", strings.Repeat("<span>x</span>", 4500), "</main><aside id='small'>Safe scoped element</aside>")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>E2E fixture</title>
<label for="name">Name</label><input id="name"><button id="submit">Submit</button>
<div id="result">Waiting</div>
<script>document.getElementById('submit').onclick = async () => {
const v = document.getElementById('name').value;
const r = await fetch('/api?api_key=do-not-log');
document.getElementById('result').textContent = v + ':' + await r.text();
console.log('submitted');
};</script>`)
	}))
	defer srv.Close()

	steps := []FlowStep{
		{Action: "goto", URL: srv.URL},
		{Action: "snapshot"},
		{Action: "fill", Ref: "e1", Value: "ACh"},
		{Action: "click", Ref: "e2"},
		{Action: "assert_text", Selector: "#result", Expected: "ACh:Done"},
		{Action: "assert_url", Expected: srv.URL + "/"},
		{Action: "console"},
		{Action: "network"},
		{Action: "goto", URL: srv.URL + "/large"},
		{Action: "snapshot"},
		{Action: "goto", URL: srv.URL + "/nodes"},
		{Action: "snapshot"},
	}
	script, err := flowScript(steps)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(tmp, "runner.mjs")
	src := fmt.Sprintf(`import {chromium} from %q;
const context = await chromium.launchPersistentContext(%q, {
 headless: true, executablePath: %q,
 args: ['--disk-cache-size=67108864']
});
const page = context.pages()[0] || await context.newPage();
try {
%s
} finally { await context.close(); }
`, "file://"+filepath.Join(engine, "node_modules/playwright/index.mjs"), profile, chrome, script)
	if err := os.WriteFile(runner, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	cmd := exec.CommandContext(ctx, node, runner)
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned flow browser process: %v: %s", err, out)
	}
	const marker = "ASA_BROWSER_E2E_RESULT "
	index := strings.LastIndex(string(out), marker)
	if index < 0 {
		t.Fatalf("no browser flow result: %s", out)
	}
	var result struct {
		OK                         bool `json:"ok"`
		Executed                   int  `json:"executed"`
		OutputBudgetRemainingBytes int  `json:"output_budget_remaining_bytes"`
		Results                    []struct {
			Action     string            `json:"action"`
			OK         bool              `json:"ok"`
			Snapshot   string            `json:"snapshot"`
			TotalBytes int               `json:"total_bytes"`
			Truncated  bool              `json:"truncated"`
			Reason     string            `json:"reason"`
			Entries    []json.RawMessage `json:"entries"`
			Refs       []struct {
				Ref  string `json:"ref"`
				Role string `json:"role"`
			} `json:"refs"`
			RefsScope string `json:"refs_scope"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[index+len(marker):]))), &result); err != nil {
		t.Fatalf("parse browser flow output: %v: %s", err, out)
	}
	if !result.OK || result.Executed != len(steps) || len(result.Results) != len(steps) {
		t.Fatalf("incomplete flow result: %+v", result)
	}
	for _, step := range result.Results {
		if !step.OK {
			t.Fatalf("flow step failed: %+v", step)
		}
	}
	if !strings.Contains(result.Results[1].Snapshot, "textbox") ||
		!strings.Contains(result.Results[1].Snapshot, "Submit") {
		t.Fatalf("snapshot missing accessible element description: %+v", result.Results[1])
	}
	refs := result.Results[1].Refs
	if len(refs) != 2 || refs[0].Ref != "e1" || refs[0].Role != "textbox" ||
		refs[1].Ref != "e2" || refs[1].Role != "button" ||
		result.Results[1].RefsScope != "flow_only" {
		t.Fatalf("snapshot refs unavailable or incorrectly scoped: %+v", result.Results[1])
	}
	if len(result.Results[6].Entries) == 0 || len(result.Results[7].Entries) == 0 {
		t.Fatalf("console or network events missing from real browser: %+v", result.Results)
	}
	last := result.Results[9]
	if !last.Truncated || last.TotalBytes <= 8192 || len(last.Snapshot) > 8192 {
		t.Fatalf("large UTF-8 browser snapshot not bounded/truncation-aware: total=%d returned=%d truncated=%t", last.TotalBytes, len(last.Snapshot), last.Truncated)
	}
	if result.OutputBudgetRemainingBytes < 0 || len(out) >= 32768 {
		t.Fatalf("model-facing flow result exceeds response budget: remaining=%d wire_bytes=%d", result.OutputBudgetRemainingBytes, len(out))
	}
	if strings.Contains(string(out), "do-not-log") {
		t.Fatal("browser flow leaked a request query credential in model-facing network evidence")
	}
	if oversized := result.Results[11]; !oversized.Truncated || oversized.Reason != "dom_too_large" || oversized.Snapshot != "" {
		t.Fatalf("oversized DOM was materialized instead of safely omitted: %+v", oversized)
	}
	// A caller must be able to inspect a narrow subtree of the same oversized
	// page instead of being forced to materialize the entire page DOM.
	scoped, err := flowScript([]FlowStep{
		{Action: "goto", URL: srv.URL + "/nodes"},
		{Action: "snapshot", Selector: "#small"},
	})
	if err != nil {
		t.Fatal(err)
	}
	scopedRunner := strings.Replace(src, script, scoped, 1)
	if scopedRunner == src {
		t.Fatal("failed to substitute scoped flow")
	}
	if err := os.WriteFile(runner, []byte(scopedRunner), 0600); err != nil {
		t.Fatal(err)
	}
	scopedCmd := exec.CommandContext(ctx, node, runner)
	scopedCmd.Dir = tmp
	scopedCmd.Env = cmd.Env
	scopedOutput, err := scopedCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(scopedOutput), "Safe scoped element") {
		t.Fatalf("narrow snapshot failed on oversized DOM: %v: %s", err, scopedOutput)
	}
	t.Log("PASS: managed Chromium browser E2E, bounded UTF-8, DOM preflight and scoped snapshot")
}
