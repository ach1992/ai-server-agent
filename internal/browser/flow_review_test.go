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
		Refs            []struct {
			Ref       string `json:"ref"`
			Role      string `json:"role"`
			RoleIndex int    `json:"role_index"`
		} `json:"refs"`
		RefsDropped     int    `json:"refs_dropped"`
		RefsScope       string `json:"refs_scope"`
		RefsReason      string `json:"refs_reason"`
		RefsUnavailable bool   `json:"refs_unavailable"`
	} `json:"results"`
}

func reviewRun(t *testing.T, engine string, steps []FlowStep) (reviewFlowResult, string, error) {
	return reviewRunWithPrelude(t, engine, steps, "")
}

// The prelude belongs only to this isolated pinned-browser test fixture. It
// injects precisely timed page/locator races without adding production hooks.
func reviewRunWithPrelude(t *testing.T, engine string, steps []FlowStep, prelude string) (reviewFlowResult, string, error) {
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
%s
try {
%s
} finally { await context.close(); }
`, "file://"+filepath.Join(engine, "node_modules/playwright/index.mjs"), filepath.Join(root, "profile"), chrome, prelude, script)
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
		case "/property-label-large", "/property-desc-large",
			"/property-owns-large", "/property-details-large",
			"/property-error-large", "/property-controls-large",
			"/property-flowto-large", "/property-active-large",
			"/property-label-small", "/property-desc-small", "/property-owns-small":
			key := strings.TrimPrefix(r.URL.Path, "/property-")
			parts := strings.Split(key, "-")
			properties := map[string]struct {
				property, attr string
				single         bool
			}{
				"label":    {"ariaLabelledByElements", "aria-labelledby", false},
				"desc":     {"ariaDescribedByElements", "aria-describedby", false},
				"owns":     {"ariaOwnsElements", "aria-owns", false},
				"details":  {"ariaDetailsElements", "aria-details", false},
				"error":    {"ariaErrorMessageElements", "aria-errormessage", false},
				"controls": {"ariaControlsElements", "aria-controls", false},
				"flowto":   {"ariaFlowToElements", "aria-flowto", false},
				"active":   {"ariaActiveDescendantElement", "aria-activedescendant", true},
			}
			meta := properties[parts[0]]
			text := "A short external description"
			if parts[1] == "large" {
				text = strings.Repeat("X", 2<<20)
			}
			fmt.Fprintf(w, `<span data-external>%s</span><button id="tiny">Local</button>
<output id="proof"></output><script>
const target = document.getElementById('tiny');
const external = document.querySelector('[data-external]');
const property = %q, attr = %q, singular = %t;
const supported = property in target;
if (supported) target[property] = singular ? external : [external];
const reflect = supported ? target[property] : null;
const valid = supported && (singular ? reflect === external :
  Array.isArray(reflect) && reflect.length === 1 && reflect[0] === external);
const attribute = target.getAttribute(attr);
document.getElementById('proof').textContent =
  (supported ? 'supported' : 'unsupported') + ':' +
  (valid ? 'verified' : 'unverified') + ':' +
  (attribute == null || attribute === '' ? 'attribute_empty' : 'attribute_set') +
  ':' + (external.id === '' ? 'idless' : 'has_id');
</script>`, text, meta.property, meta.attr, meta.single)
		case "/property-aggregate":
			fmt.Fprint(w, `<button id="tiny">Local</button><output id="proof"></output>`)
			for i := 0; i < 85; i++ {
				fmt.Fprint(w, `<span data-external>`, strings.Repeat("Q", 1000), `</span>`)
			}
			fmt.Fprint(w, `<script>
const target = document.getElementById('tiny');
const refs = Array.from(document.querySelectorAll('[data-external]'));
const supported = 'ariaLabelledByElements' in target;
if (supported) target.ariaLabelledByElements = refs;
document.getElementById('proof').textContent =
  (supported ? 'supported' : 'unsupported') + ':' +
  (supported && target.ariaLabelledByElements.length === 85 ? 'verified' : 'unverified') +
  ':' + (target.getAttribute('aria-labelledby') === '' ? 'attribute_empty' : 'attribute_set') +
  ':idless';
</script>`)
		case "/property-cycle":
			fmt.Fprint(w, `<span data-external>External</span><button id="tiny">Local</button>
<output id="proof"></output><script>
const target = document.getElementById('tiny'), external = document.querySelector('[data-external]');
const supported = 'ariaLabelledByElements' in target;
if (supported) {
  target.ariaLabelledByElements = [external];
  external.ariaLabelledByElements = [target];
}
document.getElementById('proof').textContent =
  (supported ? 'supported' : 'unsupported') + ':verified:attribute_empty:idless';
</script>`)
		case "/property-owns-synthetic":
			fmt.Fprint(w, `<span data-external>`, strings.Repeat("U", 2<<20),
				`</span><button id="tiny">Local</button><output id="proof"></output><script>
const target = document.getElementById('tiny'), external = document.querySelector('[data-external]');
// Current pinned Chromium does not natively expose ariaOwnsElements.
// Assigning the same Element[] shape exercises the future platform path.
target.ariaOwnsElements = [external];
document.getElementById('proof').textContent =
  (target.ariaOwnsElements[0] === external ? 'supported:verified' : 'invalid') +
  ':' + (target.getAttribute('aria-owns') == null ? 'attribute_empty' : 'attribute_set') +
  ':idless';
</script>`)
		case "/property-accessor-throws":
			fmt.Fprint(w, `<button id="tiny">Local</button><script>
Object.defineProperty(document.getElementById('tiny'), 'ariaLabelledByElements', {
  get() { throw new Error('REFLECTED_SECRET_SENTINEL'); }
});
</script>`)
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
	// Chromium exposes reflected Element references even for ID-less nodes
	// and may clear the aria-* content attribute when assigning the property.
	// They must be discovered before ariaSnapshot, not by getElementById.
	for _, tc := range []struct {
		name, path string
		large      bool
		optional   bool
	}{
		{"reflected_ariaLabelledByElements_large_idless", "/property-label-large", true, false},
		{"reflected_ariaDescribedByElements_large_idless", "/property-desc-large", true, false},
		{"reflected_ariaOwnsElements_large_if_supported", "/property-owns-large", true, true},
		{"reflected_ariaDetailsElements_large_idless", "/property-details-large", true, true},
		{"reflected_ariaErrorMessageElements_large_idless", "/property-error-large", true, true},
		{"reflected_ariaControlsElements_large_idless", "/property-controls-large", true, true},
		{"reflected_ariaFlowToElements_large_idless", "/property-flowto-large", true, true},
		{"reflected_ariaActiveDescendantElement_large_idless", "/property-active-large", true, true},
		{"small_reflected_ariaLabelledByElements", "/property-label-small", false, false},
		{"small_reflected_ariaDescribedByElements", "/property-desc-small", false, false},
		{"small_reflected_ariaOwnsElements_if_supported", "/property-owns-small", false, true},
		{"many_reflected_idless_nodes_share_one_budget", "/property-aggregate", true, false},
		{"cyclic_reflected_ariaLabelledByElements", "/property-cycle", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, output, e := reviewRun(t, engine, []FlowStep{
				{Action: "goto", URL: srv.URL + tc.path},
				{Action: "snapshot", Selector: "#proof"},
				{Action: "snapshot", Selector: "#tiny"},
			})
			if e != nil || !got.OK || got.Executed != 3 || len(got.Results) != 3 {
				t.Fatalf("property-reflected flow did not finish: %+v err=%v", got, e)
			}
			proof := got.Results[1].Snapshot
			if strings.Contains(proof, "unsupported") && tc.optional {
				t.Skip("Pinned Chromium does not expose this property; existing attribute fallback is tested")
			}
			if !strings.Contains(proof, "supported:verified:attribute_empty:idless") {
				t.Fatalf("property/ID-less/attribute-clearing precondition not proven: %q", proof)
			}
			last := got.Results[2]
			if tc.large {
				if !last.Truncated || last.Reason != "dom_too_large" || last.Snapshot != "" ||
					(last.PreflightReason != "text" && last.PreflightReason != "attribute") {
					t.Fatalf("large property-assigned remote accessible content was not blocked: %+v", last)
				}
			} else if last.Truncated || last.Reason != "" || !strings.Contains(last.Snapshot, "button") {
				t.Fatalf("small reflected external reference could not be snapshotted: %+v", last)
			}
			if len(output) >= 16384 {
				t.Fatalf("property-reflected remote content exceeded output budget: %d", len(output))
			}
		})
	}
	t.Run("synthetic_future_ariaOwnsElements_is_already_bounded", func(t *testing.T) {
		got, output, e := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/property-owns-synthetic"},
			{Action: "snapshot", Selector: "#proof"},
			{Action: "snapshot", Selector: "#tiny"},
		})
		if e != nil || !got.OK || got.Executed != 3 ||
			!strings.Contains(got.Results[1].Snapshot, "supported:verified:attribute_empty:idless") ||
			got.Results[2].Reason != "dom_too_large" || got.Results[2].PreflightReason != "text" ||
			got.Results[2].Snapshot != "" || len(output) >= 16384 {
			t.Fatalf("future ariaOwnsElements shape bypassed shared preflight: %+v err=%v", got, e)
		}
	})
	t.Run("reflected_property_getter_failure_fails_closed_without_leaking", func(t *testing.T) {
		got, output, e := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/property-accessor-throws"},
			{Action: "snapshot", Selector: "#tiny"},
		})
		if e != nil || !got.OK || got.Executed != 2 ||
			got.Results[1].Reason != "dom_too_large" ||
			got.Results[1].PreflightReason != "unresolvable_references" ||
			got.Results[1].Snapshot != "" ||
			strings.Contains(output, "REFLECTED_SECRET_SENTINEL") {
			t.Fatalf("uninspectable external getter was not rejected safely: %+v err=%v", got, e)
		}
	})
	t.Log("PASS: reflected ID-less DOM references, transitive/cyclic and aggregate budgets, older attribute fallback")
	t.Log("PASS: Chromium delays/privacy/ARIA budgets, external labels/IDREFs/ownership, cyclic refs")
}

// Instrument only the test runner's Playwright Locator methods. This makes
// races deterministic exactly after ariaSnapshot or within ref enumeration,
// rather than depending on timers and flaky SPA scheduling.
func refRacePrelude(method, injectedJS string) string {
	return fmt.Sprintf(`const __asaProto = Object.getPrototypeOf(page.locator('body'));
const __asaOriginal = __asaProto.%s;
let __asaRaceInjected = false;
__asaProto.%s = async function (...args) {
  const value = await __asaOriginal.apply(this, args);
  if (!__asaRaceInjected) {
    __asaRaceInjected = true;
    %s
  }
  return value;
};
`, method, method, injectedJS)
}

// Pinned real Chromium proof: refs refer to exact ElementHandles only within
// one flow. Detached elements, navigation and invalid refs cannot silently
// retarget a different node (or return a sensitive page URL in errors).
func TestBrowserFlowRefLifecyclePinnedRuntime(t *testing.T) {
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
		case "/many":
			fmt.Fprint(w, "<body>")
			for i := 0; i < 90; i++ {
				fmt.Fprintf(w, "<button>Action %d</button>", i)
			}
			fmt.Fprint(w, "</body>")
		case "/other":
			fmt.Fprint(w, "<body><button>Other page</button></body>")
		case "/target":
			fmt.Fprint(w, `<body><div id="scope"><button id="target">Original target</button></div></body>`)
		case "/privacy":
			fmt.Fprint(w, `<body><button aria-hidden="true">SECRET_ARIA_NODE</button>
<div aria-hidden="true"><button>SECRET_ARIA_ANCESTOR</button></div>
<button title="SECRET_TITLE_METADATA">Public label</button>
<label for="field">Field label</label>
<input id="field" aria-label="Field label" title="SECRET_INPUT_TITLE" placeholder="SECRET_PLACEHOLDER">
</body>`)
		case "/inert":
			fmt.Fprint(w, `<body><button>Public</button>
<div inert><button>INERT_SENTINEL</button></div></body>`)
		case "/reload":
			fmt.Fprint(w, `<body><button onclick="location.reload()">Reload same URL</button></body>`)
		case "/custom-tag":
			fmt.Fprint(w, `<body><script>
const name = 'x-' + 'z'.repeat(20000);
const node = document.createElement(name);
node.setAttribute('role', 'button');
node.textContent = 'Custom element';
document.body.append(node);
</script></body>`)
		case "/reorder":
			fmt.Fprint(w, `<body><button id="prepend">Prepend</button>
<button id="target">Target</button><div id="result">Waiting</div>
<script>
 document.getElementById('prepend').onclick = () => {
   const button = document.createElement('button');
   button.textContent = 'Inserted';
   document.getElementById('target').before(button);
 };
 document.getElementById('target').onclick = () =>
   document.getElementById('result').textContent = 'Target hit';
</script></body>`)
		default:
			fmt.Fprint(w, `<body>
<button style="display:none" aria-label="invisible-private-label">Hidden</button>
<label for="name">Name</label><input id="name"><button id="save"><span style="display:none">private-descendant-text</span>Save</button>
<div id="result">Waiting</div>
<script>document.getElementById('save').onclick = function () {
 document.getElementById('result').textContent = document.getElementById('name').value;
 this.remove();
}</script></body>`)
		}
	}))
	defer srv.Close()

	t.Run("valid-refs", func(t *testing.T) {
		result, out, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/?key=do-not-leak"},
			{Action: "snapshot"},
			{Action: "fill", Ref: "e1", Value: "Ref accepted"},
			{Action: "click", Ref: "e2"},
			{Action: "assert_text", Selector: "#result", Expected: "Ref accepted"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 5 {
			t.Fatalf("ref flow failed: err=%v result=%+v", runErr, result)
		}
		refs := result.Results[1].Refs
		if len(refs) != 2 || refs[0].Ref != "e1" || refs[0].Role != "textbox" ||
			refs[1].Ref != "e2" || refs[1].Role != "button" ||
			refs[1].RoleIndex != 0 || result.Results[1].RefsScope != "flow_only" ||
			strings.Contains(out, "invisible-private-label") ||
			strings.Contains(out, "private-descendant-text") {
			t.Fatalf("missing/ambiguous refs: %+v", result.Results[1])
		}
	})
	t.Run("dom-reorder-does-not-retarget", func(t *testing.T) {
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/reorder"},
			{Action: "snapshot"},
			{Action: "click", Ref: "e1"},
			{Action: "click", Ref: "e2"},
			{Action: "assert_text", Selector: "#result", Expected: "Target hit"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 5 {
			t.Fatalf("DOM reordering retargeted a ref: err=%v result=%+v", runErr, result)
		}
	})
	t.Run("detached", func(t *testing.T) {
		result, out, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/?key=do-not-leak"},
			{Action: "snapshot"},
			{Action: "click", Ref: "e2"},
			{Action: "click", Ref: "e2"},
		})
		if runErr == nil || result.OK || result.FailedStep == nil ||
			*result.FailedStep != 3 || result.Results[3].Error != "stale_ref" ||
			strings.Contains(out, "do-not-leak") {
			t.Fatalf("detached ref did not fail safely: err=%v result=%+v output=%s", runErr, result, out)
		}
	})
	t.Run("new-call-cannot-reuse-prior-ref", func(t *testing.T) {
		// A new browser_e2e call has no ref registry even if the managed
		// Browser profile has persistent cookies and other authorized state.
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "click", Ref: "e1"},
		})
		if runErr == nil || result.OK || len(result.Results) != 1 ||
			result.Results[0].Error != "invalid_ref" {
			t.Fatalf("ref unexpectedly survived call boundary: err=%v result=%+v", runErr, result)
		}
	})
	t.Run("same-url-reload-after-issued-ref", func(t *testing.T) {
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/reload"},
			{Action: "snapshot"},
			{Action: "click", Ref: "e1"},
			{Action: "click", Ref: "e1"},
		})
		if runErr == nil || result.OK || len(result.Results) != 4 ||
			result.Results[3].Error != "stale_ref" {
			t.Fatalf("ref survived a new document at the same URL: err=%v result=%+v", runErr, result)
		}
	})
	t.Run("navigation", func(t *testing.T) {
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL},
			{Action: "snapshot"},
			{Action: "goto", URL: srv.URL + "/other"},
			{Action: "click", Ref: "e1"},
		})
		if runErr == nil || result.OK || len(result.Results) != 4 ||
			result.Results[3].Error != "invalid_ref" {
			t.Fatalf("navigation did not invalidate refs: err=%v result=%+v", runErr, result)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		result, out, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/many"},
			{Action: "snapshot"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 2 ||
			len(result.Results[1].Refs) > 24 || result.Results[1].RefsDropped < 66 ||
			len(out) >= 32768 {
			t.Fatalf("refs exceeded budget or count: err=%v result=%+v output_len=%d", runErr, result, len(out))
		}
	})
}

// Each race is triggered from a patched test-runner Playwright boundary,
// strictly after snapshot text capture or during candidate enumeration.
func TestBrowserFlowSnapshotIssuanceRacePinnedRuntime(t *testing.T) {
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
		switch r.URL.Path {
		case "/other":
			fmt.Fprint(w, `<body><button>Other document</button></body>`)
		default:
			fmt.Fprint(w, `<body><div id="scope"><button id="target">Original target</button></div></body>`)
		}
	}))
	defer srv.Close()

	cases := []struct {
		name, method, mutation string
		steps                  []FlowStep
	}{
		{
			name:     "same-url-document-reload-after-ariaSnapshot",
			method:   "ariaSnapshot",
			mutation: `await page.reload({waitUntil:'domcontentloaded'});`,
			steps:    []FlowStep{{Action: "goto", URL: srv.URL}, {Action: "snapshot"}},
		},
		{
			name:   "same-selector-root-replacement",
			method: "ariaSnapshot",
			mutation: `await page.evaluate(() => {
  document.querySelector('#scope').outerHTML =
    '<div id="scope"><button>REPLACEMENT_PRIVATE_NODE</button></div>';
});`,
			steps: []FlowStep{{Action: "goto", URL: srv.URL}, {Action: "snapshot", Selector: "#scope"}},
		},
		{
			name:   "async-dom-reordering-during-issuance",
			method: "count",
			mutation: `await page.evaluate(() => {
 const target=document.querySelector('#target');
 target.before(document.createElement('button'));
 target.previousElementSibling.textContent = 'Added before original';
});`,
			steps: []FlowStep{{Action: "goto", URL: srv.URL}, {Action: "snapshot"}},
		},
		{
			name:   "same-selector-element-replacement-during-issuance",
			method: "count",
			mutation: `await page.evaluate(() => {
  document.querySelector('#target').outerHTML =
    '<button id="target">UNSEEN_REPLACEMENT_CONTROL</button>';
});`,
			steps: []FlowStep{{Action: "goto", URL: srv.URL}, {Action: "snapshot"}},
		},
		{
			name:     "cross-url-navigation-during-issuance",
			method:   "count",
			mutation: fmt.Sprintf("await page.goto(%q);", srv.URL+"/other"),
			steps:    []FlowStep{{Action: "goto", URL: srv.URL}, {Action: "snapshot"}},
		},
		{
			name:   "dom-growth-during-issuance",
			method: "count",
			mutation: `await page.evaluate(() => {
 const fragment=document.createDocumentFragment();
 for (let i=0;i<5000;i++) {
  const button=document.createElement('button');
  button.textContent='Generated ' + i;
  fragment.append(button);
 }
 document.body.append(fragment);
});`,
			steps: []FlowStep{{Action: "goto", URL: srv.URL}, {Action: "snapshot"}},
		},
		{
			name:   "accessibility-change-without-dom-mutation",
			method: "count",
			mutation: `await page.evaluate(() => {
 const sheet=new CSSStyleSheet();
 sheet.replaceSync('#target { display: none !important }');
 document.adoptedStyleSheets=[...document.adoptedStyleSheets,sheet];
});`,
			steps: []FlowStep{{Action: "goto", URL: srv.URL}, {Action: "snapshot"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, output, runErr := reviewRunWithPrelude(t, engine, tc.steps,
				refRacePrelude(tc.method, tc.mutation))
			if runErr != nil || !result.OK || len(result.Results) != len(tc.steps) {
				t.Fatalf("the snapshot should remain useful without refs: err=%v result=%+v", runErr, result)
			}
			last := result.Results[len(result.Results)-1]
			if len(last.Refs) != 0 || !last.RefsUnavailable || last.RefsReason != "snapshot_changed" ||
				!strings.Contains(last.Snapshot, "Original target") ||
				strings.Contains(output, "REPLACEMENT_PRIVATE_NODE") ||
				len(output) >= 32768 {
				t.Fatalf("unstable generation issued wrong refs: %+v; len=%d", last, len(output))
			}
		})
	}
}

func TestBrowserFlowAccessibilityPrivacyPinnedRuntime(t *testing.T) {
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
		switch r.URL.Path {
		case "/inert":
			fmt.Fprint(w, `<body><button>Public</button><div inert><button>INERT_PRIVATE</button></div></body>`)
		case "/aria-owns-order":
			fmt.Fprint(w, `<body><div aria-owns="second first"></div>
<button id="first">First</button><button id="second">Second</button></body>`)
		case "/duplicate":
			fmt.Fprint(w, `<body><button>Duplicate</button><button>Duplicate</button></body>`)
		case "/shadow":
			fmt.Fprint(w, `<body><div id="host"></div>
<script>document.querySelector('#host').attachShadow({mode:'open'}).innerHTML =
'<button id="shadow-control">Shadow control</button>';</script></body>`)
		case "/custom-tag":
			fmt.Fprint(w, `<body><script>
const tag = 'x-' + 'q'.repeat(20000);
const el=document.createElement(tag);
el.setAttribute('role','button'); el.textContent='Custom label';
document.body.append(el);
</script></body>`)
		default:
			fmt.Fprint(w, `<body>
<button aria-hidden="true">SECRET_ARIA_NODE</button>
<div aria-hidden="true"><button>SECRET_ARIA_ANCESTOR</button></div>
<button title="SECRET_TITLE_METADATA">Public label</button>
<label for="field">Field label</label>
<input id="field" aria-label="Field label" title="SECRET_INPUT_TITLE" placeholder="SECRET_PLACEHOLDER">
</body>`)
		}
	}))
	defer srv.Close()
	t.Run("aria-hidden-and-metadata", func(t *testing.T) {
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL}, {Action: "snapshot"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 2 {
			t.Fatalf("privacy snapshot failed: %+v %v", result, runErr)
		}
		step := result.Results[1]
		if step.RefsUnavailable || len(step.Refs) != 2 ||
			strings.Contains(step.Snapshot, "SECRET_ARIA_NODE") ||
			strings.Contains(step.Snapshot, "SECRET_ARIA_ANCESTOR") {
			t.Fatalf("a11y-hidden controls were included or refs unavailable: %+v", step)
		}
		refsJSON, _ := json.Marshal(step.Refs)
		for _, sentinel := range []string{
			"SECRET_ARIA_NODE", "SECRET_ARIA_ANCESTOR",
			"SECRET_TITLE_METADATA", "SECRET_INPUT_TITLE", "SECRET_PLACEHOLDER",
			"tag", "name", "placeholder",
		} {
			if strings.Contains(string(refsJSON), sentinel) {
				t.Fatalf("non-snapshot metadata leaked into refs: %s", refsJSON)
			}
		}
	})
	t.Run("aria-owns-accessibility-order-fails-closed", func(t *testing.T) {
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/aria-owns-order"}, {Action: "snapshot"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 2 ||
			!result.Results[1].RefsUnavailable || len(result.Results[1].Refs) != 0 ||
			!strings.Contains(result.Results[1].Snapshot, "Second") {
			t.Fatalf("DOM-order refs misrepresented accessibility order: %+v %v", result, runErr)
		}
	})
	t.Run("duplicate-roles-fail-closed", func(t *testing.T) {
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/duplicate"}, {Action: "snapshot"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 2 ||
			!result.Results[1].RefsUnavailable || len(result.Results[1].Refs) != 0 {
			t.Fatalf("ambiguous duplicate role/name handles became actionable: %+v %v", result, runErr)
		}
	})
	t.Run("inert-subtree-fails-closed", func(t *testing.T) {
		result, _, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/inert"}, {Action: "snapshot"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 2 ||
			!result.Results[1].RefsUnavailable || len(result.Results[1].Refs) != 0 {
			t.Fatalf("inert element unexpectedly received an actionable ref: %+v %v", result, runErr)
		}
	})
	t.Run("shadow-descendant-fails-closed", func(t *testing.T) {
		for _, selector := range []string{"", "#shadow-control"} {
			steps := []FlowStep{
				{Action: "goto", URL: srv.URL + "/shadow"},
				{Action: "snapshot", Selector: selector},
			}
			result, _, runErr := reviewRun(t, engine, steps)
			if runErr != nil || !result.OK || len(result.Results) != 2 ||
				!result.Results[1].RefsUnavailable || len(result.Results[1].Refs) != 0 ||
				result.Results[1].RefsReason != "shadow_scope" {
				t.Fatalf("shadow scope unexpectedly issued refs: selector=%q result=%+v err=%v",
					selector, result, runErr)
			}
		}
	})
	t.Run("large-custom-tag-not-returned", func(t *testing.T) {
		result, output, runErr := reviewRun(t, engine, []FlowStep{
			{Action: "goto", URL: srv.URL + "/custom-tag"}, {Action: "snapshot"},
		})
		if runErr != nil || !result.OK || len(result.Results) != 2 ||
			len(result.Results[1].Refs) != 1 || len(output) >= 32768 ||
			strings.Contains(output, strings.Repeat("q", 100)) {
			t.Fatalf("unbounded custom tag reached model-visible output: %+v %v", result, runErr)
		}
	})
}
