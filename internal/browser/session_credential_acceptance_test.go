package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
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
	// A separate real HTTP origin models a post-auth SSO callback. It must
	// receive navigation only, never the credential form submission.
	var ssoArrivals atomic.Int32
	ssoDestination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ssoArrivals.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/dashboard" || strings.Contains(r.URL.String(), secret) {
			t.Errorf("SSO destination received credential or unexpected method/path: %s %s", r.Method, r.URL.String())
		}
		fmt.Fprint(w, `<h1 id="sso-dashboard">Post-auth redirect completed</h1>`)
	}))
	defer ssoDestination.Close()
	var rotatingMu sync.Mutex
	rotatingTokens := make(map[string]string)
	var rotatingGets, rotatingCookieProbes int
	var rotatingInitialID, rotatingPostID string
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/sso" {
			if r.Method == http.MethodPost {
				_ = r.ParseForm()
				if r.PostFormValue("user") == "testadmin" && r.PostFormValue("password") == secret {
					http.Redirect(w, r, ssoDestination.URL+"/dashboard", http.StatusFound)
				} else {
					fmt.Fprint(w, `<div id="login-error">Credentials rejected</div>`)
				}
				return
			}
			fmt.Fprint(w, `<form action="/sso" method="post"><input id="user" name="user"><input id="secret" name="password" type="password"><button id="submit">Sign in</button></form>`)
			return
		}
		if r.URL.Path == "/origin-parity" {
			// Use the real Chromium URL parser, not a mock, to compare
			// cross-language default/nondefault-port origin semantics.
			fmt.Fprint(w, `<!doctype html><div id="origin-parity"></div><script>
          const endpoints = [
            "https://Example.ORG:443/login", "http://Example.ORG:80/login",
            "https://Example.ORG:0443/login", "http://Example.ORG:0080/login",
            "https://Example.ORG:8443/login", "http://Example.ORG:8080/login",
            "http://Example.ORG:443/login", "https://Example.ORG:80/login",
            "https://[2001:DB8::1]:443/login", "http://[::1]:80/login"
          ];
          document.querySelector("#origin-parity").textContent =
            endpoints.map(value => new URL(value).origin).join("|");
        </script>`)
			return
		}
		if r.URL.Path == "/foreign-form" {
			fmt.Fprint(w, `<!doctype html><form action="http://unrelated.invalid:80/sink" method="post">
            <input id="user" name="user"><input id="secret" name="password" type="password">
            <button id="submit">Sign in</button>
          </form>`)
			return
		}
		if r.URL.Path == "/ready" {
			c, err := r.Cookie("session")
			if err != nil || c.Value != "yes" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			fmt.Fprint(w, `<h1 id="dashboard">Dashboard authenticated</h1>`)
			return
		}
		if r.URL.Path == "/rotating" {
			rotatingMu.Lock()
			defer rotatingMu.Unlock()
			cookie, _ := r.Cookie("rotating_session")
			id := ""
			if cookie != nil {
				id = cookie.Value
			}
			if r.Method == http.MethodPost {
				_ = r.ParseForm()
				rotatingPostID = id
				if id != "" && r.PostFormValue("token") == rotatingTokens[id] &&
					r.PostFormValue("user") == "testadmin" && r.PostFormValue("password") == secret {
					fmt.Fprint(w, `<h1 id="dashboard">Dashboard authenticated</h1>`)
				} else {
					fmt.Fprint(w, `<div id="login-error">Session token rejected</div>`)
				}
				return
			}
			rotatingGets++
			if id == "" {
				id = fmt.Sprintf("session-%d", rotatingGets)
			} else {
				rotatingCookieProbes++
			}
			if rotatingInitialID == "" {
				rotatingInitialID = id
			}
			// A login-page GET rotates the token for its session. A readiness
			// request must neither reuse the Browser cookie nor replace it.
			token := fmt.Sprintf("token-%d", rotatingGets)
			rotatingTokens[id] = token
			http.SetCookie(w, &http.Cookie{Name: "rotating_session", Value: id, HttpOnly: true, Path: "/"})
			fmt.Fprintf(w, `<!doctype html><form action="/rotating" method="post">
          <input id="user" name="user"><input id="secret" name="password" type="password">
          <input type="hidden" name="token" value="%s"><button id="submit">Sign in</button>
        </form>`, token)
			return
		}
		if r.URL.Path == "/unready" {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "maintenance")
			return
		}
		if r.URL.Path == "/retain" {
			fmt.Fprint(w, `<!doctype html>
          <form id="retained">
           <input id="user"><input id="secret" type="password">
           <button id="submit">Sign in</button>
          </form>
          <div id="dashboard" style="display:none">Dashboard retained authenticated</div>
          <div id="fieldstate">pending</div>
          <script>
           document.querySelector("#retained").addEventListener("submit",e=>{
             e.preventDefault();
             document.querySelector("#secret").setAttribute("value",document.querySelector("#secret").value);
             document.querySelector("#dashboard").style.display="block";
           });
           setInterval(()=>{
             const field=document.querySelector("#secret");
             document.querySelector("#fieldstate").textContent=
               document.querySelector("#dashboard").style.display==="block" &&
               field.value==="" && !field.hasAttribute("value") ? "cleared":"pending";
           },10);
          </script>`)
			return
		}
		if r.URL.Path == "/mfa" {
			fmt.Fprint(w, `<!doctype html><form id="login">
            <input id="user"><input id="secret" type="password"><button id="submit">Sign in</button>
          </form><div id="dashboard" style="display:none">Success</div>
          <div id="mfa" style="display:none">Challenge required</div>
          <script>document.querySelector("#login").addEventListener("submit",e=>{
            e.preventDefault();document.querySelector("#mfa").style.display="block"
          })</script>`)
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
		errorNotice := ""
		if r.Method == "POST" {
			errorNotice = `<div id="login-error">Credentials rejected</div>`
		}
		fmt.Fprint(w, `<!doctype html><form action="/login" method="post">
    <input name="user" id="user" autocomplete="username">
    <input name="password" id="secret" type="password" autocomplete="current-password">
    <button type="submit" id="submit">Sign in</button>
    <span id="mfa" style="display:none">MFA required</span>
  </form>`+errorNotice)
	}))
	// Ordinary CI runs on a random loopback port. An explicitly opt-in,
	// root-only disposable Test-host run additionally proves a real HTTP :80
	// or HTTPS :443 login with the registered endpoint's explicit default
	// port; no external listener or privileged port is used in normal CI.
	defaultPortMode := os.Getenv("ASA_BROWSER_DEFAULT_PORT_SMOKE")
	switch defaultPortMode {
	case "":
		app.Start()
	case "http", "https":
		if os.Geteuid() != 0 {
			t.Skip("default-port Browser acceptance requires explicit root-only Test-host opt-in")
		}
		port := 80
		if defaultPortMode == "https" {
			port = 443
		}
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("dedicated Test-host loopback port unavailable: %v", err)
		}
		_ = app.Listener.Close()
		app.Listener = listener
		if defaultPortMode == "https" {
			app.StartTLS()
		} else {
			app.Start()
		}
	default:
		t.Fatal("unsupported default-port Browser smoke mode")
	}
	defer app.Close()
	d := t.TempDir()
	if err := os.Mkdir(filepath.Join(d, "downloads"), 0700); err != nil {
		t.Fatal(err)
	}
	script, err := managedSessionRunner(engine, filepath.Join(d, "profile"), filepath.Join(d, "downloads"), defaultPortMode == "https", "isolated")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(d, "browser.mjs")
	if err := os.WriteFile(source, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
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
	origin := serviceaccess.Origin(app.URL)
	request := func(nonce, pass string) map[string]any {
		return map[string]any{"type": "credential_login", "nonce": nonce, "expected_origin": origin,
			"username": "testadmin", "secret": pass, "username_selector": "#user", "secret_selector": "#secret",
			"submit_selector": "#submit", "success_selector": "#dashboard", "success_url": "", "challenge_selector": "#mfa",
			"login_error_selector": "#login-error", "service_endpoint": app.URL + "/login"}
	}
	// Guard against the original F1 false-positive: Chromium canonicalizes
	// explicit default ports, while Go supplies expected_origin to the
	// protected login runner. A disagreement rejects a legitimate login.
	parityInputs := []string{
		"https://Example.ORG:443/login", "http://Example.ORG:80/login",
		"https://Example.ORG:0443/login", "http://Example.ORG:0080/login",
		"https://Example.ORG:8443/login", "http://Example.ORG:8080/login",
		"http://Example.ORG:443/login", "https://Example.ORG:80/login",
		"https://[2001:DB8::1]:443/login", "http://[::1]:80/login",
	}
	parityOrigins := make([]string, 0, len(parityInputs))
	for _, endpoint := range parityInputs {
		parityOrigins = append(parityOrigins, serviceaccess.Origin(endpoint))
	}
	navigate(app.URL+"/origin-parity", strings.Repeat("0", 31)+"a")
	parity := send(map[string]any{"type": "flow", "nonce": strings.Repeat("0", 31) + "b",
		"steps": []FlowStep{{Action: "assert_text", Selector: "#origin-parity",
			Expected: strings.Join(parityOrigins, "|")}}})
	if parity["event"] != "result" || parity["result"].(map[string]any)["ok"] != true {
		t.Fatalf("Chromium URL.origin differs from protected Go credential origin: %+v", parity)
	}
	navigate(app.URL+"/login", strings.Repeat("1", 32))
	// Refuse to inject into an unrelated origin, even with a valid store ref.
	foreign := request(strings.Repeat("2", 32), secret)
	foreign["expected_origin"] = "http://unrelated.invalid"
	mismatch := send(foreign)
	if mismatch["event"] != "error" || mismatch["reason"] != "credential_origin_mismatch" {
		t.Fatalf("cross-origin secret use was not denied: %+v", mismatch)
	}
	// The registered-origin page may contain a malicious cross-origin form
	// action. Refuse it before sending a credential to another destination,
	// even if its URL explicitly uses a default HTTP port.
	navigate(app.URL+"/foreign-form", strings.Repeat("0", 31)+"c")
	crossForm := request(strings.Repeat("0", 31)+"d", secret)
	crossForm["service_endpoint"] = app.URL + "/foreign-form"
	crossFormResult := send(crossForm)
	if crossFormResult["event"] != "error" ||
		crossFormResult["reason"] != "credential_form_origin_mismatch" {
		t.Fatalf("cross-origin form action not blocked before submission: %+v", crossFormResult)
	}
	navigate(app.URL+"/login", strings.Repeat("0", 31)+"e")
	// Invalid password cannot produce an authenticated result, even if the
	// form submits and the page returns normal HTTP 200.
	invalid := send(request(strings.Repeat("3", 32), "fixture-wrong-password"))
	if invalid["event"] != "error" || invalid["reason"] != "credential_invalid_login" {
		t.Fatalf("false-positive auth: %+v", invalid)
	}
	navigate(app.URL+"/login", strings.Repeat("4", 32))
	loginStarted := time.Now()
	success := send(request(strings.Repeat("5", 32), secret))
	loginElapsed := time.Since(loginStarted)
	if success["event"] != "result" || success["result"].(map[string]any)["status"] != "authenticated" {
		t.Fatalf("real Playwright login unverified: %+v stderr=%q", success, stderr.String())
	}
	if loginElapsed >= 10*time.Second {
		t.Errorf("successful navigation login waited for the vanished password field: %s", loginElapsed)
	}
	t.Logf("successful navigation login completed in %s", loginElapsed)
	observed := send(map[string]any{"type": "flow", "nonce": strings.Repeat("6", 32), "steps": []FlowStep{{Action: "assert_text", Selector: "#dashboard", Expected: "Dashboard authenticated"}}})
	if observed["event"] != "result" || observed["result"].(map[string]any)["ok"] != true {
		t.Fatalf("authenticated navigation failed: %+v", observed)
	}
	// Rev 4: authenticated visual/diagnostic capabilities remain usable
	// on the SAME Chromium context that received private credential input.
	for i, step := range []FlowStep{{Action: "snapshot"}, {Action: "console"}, {Action: "network"}} {
		nonce := fmt.Sprintf("%032x", 101+i)
		result := send(map[string]any{"type": "flow", "nonce": nonce, "steps": []FlowStep{step}})
		if result["event"] != "result" || result["result"].(map[string]any)["ok"] != true {
			t.Fatalf("authenticated %s denied: %+v", step.Action, result)
		}
	}
	advanced := send(map[string]any{"type": "script", "nonce": fmt.Sprintf("%032x", 104),
		"script": "await page.locator('#dashboard').evaluate(el=>el.setAttribute('data-advanced','yes')); console.log('protected-session-advanced-ok'); return await page.locator('#dashboard').getAttribute('data-advanced');"})
	if advanced["event"] != "result" {
		t.Fatalf("script frame %+v", advanced)
	}
	outcome := advanced["result"].(map[string]any)
	if outcome["ok"] != true || outcome["value"] != "yes" ||
		outcome["logs"].([]any)[0] != "protected-session-advanced-ok" {
		t.Fatalf("advanced same-session Playwright failed: %+v", advanced)
	}
	image := send(map[string]any{"type": "image_capture", "nonce": fmt.Sprintf("%032x", 105),
		"quality": 40, "max_width": 640, "max_height": 360, "crop_x": 0, "crop_y": 0})
	if image["event"] != "image_meta" || image["size"].(float64) < 4 {
		t.Fatalf("authenticated screenshot unavailable: %+v", image)
	}
	digest := "sha256:" + image["sha256"].(string)
	window := send(map[string]any{"type": "image_read", "nonce": fmt.Sprintf("%032x", 106),
		"file_version": digest, "offset": 0, "limit": 1024})
	if window["event"] != "image_chunk" || window["data"] == "" ||
		window["sha256"] != image["sha256"] {
		t.Fatalf("authenticated screenshot continuation failed: %+v", window)
	}
	discarded := send(map[string]any{"type": "image_discard", "nonce": fmt.Sprintf("%032x", 107), "file_version": digest})
	if discarded["event"] != "image_discarded" {
		t.Fatalf("image discard failed: %+v", discarded)
	}
	trace := send(map[string]any{"type": "trace_record", "nonce": fmt.Sprintf("%032x", 108),
		"steps": []FlowStep{{Action: "assert_text", Selector: "#dashboard", Expected: "Dashboard authenticated"}}})
	if trace["event"] != "trace_meta" || trace["size"].(float64) < 4 {
		t.Fatalf("authenticated trace unavailable: %+v", trace)
	}
	traceVersion := "sha256:" + trace["sha256"].(string)
	traceWindow := send(map[string]any{"type": "trace_read", "nonce": fmt.Sprintf("%032x", 109),
		"file_version": traceVersion, "offset": 0})
	if traceWindow["event"] != "trace_chunk" || traceWindow["data"] == "" {
		t.Fatalf("authenticated trace continuation failed: %+v", traceWindow)
	}
	discardedTrace := send(map[string]any{"type": "trace_discard", "nonce": fmt.Sprintf("%032x", 110), "file_version": traceVersion})
	if discardedTrace["event"] != "trace_discarded" {
		t.Fatalf("trace discard failed: %+v", discardedTrace)
	}
	// A legitimate login can submit the secret to the registered origin and
	// then navigate to a different authorized application origin. This is
	// distinct from permitting a cross-origin credential form action.
	navigate(app.URL+"/sso", fmt.Sprintf("%032x", 111))
	ssoLogin := request(fmt.Sprintf("%032x", 112), secret)
	ssoLogin["service_endpoint"] = app.URL + "/sso"
	ssoLogin["success_selector"] = ""
	ssoLogin["success_url"] = ssoDestination.URL + "/dashboard"
	ssoResult := send(ssoLogin)
	if ssoResult["event"] != "result" || ssoResult["result"].(map[string]any)["status"] != "authenticated" {
		t.Fatalf("post-auth redirected login not confirmed: %+v", ssoResult)
	}
	ssoVisible := send(map[string]any{"type": "flow", "nonce": fmt.Sprintf("%032x", 113),
		"steps": []FlowStep{{Action: "assert_text", Selector: "#sso-dashboard", Expected: "Post-auth redirect completed"}}})
	if ssoVisible["event"] != "result" || ssoVisible["result"].(map[string]any)["ok"] != true || ssoArrivals.Load() == 0 {
		t.Fatalf("post-SSO Browser flow unusable: %+v arrivals=%d", ssoVisible, ssoArrivals.Load())
	}
	navigate(app.URL+"/retain", strings.Repeat("7", 32))
	retained := request(strings.Repeat("8", 32), secret)
	retained["service_endpoint"] = origin + "/retain"
	successRetained := send(retained)
	if successRetained["event"] != "result" {
		t.Fatalf("retained same-page login unverified: %+v", successRetained)
	}
	cleaned := send(map[string]any{"type": "flow", "nonce": strings.Repeat("9", 32),
		"steps": []FlowStep{{Action: "assert_text", Selector: "#fieldstate", Expected: "cleared"}}})
	if cleaned["event"] != "result" || cleaned["result"].(map[string]any)["ok"] != true {
		t.Fatalf("password field not wiped after successful same-page login: %+v", cleaned)
	}
	navigate(app.URL+"/rotating", strings.Repeat("0", 31)+"1")
	rotating := request(strings.Repeat("0", 31)+"2", secret)
	rotating["service_endpoint"] = origin + "/rotating"
	rotated := send(rotating)
	if rotated["event"] != "result" || rotated["result"].(map[string]any)["status"] != "authenticated" {
		t.Errorf("readiness invalidated the rendered login session: %+v", rotated)
	}
	rotatingMu.Lock()
	gets, cookieProbes, initialID, postID := rotatingGets, rotatingCookieProbes, rotatingInitialID, rotatingPostID
	rotatingMu.Unlock()
	if gets != 2 || cookieProbes != 0 || postID != initialID {
		t.Errorf("readiness cookie isolation: GETs=%d Browser-cookie probes=%d original session preserved=%t",
			gets, cookieProbes, postID == initialID)
	}
	navigate(app.URL+"/mfa", strings.Repeat("a", 32))
	mfa := request(strings.Repeat("b", 32), secret)
	mfa["service_endpoint"] = origin + "/mfa"
	mfaResult := send(mfa)
	if mfaResult["event"] != "error" || mfaResult["reason"] != "credential_manual_intervention" {
		t.Fatalf("MFA not distinguished: %+v", mfaResult)
	}
	navigate(app.URL+"/login", strings.Repeat("c", 32))
	unready := request(strings.Repeat("d", 32), secret)
	unready["service_endpoint"] = origin + "/unready"
	unavailable := send(unready)
	if unavailable["event"] != "error" || unavailable["reason"] != "credential_service_not_ready" {
		t.Fatalf("server HTTP 503 not distinguished: %+v", unavailable)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<form><input id="user"><input id="secret" type="password"><button id="submit">Sign in</button></form>`)
	}))
	navigate(dead.URL+"/login", strings.Repeat("e", 32))
	dead.Close()
	broken := request(strings.Repeat("f", 32), secret)
	broken["expected_origin"] = dead.URL
	broken["service_endpoint"] = dead.URL + "/login"
	refused := send(broken)
	if refused["event"] != "error" || refused["reason"] != "credential_service_unreachable" {
		t.Fatalf("unreachable endpoint misclassified: %+v", refused)
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
