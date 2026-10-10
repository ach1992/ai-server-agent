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
	case "snapshot":
		if s.URL != "" || s.Value != "" || s.Expected != "" {
			return errors.New("snapshot accepts only optional selector or role/name")
		}
	case "console", "network":
		if s.URL != "" || hasLocator || s.Name != "" || s.Value != "" || s.Expected != "" {
			return errors.New("console and network actions take no element or value fields")
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

// Never echo raw navigation error messages: Chromium/Playwright includes the
// full failed URL (including query and fragment) in many failure paths.
const __asaNavigationFailure = error => {
  const message = String(error?.message || error);
  const code = message.match(/\bnet::(ERR_[A-Z0-9_]+)/);
  if (code) return 'navigation_' + code[1];
  if (/timeout/i.test(message)) return 'navigation_timeout';
  return 'navigation_failed';
};
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
        const target = step.selector || step.role ? __asaLocator(step) : page.locator('body');
        // Guard before invoking Playwright's unbounded ariaSnapshot().
        // Count local content AND outside accessible-name/ownership references:
        // ARIA IDREFs, associated native <label>s and transitive references.
        // A tiny scoped target can otherwise pull megabytes from elsewhere.
        const scope = await target.evaluate(element => {
          const budget = { nodes: 0, text_units: 0, attribute_units: 0,
            state_units: 0, generated_units: 0 };
          const roots = [element];
          const queued = new Set(roots);
          const visited = new Set();
          const reject = reason => ({ too_large: true, reason, ...budget });
          const addRoot = node => {
            if (!node || visited.has(node) || queued.has(node)) return false;
            // Bound the pending reference graph, including cyclic IDREFs.
            if (queued.size >= 4000) return true;
            queued.add(node);
            roots.push(node);
            return false;
          };
          const add = (field, units) => {
            budget[field] += units;
            return (field !== 'text_units' && units > 16384) || budget.nodes > 4000 ||
              budget.text_units > 70000 || budget.attribute_units > 70000 ||
              budget.state_units > 70000 || budget.generated_units > 70000 ||
              (budget.text_units + budget.attribute_units +
               budget.state_units + budget.generated_units) > 120000;
          };
          const idrefs = ['aria-labelledby', 'aria-describedby', 'aria-owns',
            'aria-details', 'aria-errormessage'];
          while (roots.length) {
            const root = roots.pop();
            if (visited.has(root)) continue;
            const walker = document.createTreeWalker(root,
              NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT);
            let node = root;
            do {
              if (visited.has(node)) {
                node = walker.nextNode();
                continue;
              }
              visited.add(node);
              budget.nodes++;
              if (budget.nodes > 4000) return reject('node_count');
              if (node.nodeType === Node.TEXT_NODE) {
                if (add('text_units', node.length))
                  return { too_large: true, reason: 'text', ...budget };
              } else if (node.nodeType === Node.ELEMENT_NODE) {
                for (const attr of node.attributes) {
                  if (add('attribute_units', attr.value.length))
                    return { too_large: true, reason: 'attribute', ...budget };
                }
                if (node instanceof HTMLInputElement ||
                    node instanceof HTMLTextAreaElement ||
                    node instanceof HTMLSelectElement) {
                  if (add('state_units', node.value.length))
                    return { too_large: true, reason: 'control_value', ...budget };
                }
                if (node.shadowRoot && addRoot(node.shadowRoot))
                  return reject('dependency_refs');
                // Resolve references within their actual document/shadow root,
                // not by scanning an unbounded document for labels/IDs.
                const tree = node.getRootNode();
                if (tree && typeof tree.getElementById === 'function') {
                  for (const attr of idrefs) {
                    const ids = node.getAttribute(attr);
                    if (!ids) continue;
                    for (const id of ids.trim().split(/\s+/)) {
                      if (id && addRoot(tree.getElementById(id)))
                        return reject('dependency_refs');
                    }
                  }
                } else if (idrefs.some(attr => node.hasAttribute(attr))) {
                  return reject('unresolvable_references');
                }
                // Native labels may be anywhere in the same document, not
                // descendants of the scoped form control.
                if ('labels' in node && node.labels) {
                  for (const label of node.labels) {
                    if (addRoot(label)) return reject('dependency_refs');
                  }
                }
                // ariaSnapshot includes generated content from CSS pseudo
                // elements, which does not appear as a DOM text node.
                for (const pseudo of ['::before', '::after', '::marker']) {
                  const generated = getComputedStyle(node, pseudo).content;
                  if (add('generated_units', generated?.length || 0))
                    return { too_large: true, reason: 'generated_content', ...budget };
                }
              }
              node = walker.nextNode();
            } while (node);
          }
          return { too_large: false };
        }, undefined, { timeout });
        if (scope.too_large) {
          item.snapshot = '';
          item.truncated = true;
          item.reason = 'dom_too_large';
          item.scanned_nodes = scope.nodes;
          item.preflight_reason = scope.reason;
          break;
        }
        const content = await target.ariaSnapshot({ timeout });
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
        // Element absence is normal during SPA/data-load transitions. A
        // short innerText timeout must not prematurely end a longer caller
        // deadline. Retry only within that exact bounded step deadline.
        const locator = __asaLocator(step);
        const deadline = Date.now() + timeout;
        let matched = false;
        while (Date.now() < deadline) {
          const remaining = deadline - Date.now();
          try {
            const value = await locator.innerText({ timeout: Math.min(remaining, 500) });
            if (value.includes(step.expected)) { matched = true; break; }
          } catch (_) {
            // Transient absence/visibility/locator errors are retried only
            // while the caller's deadline still has budget.
          }
          const pause = Math.min(100, Math.max(0, deadline - Date.now()));
          if (pause > 0) await page.waitForTimeout(pause);
        }
        if (!matched) throw new Error('expected text substring was not found within timeout');
        break;
      }
      case 'assert_url': {
        const deadline = Date.now() + timeout;
        let matched = page.url() === step.expected;
        while (!matched && Date.now() < deadline) {
          const pause = Math.min(100, Math.max(0, deadline - Date.now()));
          if (pause > 0) await page.waitForTimeout(pause);
          matched = page.url() === step.expected;
        }
        if (!matched) throw new Error('exact URL was not reached within timeout');
        break;
      }
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
    // Navigation failures routinely include the original URL with private
    // query/fragment data. Keep a useful *classification* but never echo
    // their raw message; the request path is separately normalized.
    if (step.action === 'goto') {
      item.error = __asaNavigationFailure(error);
      item.url = __asaURL(step.url);
    } else {
      item.error = __asaCap(String(error?.message || error).split('\n')[0], 240);
    }
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
