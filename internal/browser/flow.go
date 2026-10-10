package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

const (
	maxFlowSteps         = 12
	maxFlowInputBytes    = 16 << 10
	maxFlowSelectorBytes = 512
	maxFlowURLBytes      = 2048
	maxFlowValueBytes    = 4096
	maxFlowExpectedBytes = 1024
	maxFlowStepTimeoutMS = int64(30000)
)

// FlowStep describes a single, bounded Playwright action. It intentionally
// avoids arbitrary JavaScript; browser_run remains the advanced escape hatch.
// All steps in a flow use the same managed browser page, admission and profile.
type FlowStep struct {
	Action    string `json:"action" jsonschema:"goto, snapshot, click, fill, assert_text, assert_url, console or network"`
	URL       string `json:"url,omitempty" jsonschema:"HTTP(S) URL for goto; about:blank is permitted"`
	Selector  string `json:"selector,omitempty" jsonschema:"CSS locator (instead of role/name); maximum 512 UTF-8 bytes"`
	Role      string `json:"role,omitempty" jsonschema:"Accessible role (instead of selector), such as button or textbox"`
	Name      string `json:"name,omitempty" jsonschema:"Exact accessible name when role is used"`
	Value     string `json:"value,omitempty" jsonschema:"Input value for fill, maximum 4096 UTF-8 bytes"`
	Expected  string `json:"expected,omitempty" jsonschema:"Required expected text substring or exact URL for an assertion"`
	TimeoutMS int64  `json:"timeout_ms,omitempty" jsonschema:"Per-step timeout milliseconds (default 10000; maximum 30000)"`
}

type FlowOptions struct {
	Steps             []FlowStep
	TimeoutMS         int64
	IgnoreHTTPSErrors bool
}

func validateFlowStep(s FlowStep) error {
	if s.TimeoutMS < 0 || s.TimeoutMS > maxFlowStepTimeoutMS {
		return fmt.Errorf("timeout_ms must be between 0 and %d", maxFlowStepTimeoutMS)
	}
	if len(s.URL) > maxFlowURLBytes || len(s.Selector) > maxFlowSelectorBytes ||
		len(s.Role) > 40 || len(s.Name) > 256 || len(s.Value) > maxFlowValueBytes ||
		len(s.Expected) > maxFlowExpectedBytes {
		return errors.New("step field exceeds its size limit")
	}
	hasLocator := s.Selector != "" || s.Role != ""
	if (s.Selector != "" && (s.Role != "" || s.Name != "")) || (s.Name != "" && s.Role == "") {
		return errors.New("supply either selector or role/name, never both")
	}
	switch s.Action {
	case "goto":
		if s.URL == "" || hasLocator || s.Name != "" || s.Value != "" || s.Expected != "" {
			return errors.New("goto requires only url")
		}
		if s.URL != "about:blank" {
			u, err := url.Parse(s.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
				return errors.New("goto requires an absolute http/https URL without embedded user credentials")
			}
		}
	case "snapshot", "console", "network":
		if s.URL != "" || hasLocator || s.Name != "" || s.Value != "" || s.Expected != "" {
			return errors.New("inspection actions take no element or value fields")
		}
	case "click", "fill", "assert_text":
		if !hasLocator || s.URL != "" {
			return errors.New("element actions require selector or role/name, not url")
		}
		if s.Action == "click" && (s.Value != "" || s.Expected != "") {
			return errors.New("click takes no value or expected")
		}
		if s.Action == "fill" && s.Expected != "" {
			return errors.New("fill takes no expected")
		}
		if s.Action == "assert_text" && (s.Expected == "" || s.Value != "") {
			return errors.New("assert_text requires expected text")
		}
	case "assert_url":
		if s.Expected == "" || s.URL != "" || hasLocator || s.Name != "" || s.Value != "" {
			return errors.New("assert_url requires only expected URL")
		}
	default:
		return fmt.Errorf("unsupported browser action %q", s.Action)
	}
	return nil
}

func flowScript(steps []FlowStep) (string, error) {
	if len(steps) == 0 || len(steps) > maxFlowSteps {
		return "", fmt.Errorf("steps must contain between 1 and %d actions", maxFlowSteps)
	}
	for i, step := range steps {
		if err := validateFlowStep(step); err != nil {
			return "", fmt.Errorf("steps[%d]: %w", i, err)
		}
	}
	payload, err := json.Marshal(steps)
	if err != nil {
		return "", err
	}
	if len(payload) > maxFlowInputBytes {
		return "", fmt.Errorf("serialized flow exceeds %d bytes", maxFlowInputBytes)
	}
	script := strings.Replace(browserFlowJS, "__ASA_FLOW_STEPS_JSON__", string(payload), 1)
	if len(script) > maxBrowserScriptBytes {
		return "", errors.New("generated browser flow exceeds the browser script limit")
	}
	return script, nil
}

func (m *Manager) Flow(ctx context.Context, opts FlowOptions) (executor.Response, error) {
	script, err := flowScript(opts.Steps)
	if err != nil {
		return browserError("invalid_browser_flow", "validation", err.Error()), nil
	}
	// Intentionally reuse Run: its shared profile, fail-fast admission, TLS,
	// executor resource/time bounds and disposable-data cleanup own this flow.
	return m.Run(ctx, RunOptions{
		Script: script, TimeoutMS: opts.TimeoutMS,
		IgnoreHTTPSErrors: opts.IgnoreHTTPSErrors,
	})
}

// This is intentionally a finite sequence in one managed Chromium process.
// It is not a persistent cross-call browser session and never creates a CLI
// daemon, a second browser profile, a new listener or another executor path.
// Results are compact; snapshots disclose explicit truncation. Trace ZIP /
// binary delivery belongs to #99 and is not claimed by this JSON event summary.
const browserFlowJS = `
const __asaSteps = __ASA_FLOW_STEPS_JSON__;
const __asaConsole = [];
const __asaNetwork = [];
let __asaConsoleDropped = 0, __asaNetworkDropped = 0;
// Count UTF-8 bytes, not merely JavaScript characters: a high-output page
// cannot silently push a large structured payload into a model's context.
let __asaOutputBudget = 12000;
const __asaBudgeted = (value, max = 8192) => {
  const raw = String(value ?? '');
  const limit = Math.max(0, Math.min(max, __asaOutputBudget));
  let text = raw;
  if (Buffer.byteLength(text, 'utf8') > limit) {
    text = Buffer.from(text, 'utf8').subarray(0, limit).toString('utf8');
    if (text.endsWith('\uFFFD')) text = text.slice(0, -1);
  }
  __asaOutputBudget -= Buffer.byteLength(text, 'utf8');
  return { text, truncated: Buffer.byteLength(raw, 'utf8') > Buffer.byteLength(text, 'utf8') };
};
const __asaBudgetedEntries = entries => {
  const selected = [];
  for (const event of entries) {
    const wire = JSON.stringify(event);
    if (Buffer.byteLength(wire, 'utf8') > __asaOutputBudget) break;
    selected.push(event);
    __asaOutputBudget -= Buffer.byteLength(wire, 'utf8');
  }
  return { entries: selected, omitted: entries.length - selected.length };
};
const __asaCap = (v, n = 400) => String(v ?? '').slice(0, n);
const __asaURL = (v) => {
  try {
    const u = new URL(v);
    return u.protocol === 'about:' ? 'about:blank' : u.origin + __asaCap(u.pathname, 160);
  } catch { return '[unavailable]'; }
};
const __asaPush = (items, entry, kind) => {
  if (items.length < 24) items.push(entry);
  else if (kind === 'console') __asaConsoleDropped++;
  else __asaNetworkDropped++;
};
page.on('console', message =>
  __asaPush(__asaConsole, { level: message.type(), message: __asaCap(message.text()) }, 'console'));
page.on('pageerror', error =>
  __asaPush(__asaConsole, { level: 'pageerror', message: __asaCap(error.message) }, 'console'));
page.on('requestfailed', request =>
  __asaPush(__asaNetwork, { method: request.method(), url: __asaURL(request.url()),
    failure: __asaCap(request.failure()?.errorText) }, 'network'));
page.on('response', response =>
  __asaPush(__asaNetwork, { method: response.request().method(), status: response.status(),
    url: __asaURL(response.url()) }, 'network'));

const __asaLocator = step => step.selector
  ? page.locator(step.selector)
  : page.getByRole(step.role, step.name ? { name: step.name, exact: true } : {});
const __asaResults = [];
let __asaFailedStep = null;
for (let i = 0; i < __asaSteps.length; i++) {
  const step = __asaSteps[i];
  const started = Date.now();
  const item = { index: i, action: step.action, ok: true };
  const timeout = step.timeout_ms || 10000;
  try {
    switch (step.action) {
      case 'goto':
        await page.goto(step.url, { waitUntil: 'domcontentloaded', timeout });
        item.url = __asaURL(page.url());
        break;
      case 'snapshot': {
        const content = await page.locator('body').ariaSnapshot({ timeout });
        const bounded = __asaBudgeted(content);
        item.total_bytes = Buffer.byteLength(content, 'utf8');
        item.snapshot = bounded.text;
        item.truncated = bounded.truncated;
        break;
      }
      case 'click':
        await __asaLocator(step).click({ timeout });
        break;
      case 'fill':
        await __asaLocator(step).fill(step.value || '', { timeout });
        break;
      case 'assert_text': {
        // Real forms often settle asynchronously after a click/fetch. Poll
        // within the caller's bounded per-step timeout, not a fixed sleep.
        const locator = __asaLocator(step);
        const deadline = Date.now() + timeout;
        let matched = false;
        while (Date.now() < deadline) {
          const remaining = deadline - Date.now();
          const value = await locator.innerText({ timeout: Math.min(remaining, 2000) });
          if (value.includes(step.expected)) { matched = true; break; }
          await page.waitForTimeout(Math.min(100, Math.max(0, deadline - Date.now())));
        }
        if (!matched) throw new Error('expected text substring was not found within timeout');
        break;
      }
      case 'assert_url':
        if (page.url() !== step.expected) throw new Error('exact URL assertion failed');
        break;
      case 'console': {
        const bounded = __asaBudgetedEntries(__asaConsole);
        item.entries = bounded.entries;
        item.dropped = __asaConsoleDropped + bounded.omitted;
        break;
      }
      case 'network': {
        const bounded = __asaBudgetedEntries(__asaNetwork);
        item.entries = bounded.entries;
        item.dropped = __asaNetworkDropped + bounded.omitted;
        break;
      }
      default:
        throw new Error('unsupported action');
    }
  } catch (error) {
    item.ok = false;
    // Playwright exceptions can include whole DOM excerpts and credentials.
    // Return only the first diagnostic line, bounded and without a stack.
    item.error = __asaCap(String(error?.message || error).split('\n')[0], 240);
    __asaFailedStep = i;
  }
  item.duration_ms = Date.now() - started;
  __asaResults.push(item);
  if (__asaFailedStep !== null) break;
}
console.log('ASA_BROWSER_E2E_RESULT ' + JSON.stringify({
  ok: __asaFailedStep === null,
  failed_step: __asaFailedStep,
  executed: __asaResults.length,
  results: __asaResults,
  console_events_dropped: __asaConsoleDropped,
  network_events_dropped: __asaNetworkDropped,
  output_budget_remaining_bytes: __asaOutputBudget
}));
if (__asaFailedStep !== null) process.exitCode = 1;
`
