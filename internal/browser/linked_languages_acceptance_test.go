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

// Real opt-in linked HTML/CSS/JS acceptance: offline LSP syntax analysis is
// not a substitute for computed browser style or executed DOM event behavior.
// This fixture uses only a fresh worker test-owned Chromium profile and a
// private httptest listener; it does not change the installed Agent/browser.
func TestLinkedHTMLCSSJavaScriptRealBrowser(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("requires pinned Playwright/Chromium runtime")
	}
	rel := "chrome-linux64/chrome"
	if runtime.GOARCH == "arm64" {
		rel = "chrome-linux/chrome"
	}
	node := filepath.Join(engine, "node/bin/node")
	chrome := filepath.Join(engine, "browsers", "chromium-"+browserChromiumRevision, rel)
	pw := filepath.Join(engine, "node_modules/playwright/index.mjs")
	for _, path := range []string{node, chrome, pw} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("pinned browser executable/resources missing or unsafe: %s %v", path, err)
		}
	}
	// HTTP server writes only fixture-owned immutable source, with no DB or
	// external dependencies. The browser is authorized to run these pages.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/style.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			fmt.Fprint(w, `#hero { color: rgb(17, 85, 170); font-weight: 700; }
body { direction: rtl; }`)
		case "/app.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			fmt.Fprint(w, `document.getElementById("go").addEventListener("click", () => {
  document.getElementById("hero").textContent = "Clicked ✓";
});`)
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html>
<html lang="fa" dir="rtl">
<head><meta charset="utf-8"><title>Linked languages fixture</title>
<link rel="stylesheet" href="/style.css"><script defer src="/app.js"></script></head>
<body><main><h1 id="hero">Before click</h1><button id="go">Go</button></main></body>
</html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(root, "check.mjs")
	js := fmt.Sprintf(`import { chromium } from %q;
const context = await chromium.launchPersistentContext(%q,{headless:true,executablePath:%q});
const page = context.pages()[0] || await context.newPage();
try {
 await page.goto(%q,{waitUntil:"load",timeout:12000});
 const before = await page.evaluate(() => {
  const hero = document.getElementById('hero');
  const style = getComputedStyle(hero);
  const stylesheet = document.querySelector('link[rel="stylesheet"]');
  const script = document.querySelector('script[src]');
  return {text: hero.textContent, color: style.color,
    weight: style.fontWeight, direction: getComputedStyle(document.body).direction,
    lang: document.documentElement.lang,
    cssLink: stylesheet?.href, jsLink: script?.src,
    styleSheets: document.styleSheets.length};
 });
 if (before.text !== "Before click" || before.color !== "rgb(17, 85, 170)" ||
     before.weight !== "700" || before.direction !== "rtl" || before.lang !== "fa" ||
     !before.cssLink.endsWith("/style.css") || !before.jsLink.endsWith("/app.js") ||
     before.styleSheets < 1) {
  throw new Error("linked stylesheet/HTML readiness violated: " + JSON.stringify(before));
 }
 await page.getByRole('button',{name:'Go'}).click();
 await page.getByRole('heading',{name:'Clicked ✓'}).waitFor({timeout:6000});
 const after = await page.evaluate(() => ({
    text: document.getElementById('hero').textContent,
    color: getComputedStyle(document.getElementById('hero')).color
 }));
 if (after.text !== "Clicked ✓" || after.color !== before.color) {
  throw new Error("linked JS/CSS DOM behavior violated: " + JSON.stringify(after));
 }
 console.log("ASA_LINKED_WEB_E2E " + JSON.stringify({ok:true,before,after}));
} finally { await context.close(); }
`, "file://"+pw, profile, chrome, srv.URL)
	if err := os.WriteFile(runner, []byte(js), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, runner)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real linked HTML/CSS/JS Browser failed: %v: %.1000s", err, out)
	}
	const marker = "ASA_LINKED_WEB_E2E "
	lines := strings.Split(string(out), "\n")
	verified := false
	for _, line := range lines {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		var evidence struct {
			OK     bool `json:"ok"`
			Before struct {
				Color, Direction, Lang string
				StyleSheets            int `json:"styleSheets"`
			} `json:"before"`
			After struct{ Text string } `json:"after"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &evidence); err != nil {
			t.Fatalf("bad browser evidence JSON: %v", err)
		}
		if !evidence.OK || evidence.Before.Color != "rgb(17, 85, 170)" ||
			evidence.Before.Direction != "rtl" || evidence.Before.Lang != "fa" ||
			evidence.Before.StyleSheets < 1 || evidence.After.Text != "Clicked ✓" {
			t.Fatalf("Browser claim missing real linked language assertions: %+v", evidence)
		}
		verified = true
	}
	if !verified {
		t.Fatalf("linked Browser test did not emit result: %.1000s", out)
	}
	t.Log("real Chromium fetched linked HTML, CSS, JS: CSS computed color/weight/direction, RTL/lang, and click DOM mutation confirmed")
}
