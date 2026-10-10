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
	"time"
)

type reviewFlowResult struct {
	OK         bool `json:"ok"`
	FailedStep *int `json:"failed_step"`
	Executed   int  `json:"executed"`
	Results    []struct {
		Action          string `json:"action"`
		Error           string `json:"error"`
		DurationMS      int    `json:"duration_ms"`
		Truncated       bool   `json:"truncated"`
		Reason          string `json:"reason"`
		PreflightReason string `json:"preflight_reason"`
		ScannedNodes    int    `json:"scanned_nodes"`
		Snapshot        string `json:"snapshot"`
	} `json:"results"`
}

func reviewRun(t *testing.T, engine string, steps []FlowStep) (reviewFlowResult, string, error) {
	t.Helper()
	script, err := flowScript(steps)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "profile"), 0700); err != nil {
		t.Fatal(err)
	}
	chromeDir := "chrome-linux64/chrome"
	if runtime.GOARCH == "arm64" {
		chromeDir = "chrome-linux/chrome"
	}
	chrome := filepath.Join(engine, "browsers", "chromium-"+browserChromiumRevision, chromeDir)
	node := filepath.Join(engine, "node/bin/node")
	for _, file := range []string{node, chrome, filepath.Join(engine, "node_modules/playwright/index.mjs")} {
		fi, e := os.Lstat(file)
		if e != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("missing pinned runtime %s: %v", file, e)
		}
	}
	source := fmt.Sprintf(`import { chromium } from %q;
const context = await chromium.launchPersistentContext(%q,{headless:true,executablePath:%q});
const page = context.pages()[0] || await context.newPage();
try {
%s
} finally { await context.close(); }
`, "file://"+filepath.Join(engine, "node_modules/playwright/index.mjs"), filepath.Join(root, "profile"), chrome, script)
	runner := filepath.Join(root, "runner.mjs")
	if err := os.WriteFile(runner, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, runner)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	out, runErr := cmd.CombinedOutput()
	const marker = "ASA_BROWSER_E2E_RESULT "
	index := strings.LastIndex(string(out), marker)
	if index < 0 {
		t.Fatalf("missing flow result: run_err=%v; output=%.600s", runErr, out)
	}
	var result reviewFlowResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[index+len(marker):]))), &result); err != nil {
		t.Fatalf("bad flow result: %v; output=%.600s", err, out)
	}
	return result, string(out), runErr
}

func TestBrowserFlowReviewAcceptance(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("requires pinned Playwright and Chromium")
	}
	var err error
	engine, err = filepath.Abs(engine)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/element":
			fmt.Fprint(w, `<body>Ready<script>setTimeout(()=>document.body.insertAdjacentHTML('beforeend','<p id="later">Arrived</p>'),2400)</script></body>`)
		case "/spa":
			fmt.Fprint(w, `<body>SPA ready<script>setTimeout(()=>history.pushState(null,'','/ready'),2400)</script></body>`)
		case "/aria":
			fmt.Fprint(w, `<button id="huge" aria-label="`, strings.Repeat("x", 2<<20), `">x</button>`)
		case "/value":
			fmt.Fprint(w, `<input id="value"><script>document.querySelector('#value').value='v'.repeat(2<<20)</script>`)
		case "/css":
			fmt.Fprint(w, `<style>#tiny::before{content:"`, strings.Repeat("x", 120000), `"}</style><button id="tiny">Tiny</button>`)
		case "/shadow":
			fmt.Fprint(w, `<div id="host"></div><script>
const b=document.createElement('button');
b.setAttribute('aria-label','Z'.repeat(2<<20));
document.querySelector('#host').attachShadow({mode:'open'}).append(b);
</script>`)
		case "/external-labelledby":
			fmt.Fprint(w, `<span id="outside">`, strings.Repeat("N", 2<<20),
				`</span><button id="tiny" aria-labelledby="outside">Tiny</button>`)
		case "/external-native-label":
			fmt.Fprint(w, `<label for="tiny">`, strings.Repeat("L", 2<<20),
				`</label><input id="tiny">`)
		case "/external-owns":
			fmt.Fprint(w, `<div id="owned" role="option" aria-label="`, strings.Repeat("O", 2<<20),
				`">Owned</div><div id="tiny" role="listbox" aria-owns="owned">Small</div>`)
		case "/external-chain":
			fmt.Fprint(w, `<button id="tiny" aria-labelledby="first">Tiny</button>
<span id="first" aria-labelledby="second">First</span><span id="second">`,
				strings.Repeat("T", 2<<20), `</span>`)
		case "/external-describedby":
			fmt.Fprint(w, `<p id="description">`, strings.Repeat("D", 2<<20),
				`</p><input id="tiny" aria-describedby="description">`)
		case "/aggregate-external":
			ids := make([]string, 85)
			for i := range ids {
				ids[i] = fmt.Sprintf("a%d", i)
			}
			fmt.Fprintf(w, `<button id="tiny" aria-labelledby="%s">Tiny</button>`, strings.Join(ids, " "))
			for i := range ids {
				fmt.Fprintf(w, `<span id="a%d">%s</span>`, i, strings.Repeat("q", 1000))
			}
		case "/small-labelledby":
			fmt.Fprint(w, `<span id="title">Proceed safely</span><button id="tiny" aria-labelledby="title">Tiny</button>`)
		case "/small-native-label":
			fmt.Fprint(w, `<label for="tiny">Account name</label><input id="tiny">`)
		case "/small-owns":
			fmt.Fprint(w, `<div id="owned" role="option">Item A</div><div id="tiny" role="listbox" aria-owns="owned"></div>`)
		case "/cycle":
			fmt.Fprint(w, `<span id="a" aria-labelledby="b">First</span><span id="b" aria-labelledby="a">Second</span>
<button id="tiny" aria-labelledby="a">Tiny</button>`)
		default:
			fmt.Fprint(w, `<p id="initial">Initial content</p>`)
		}
	}))
	defer srv.Close()
	t.Run("late_element", func(t *testing.T) {
		steps := []FlowStep{{Action: "goto", URL: srv.URL + "/element"}, {Action: "assert_text", Selector: "#later", Expected: "Arrived", TimeoutMS: 6000}, {Action: "assert_url", Expected: srv.URL + "/element"}}
		got, _, e := reviewRun(t, engine, steps)
		if e != nil || !got.OK || got.Executed != len(steps) || got.Results[1].DurationMS < 2000 {
			t.Fatalf("late element: %+v err=%v", got, e)
		}
	})
	t.Run("late_spa_url", func(t *testing.T) {
		steps := []FlowStep{{Action: "goto", URL: srv.URL + "/spa"}, {Action: "assert_url", Expected: srv.URL + "/ready", TimeoutMS: 6000}, {Action: "assert_text", Selector: "body", Expected: "SPA ready"}}
		got, _, e := reviewRun(t, engine, steps)
		if e != nil || !got.OK || got.Executed != len(steps) || got.Results[1].DurationMS < 2000 {
			t.Fatalf("late URL: %+v err=%v", got, e)
		}
	})
	for name, steps := range map[string][]FlowStep{
		"missing_element_at_deadline": {{Action: "goto", URL: srv.URL}, {Action: "assert_text", Selector: "#never", Expected: "Missing", TimeoutMS: 650}, {Action: "snapshot"}},
		"wrong_url_at_deadline":       {{Action: "goto", URL: srv.URL + "/spa"}, {Action: "assert_url", Expected: srv.URL + "/ready", TimeoutMS: 650}, {Action: "console"}},
	} {
		t.Run(name, func(t *testing.T) {
			got, _, e := reviewRun(t, engine, steps)
			if e == nil || got.OK || got.FailedStep == nil || *got.FailedStep != 1 || got.Executed != 2 || len(got.Results) != 2 || got.Results[1].DurationMS < 550 || got.Results[1].DurationMS > 2500 {
				t.Fatalf("assertion did not honor timeout/stop: %+v err=%v", got, e)
			}
		})
	}
	t.Run("failed_navigation_query_fragment_redacted", func(t *testing.T) {
		steps := []FlowStep{{Action: "goto", URL: "http://127.0.0.1:1/private?token=REVIEW_QUERY_SENTINEL_624#REVIEW_FRAGMENT_SENTINEL_625"}, {Action: "snapshot"}}
		got, output, e := reviewRun(t, engine, steps)
		if e == nil || got.OK || got.Executed != 1 || got.FailedStep == nil || *got.FailedStep != 0 {
			t.Fatalf("navigation failure not stopped: %+v err=%v", got, e)
		}
		if strings.Contains(output, "REVIEW_QUERY_SENTINEL_624") || strings.Contains(output, "REVIEW_FRAGMENT_SENTINEL_625") {
			t.Fatal("failed navigation leaked secret sentinel")
		}
		if !strings.HasPrefix(got.Results[0].Error, "navigation_ERR_") {
			t.Fatalf("lost safe error classification: %+v", got.Results[0])
		}
	})
	for _, tc := range []struct{ name, path, selector, reason string }{
		{"large_aria_accessible_name", "/aria", "#huge", "attribute"},
		{"large_live_form_value", "/value", "#value", "control_value"},
		{"large_generated_css", "/css", "#tiny", "generated_content"},
		{"large_shadow_accessible_name", "/shadow", "#host", "attribute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []FlowStep{{Action: "goto", URL: srv.URL + tc.path}, {Action: "snapshot", Selector: tc.selector}}
			got, output, e := reviewRun(t, engine, steps)
			if e != nil || !got.OK || got.Executed != 2 {
				t.Fatalf("preflight test failed: %+v err=%v", got, e)
			}
			last := got.Results[1]
			if !last.Truncated || last.Reason != "dom_too_large" || last.PreflightReason != tc.reason || last.Snapshot != "" || last.ScannedNodes > 16 {
				t.Fatalf("preflight did not block before snapshot materialization: %+v", last)
			}
			if len(output) > 16384 {
				t.Fatalf("model output too large: %d bytes", len(output))
			}
		})
	}
	for _, tc := range []struct{ name, path, selector, reason string }{
		{"external_aria_labelledby_large_text", "/external-labelledby", "#tiny", "text"},
		{"external_native_label_large_text", "/external-native-label", "#tiny", "text"},
		{"external_aria_owns_large_accessible_name", "/external-owns", "#tiny", "attribute"},
		{"transitive_external_aria_idref", "/external-chain", "#tiny", "text"},
		{"external_aria_describedby_large_text", "/external-describedby", "#tiny", "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, output, e := reviewRun(t, engine, []FlowStep{
				{Action: "goto", URL: srv.URL + tc.path},
				{Action: "snapshot", Selector: tc.selector},
			})
			if e != nil || !got.OK || got.Executed != 2 || len(got.Results) != 2 {
				t.Fatalf("external accessibility dependency check failed: %+v err=%v", got, e)
			}
			last := got.Results[1]
			if !last.Truncated || last.Reason != "dom_too_large" ||
				last.PreflightReason != tc.reason || last.Snapshot != "" || last.ScannedNodes > 12 {
				t.Fatalf("large external dependency was not bounded before ARIA snapshot: %+v", last)
			}
			if len(output) >= 16384 {
				t.Fatalf("external dependency leaked oversized browser output: %d bytes", len(output))
			}
		})
	}
	t.Run("aggregate_external_idrefs_use_one_shared_budget", func(t *testing.T) {
		got, output, e := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/aggregate-external"},
			{Action: "snapshot", Selector: "#tiny"},
		})
		if e != nil || !got.OK || got.Executed != 2 {
			t.Fatalf("aggregate external IDREF flow failed: %+v err=%v", got, e)
		}
		last := got.Results[1]
		if !last.Truncated || last.PreflightReason != "text" ||
			last.Reason != "dom_too_large" || last.Snapshot != "" ||
			last.ScannedNodes < 70 || last.ScannedNodes > 4000 || len(output) > 16384 {
			t.Fatalf("aggregate external IDREF budget was bypassed: %+v output_bytes=%d", last, len(output))
		}
	})
	for _, tc := range []struct{ name, path, expected string }{
		{"small_external_aria_labelledby_still_works", "/small-labelledby", "Proceed safely"},
		{"small_external_native_label_still_works", "/small-native-label", "Account name"},
		{"small_external_aria_owned_subtree_still_works", "/small-owns", "listbox"},
		{"cyclic_aria_references_do_not_loop", "/cycle", "button"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, e := reviewRun(t, engine, []FlowStep{
				{Action: "goto", URL: srv.URL + tc.path},
				{Action: "snapshot", Selector: "#tiny"},
			})
			if e != nil || !got.OK || got.Executed != 2 || len(got.Results) != 2 {
				t.Fatalf("small external reference/cycle failed: %+v err=%v", got, e)
			}
			last := got.Results[1]
			if last.Truncated || last.Reason != "" || !strings.Contains(last.Snapshot, tc.expected) {
				t.Fatalf("small external reference failed to produce accessible snapshot: %+v", last)
			}
		})
	}
	t.Log("PASS: Chromium delays/privacy/ARIA budgets, external labels/IDREFs/ownership, cyclic refs")
}
