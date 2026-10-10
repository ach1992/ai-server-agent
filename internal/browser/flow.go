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
	maxFlowRefBytes      = 8
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
	Selector  string `json:"selector,omitempty" jsonschema:"CSS locator (instead of role/name/ref); maximum 512 UTF-8 bytes"`
	Ref       string `json:"ref,omitempty" jsonschema:"Snapshot-issued element ref (e1, e2, ...); usable only within this one browser_e2e call, not across calls"`
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

func validFlowRef(ref string) bool {
	if len(ref) < 2 || len(ref) > maxFlowRefBytes || ref[0] != 'e' || ref[1] < '1' || ref[1] > '9' {
		return false
	}
	for i := 2; i < len(ref); i++ {
		if ref[i] < '0' || ref[i] > '9' {
			return false
		}
	}
	return true
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
	hasLocator := s.Selector != "" || s.Role != "" || s.Ref != ""
	if (s.Ref != "" && (!validFlowRef(s.Ref) || s.Selector != "" || s.Role != "" || s.Name != "")) ||
		(s.Selector != "" && (s.Role != "" || s.Name != "")) || (s.Name != "" && s.Role == "") {
		return errors.New("supply exactly one of selector, role/name or a valid snapshot ref")
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
		if s.URL != "" || s.Ref != "" || s.Value != "" || s.Expected != "" {
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
// Refs are only issued from a verified, stable accessibility snapshot
// generation. This map never survives the one browser_e2e call.
const __asaRefs = new Map();
let __asaRefSequence = 0;
const __asaMaxRefs = 64;
const __asaRefRoles = ['textbox','button','link','checkbox','radio','combobox','switch','tab','option','menuitem'];
// This safety check is serialized into the browser for both issuance and use.
// A CSS-visible aria-hidden or inert descendant must not become actionable.
const __asaRefAccessible = el => {
  if (!el.isConnected || el.ownerDocument !== document) return false;
  for (let n = el; n;) {
    if (n.nodeType === Node.ELEMENT_NODE &&
        (n.hasAttribute('hidden') || n.inert ||
         n.getAttribute('aria-hidden')?.toLowerCase() === 'true')) return false;
    n = n.parentElement || n.getRootNode()?.host || null;
  }
  return true;
};
const __asaGetTarget = async step => {
  if (!step.ref) return __asaLocator(step);
  const entry = __asaRefs.get(step.ref);
  if (!entry) throw new Error('invalid_ref');
  if (entry.url !== page.url()) throw new Error('stale_ref');
  const active = await entry.element.evaluate(__asaRefAccessible).catch(() => false);
  if (!active) throw new Error('stale_ref');
  return entry.element;
};
// Playwright's getByRole() uses accessibility semantics instead of raw CSS
// visibility. The complete, untruncated snapshot must contain the same count
// of each emitted role; otherwise ALL refs are unavailable (never guess).
const __asaSnapshotRoleHeaders = snapshot => {
  const headers = new Map();
  const expression = /^\s*- (textbox|button|link|checkbox|radio|combobox|switch|tab|option|menuitem)(?=[\s:]|$)/;
  for (const line of snapshot.split('\n')) {
    const match = expression.exec(line);
    if (!match) continue;
    if (!headers.has(match[1])) headers.set(match[1], []);
    headers.get(match[1]).push(line.trim());
  }
  return headers;
};
// Handles are staged but NOT published until the root/document mutation
// monitor and an identical second ariaSnapshot have both been verified.
const __asaSnapshotRefs = async (target, content, stable, timeout) => {
  const roles = __asaSnapshotRoleHeaders(content);
  const handles = [];
  let dropped = 0;
  for (const role of __asaRefRoles) {
    const headers = roles.get(role) || [];
    const expected = headers.length;
    if (!expected) continue;
    // Two identical accessible headers cannot be mapped unambiguously to
    // distinct DOM handles, even when count and document generation match.
    if (new Set(headers).size !== expected) return null;
    if (!await stable()) return null;
    const locator = target.getByRole(role);
    const actual = await locator.count();
    if (!await stable() || actual !== expected) return null;
    const capacity = Math.max(0, Math.min(24 - handles.length,
      __asaMaxRefs - __asaRefSequence - handles.length));
    const take = Math.min(actual, capacity);
    dropped += actual - take;
    for (let i = 0; i < take; i++) {
      if (!await stable()) return null;
      const current = locator.nth(i);
      // ARIA ownership can reorder the accessibility tree without changing
      // DOM/getByRole order. Require the exact role+name/state header to
      // agree with the entry at the SAME position in the returned snapshot.
      const ownSnapshot = await current.ariaSnapshot({
        timeout: Math.min(timeout, 1000)
      }).catch(() => null);
      if (!ownSnapshot || ownSnapshot.split('\n', 1)[0].trim() !== headers[i] ||
          !await stable()) return null;
      const handle = await current.elementHandle({
        timeout: Math.min(timeout, 1000)
      }).catch(() => null);
      if (!handle || !await stable() ||
          !await handle.evaluate(__asaRefAccessible).catch(() => false)) return null;
      // Only static role/index metadata crosses the renderer. A candidate
      // with an unproved or reordered accessible header receives no ref.
      handles.push({ element: handle, role, role_index: i });
    }
  }
  return { handles, dropped };
};
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
        __asaRefs.clear();
        await page.goto(step.url, { waitUntil: 'domcontentloaded', timeout });
        item.url = __asaURL(page.url());
        break;
      case 'snapshot': {
        const target = step.selector || step.role ? __asaLocator(step) : page.locator('body');
        // Guard before invoking Playwright's unbounded ariaSnapshot().
        // Count local content AND outside accessible-name/ownership references:
        // ARIA IDREFs, associated native <label>s and transitive references.
        // A tiny scoped target can otherwise pull megabytes from elsewhere.
        const root = await target.elementHandle({ timeout });
        // Install before the DOM preflight, ariaSnapshot and role lookups.
        // A document replacement (including same-URL reload), detached root
        // or any DOM mutation invalidates the snapshot/ref association.
        const monitor = await root.evaluateHandle(el => {
          const doc = el.ownerDocument;
          let changed = false;
          const observer = new MutationObserver(() => { changed = true; });
          observer.observe(doc, {
            subtree: true, childList: true, attributes: true, characterData: true
          });
          return {
            stable: () => {
              if (observer.takeRecords().length) changed = true;
              return !changed && doc === document && el.isConnected;
            },
            close: () => observer.disconnect()
          };
        });
        const stable = () => monitor.evaluate(m => m.stable()).catch(() => false);
        item.refs = [];
        item.refs_unavailable = true;
        item.refs_dropped = null;
        item.refs_scope = 'flow_only';
        try {
        const scope = await root.evaluate(element => {
          const budget = { nodes: 0, text_units: 0, attribute_units: 0,
            state_units: 0, generated_units: 0,
            has_shadow: element.getRootNode() instanceof ShadowRoot };
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
          // ARIA external references have two independent representations:
          // DOM-reflected Element arrays/singletons, which may point to
          // ID-less nodes and clear the corresponding content attribute,
          // and legacy attribute IDREF strings. Check both without querying
          // the entire document, using ONE visited/queued/content budget.
          const relationships = [
            ['ariaLabelledByElements', 'aria-labelledby', true],
            ['ariaDescribedByElements', 'aria-describedby', true],
            ['ariaOwnsElements', 'aria-owns', true],
            ['ariaDetailsElements', 'aria-details', true],
            ['ariaErrorMessageElements', 'aria-errormessage', true],
            ['ariaControlsElements', 'aria-controls', true],
            ['ariaFlowToElements', 'aria-flowto', true],
            ['ariaActiveDescendantElement', 'aria-activedescendant', false],
          ];
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
                if (node.shadowRoot) {
                  // A document observer cannot follow mutations in shadow
                  // trees; fail closed on refs for this snapshot scope.
                  budget.has_shadow = true;
                  if (addRoot(node.shadowRoot)) return reject('dependency_refs');
                }
                // The same node may expose a reflected property and an IDREF
                // attribute; consider both so neither can bypass preflight.
                // Property paths are essential for ID-less remote Elements.
                const tree = node.getRootNode();
                for (const [property, attr, multiple] of relationships) {
                  if (property in node) {
                    let refs;
                    try {
                      refs = node[property];
                    } catch {
                      return reject('unresolvable_references');
                    }
                    if (refs != null) {
                      if (multiple) {
                        if (!Array.isArray(refs) || refs.length > 4000)
                          return reject('dependency_refs');
                        for (const ref of refs) {
                          if (!(ref instanceof Element))
                            return reject('unresolvable_references');
                          if (addRoot(ref)) return reject('dependency_refs');
                        }
                      } else {
                        if (!(refs instanceof Element))
                          return reject('unresolvable_references');
                        if (addRoot(refs)) return reject('dependency_refs');
                      }
                    }
                  }
                  const ids = node.getAttribute(attr);
                  if (!ids) continue;
                  if (!tree || typeof tree.getElementById !== 'function')
                    return reject('unresolvable_references');
                  for (const id of ids.trim().split(/\s+/)) {
                    if (id && addRoot(tree.getElementById(id)))
                      return reject('dependency_refs');
                  }
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
          return { too_large: false, has_shadow: budget.has_shadow };
        }, undefined, { timeout });
        if (scope.too_large) {
          item.snapshot = '';
          item.truncated = true;
          item.reason = 'dom_too_large';
          item.scanned_nodes = scope.nodes;
          item.preflight_reason = scope.reason;
          break;
        }
        if (!await stable()) {
          item.snapshot = '';
          item.truncated = true;
          item.reason = 'dom_changed_during_preflight';
          break;
        }
        const content = await target.ariaSnapshot({ timeout });
        const bounded = __asaBudgeted(content);
        item.total_bytes = Buffer.byteLength(content, 'utf8');
        item.snapshot = bounded.text;
        item.truncated = bounded.truncated;
        // Never issue a ref to an element not present in the complete,
        // stable accessibility snapshot. A document mutation/reload or
        // unsupported shadow subtree yields snapshot text but no refs.
        if (bounded.truncated || scope.has_shadow || !await stable()) {
          item.refs_reason = bounded.truncated ? 'snapshot_truncated' :
            scope.has_shadow ? 'shadow_scope' : 'snapshot_changed';
          break;
        }
        const candidates = await __asaSnapshotRefs(target, content, stable, timeout).catch(() => null);
        if (!candidates || !await stable()) {
          item.refs_reason = 'snapshot_changed';
          break;
        }
        // Re-check accessibility after the independent role lookups. This
        // detects CSSOM/accessibility changes even without a DOM mutation.
        const verify = await target.ariaSnapshot({ timeout }).catch(() => null);
        if (verify !== content || !await stable()) {
          item.refs_reason = 'snapshot_changed';
          break;
        }
        const projected = candidates.handles.map((entry, j) => ({
          ref: 'e' + (__asaRefSequence + j + 1),
          role: entry.role, role_index: entry.role_index
        }));
        const selected = __asaBudgetedEntries(projected);
        if (!await stable()) {
          item.refs_reason = 'snapshot_changed';
          break;
        }
        for (let j = 0; j < selected.entries.length; j++) {
          const id = selected.entries[j].ref;
          __asaRefs.set(id, { element: candidates.handles[j].element, url: page.url() });
        }
        __asaRefSequence += selected.entries.length;
        item.refs = selected.entries;
        item.refs_dropped = candidates.dropped + selected.omitted;
        item.refs_unavailable = false;
        break;
        } finally {
          await monitor.evaluate(m => m.close()).catch(() => {});
          await monitor.dispose().catch(() => {});
          await root.dispose().catch(() => {});
        }
      }
      case 'click':
        await (await __asaGetTarget(step)).click({ timeout });
        break;
      case 'fill':
        await (await __asaGetTarget(step)).fill(step.value || '', { timeout });
        break;
      case 'assert_text': {
        // Element absence is normal during SPA/data-load transitions. A
        // short innerText timeout must not prematurely end a longer caller
        // deadline. Retry only within that exact bounded step deadline.
        const locator = await __asaGetTarget(step);
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
     } else if (step.action === 'snapshot') {
      item.error = 'snapshot_failed';
    } else if (step.ref) {
      // ElementHandle action failures can include the page's sensitive URL,
      // query or DOM. Ref errors are finite classifications, never raw text.
      const message = String(error?.message || error);
      item.error = message === 'invalid_ref' || message === 'stale_ref' ?
        message : 'ref_action_failed';
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
