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

// This exercises real Chromium, not an Incus guest/aaPanel/WordPress. It is
// explicitly opt-in because the optional pinned Browser runtime is not part
// of the base Go unit-test environment.
func TestManagedCredentialLoginPinnedChromium(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("opt-in: set AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME to the installed pinned Browser engine")
	}
	node := filepath.Join(engine, "node", "bin", "node")
	if _, err := os.Stat(node); err != nil {
		t.Fatal(err)
	}
	secret := "LOCAL-FIXTURE-CREDENTIAL-ONLY-NOT-A-REAL-SECRET"
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/ready" {
			c, err := r.Cookie("session")
			if err != nil || c.Value != "yes" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			fmt.Fprint(w, `<h1 id="dashboard">Dashboard authenticated</h1>`)
			return
		}
		if r.Method == "POST" {
			_ = r.ParseForm()
			if r.PostFormValue("user") == "testadmin" && r.PostFormValue("password") == secret {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "yes", HttpOnly: true, Path: "/"})
				http.Redirect(w, r, "/ready", http.StatusFound)
				return
			}
		}
		fmt.Fprint(w, `<!doctype html><form action="/login" method="post">
    <input name="user" id="user" autocomplete="username">
    <input name="password" id="secret" type="password" autocomplete="current-password">
    <button type="submit" id="submit">Sign in</button>
    <span id="mfa" style="display:none">MFA required</span>
  </form>`)
	}))
	defer app.Close()
	d := t.TempDir()
	if err := os.Mkdir(filepath.Join(d, "downloads"), 0700); err != nil {
		t.Fatal(err)
	}
	script, err := managedSessionRunner(engine, filepath.Join(d, "profile"), filepath.Join(d, "downloads"), false, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(d, "browser.mjs")
	if err := os.WriteFile(source, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, source)
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	reader := bufio.NewReader(out)
	read := func() map[string]any {
		t.Helper()
		wire, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("browser runner frame: %v stderr=%q", err, stderr.String())
		}
		if !strings.HasPrefix(wire, "ASA_BROWSER_SESSION ") {
			t.Fatalf("unexpected Browser frame %q", wire)
		}
		if strings.Contains(wire, secret) {
			t.Fatalf("secret disclosed in Browser result frame")
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(wire, "ASA_BROWSER_SESSION ")), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	send := func(data map[string]any) map[string]any {
		t.Helper()
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 16384 {
			t.Fatal("over-budget Browser stdin frame")
		}
		if _, err := in.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		return read()
	}
	if first := read(); first["event"] != "ready" {
		t.Fatalf("Browser not ready %+v stderr=%q", first, stderr.String())
	}
	navigate := func(url string, nonce string) {
		got := send(map[string]any{"type": "flow", "nonce": nonce, "steps": []FlowStep{{Action: "goto", URL: url}}})
		if got["event"] != "result" || got["result"].(map[string]any)["ok"] != true {
			t.Fatalf("navigation failed: %+v", got)
		}
	}
	origin := app.URL
	request := func(nonce, pass string) map[string]any {
		return map[string]any{"type": "credential_login", "nonce": nonce, "expected_origin": origin,
			"username": "testadmin", "secret": pass, "username_selector": "#user", "secret_selector": "#secret",
			"submit_selector": "#submit", "success_selector": "#dashboard", "success_url": "", "challenge_selector": "#mfa"}
	}
	navigate(app.URL+"/login", strings.Repeat("1", 32))
	// Refuse to inject into an unrelated origin, even with a valid store ref.
	foreign := request(strings.Repeat("2", 32), secret)
	foreign["expected_origin"] = "http://unrelated.invalid"
	mismatch := send(foreign)
	if mismatch["event"] != "error" || mismatch["reason"] != "credential_origin_mismatch" {
		t.Fatalf("cross-origin secret use was not denied: %+v", mismatch)
	}
	// Invalid password cannot produce an authenticated result, even if the
	// form submits and the page returns normal HTTP 200.
	invalid := send(request(strings.Repeat("3", 32), "fixture-wrong-password"))
	if invalid["event"] != "error" || invalid["reason"] != "credential_login_not_confirmed" {
		t.Fatalf("false-positive auth: %+v", invalid)
	}
	navigate(app.URL+"/login", strings.Repeat("4", 32))
	success := send(request(strings.Repeat("5", 32), secret))
	if success["event"] != "result" || success["result"].(map[string]any)["status"] != "authenticated" {
		t.Fatalf("real Playwright login unverified: %+v stderr=%q", success, stderr.String())
	}
	observed := send(map[string]any{"type": "flow", "nonce": strings.Repeat("6", 32), "steps": []FlowStep{{Action: "assert_text", Selector: "#dashboard", Expected: "Dashboard authenticated"}}})
	if observed["event"] != "result" || observed["result"].(map[string]any)["ok"] != true {
		t.Fatalf("authenticated navigation failed: %+v", observed)
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatal("credential leaked to Node stderr")
	}
	if _, err := in.Write([]byte("{\"type\":\"close\"}\n")); err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Browser close: %v stderr=%q", err, stderr.String())
	}
}
