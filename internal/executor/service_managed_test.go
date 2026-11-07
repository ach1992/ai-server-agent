package executor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ach1992/ai-server-agent/internal/managedincus"
	"os"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

func TestProtectedServiceCredentialMCPExecutorBoundary(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), InstanceID: "asa_verified_host"}
	s := &Server{cfg: cfg}
	req := Request{Action: "service_access", PrincipalID: "client-id", PrincipalClass: "direct", ServiceAccess: serviceaccess.Input{Operation: "register", Registration: serviceaccess.Registration{HostID: "spoofed", Service: "aaPanel", Endpoint: "https://localhost:8443/login", Username: "operator"}}}
	resp := s.serviceAccessAction(context.Background(), req)
	if !resp.OK {
		t.Fatalf("register: %+v", resp)
	}
	var meta serviceaccess.Metadata
	if err := json.Unmarshal([]byte(resp.Output), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.HostID != cfg.InstanceID || meta.ID == "" {
		t.Fatalf("caller spoofed host: %+v", meta)
	}
	req.ServiceAccess = serviceaccess.Input{Operation: "update", Ref: meta.ID, Registration: serviceaccess.Registration{Endpoint: "https://localhost:8443/changed-login"}}
	if r := s.serviceAccessAction(context.Background(), req); !r.OK {
		t.Fatalf("same-origin metadata update: %+v", r)
	}
	req.ServiceAccess = serviceaccess.Input{Operation: "update", Ref: meta.ID, Registration: serviceaccess.Registration{Endpoint: "https://new-origin.example/login"}}
	if r := s.serviceAccessAction(context.Background(), req); r.OK || r.ErrorCode != "approval_required" {
		t.Fatalf("cross-origin update without approval succeeded: %+v", r)
	}
	req.ServiceAccess = serviceaccess.Input{Operation: "generate", Ref: meta.ID, Kind: "password"}
	resp = s.serviceAccessAction(context.Background(), req)
	if !resp.OK {
		t.Fatalf("generate: %+v", resp)
	}
	rec, err := serviceaccess.New(cfg.StateDir).Resolve(meta.ID, cfg.InstanceID)
	if err != nil || len(rec.Secret) != 64 {
		t.Fatalf("private store unavailable: %v", err)
	}
	if strings.Contains(resp.Output, rec.Secret) || strings.Contains(resp.Output, `"secret"`) {
		t.Fatal("executor disclosed generated password")
	}
	var generated serviceaccess.Metadata
	if err := json.Unmarshal([]byte(resp.Output), &generated); err != nil {
		t.Fatal(err)
	}
	if generated.Username != "operator" {
		t.Fatalf("generate erased login user: %+v", generated)
	}
	if generated.ExpiresAt == nil {
		t.Fatal("default TTL missing")
	}
	req.ServiceAccess = serviceaccess.Input{Operation: "list"}
	listed := s.serviceAccessAction(context.Background(), req)
	if !listed.OK || strings.Contains(listed.Output, rec.Secret) {
		t.Fatalf("listed credential or failed: %+v", listed)
	}
	req.ServiceAccess = serviceaccess.Input{Operation: "revoke", Ref: meta.ID}
	if r := s.serviceAccessAction(context.Background(), req); !r.OK {
		t.Fatalf("revoke: %+v", r)
	}
	if _, err := serviceaccess.New(cfg.StateDir).Resolve(meta.ID, cfg.InstanceID); err == nil {
		t.Fatal("revoked reference resolved")
	}
	req.ServiceAccess = serviceaccess.Input{Operation: "delete", Ref: meta.ID}
	if r := s.serviceAccessAction(context.Background(), req); r.OK {
		t.Fatal("delete accepted without approval")
	}
	req.Approval = true
	if r := s.serviceAccessAction(context.Background(), req); !r.OK {
		t.Fatalf("owner-approved deletion refused: %+v", r)
	}
	req.PrincipalID = ""
	if r := s.serviceAccessAction(context.Background(), req); r.OK {
		t.Fatal("anonymous action authorized")
	}
}
func TestCredentialDerivedBrowserRevocationGate(t *testing.T) {
	s := &Server{cfg: config.Config{StateDir: t.TempDir(), InstanceID: "asa_host"}}
	st := serviceaccess.New(s.cfg.StateDir)
	meta, err := st.Register(serviceaccess.Registration{HostID: s.cfg.InstanceID, Service: "WordPress", Endpoint: "http://127.0.0.1:8080/wp-login.php"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll(meta.ID, "password", "admin", "fixture-secret", 30, false); err != nil {
		t.Fatal(err)
	}
	const sid = "stdio_0123456789abcdef0123456789abcdef"
	s.sessions = newStdioSessionBrokerForSlots(2)
	s.sessions.sessions = map[string]*stdioSession{
		sid: {id: sid, kind: "browser", ownerID: "one", ownerClass: "direct", workspace: "/authorized/worktree", browserMode: "isolated",
			browser: &browserStdioState{credentialRef: meta.ID, credentialAuthOK: true}},
	}
	s.browserAdmission.holders = map[string]string{sid: "isolated"}
	req := Request{Action: "browser_session_flow", Content: `[{"action":"assert_url","expected":"http://127.0.0.1:8080"}]`, SessionID: sid, Workspace: "/authorized/worktree", PrincipalID: "one", PrincipalClass: "direct"}
	if g := s.credentialSessionGate(req); !g.OK {
		t.Fatalf("valid credential denied: %+v", g)
	}
	req.PrincipalID = "other"
	if g := s.credentialSessionGate(req); g.OK {
		t.Fatal("session crossed principal boundary")
	}
	req.PrincipalID = "one"
	s.sessions.sessions[sid].browser.credentialAuthOK = false
	if g := s.credentialSessionGate(req); g.OK || g.ErrorCode != "browser_credential_login_unverified" {
		t.Fatalf("failed login allowed snapshot: %+v", g)
	}
	s.sessions.sessions[sid].browser.credentialAuthOK = true
	if _, err := st.Revoke(meta.ID); err != nil {
		t.Fatal(err)
	}
	if g := s.credentialSessionGate(req); g.OK {
		t.Fatal("revoked reference continued Browser flow")
	}
	req.Action = "browser_session_close"
	if g := s.credentialSessionGate(req); !g.OK {
		t.Fatal("revoke blocked safe Browser close")
	}
}
func TestCredentialLoginInputBoundToOrigin(t *testing.T) {
	valid := credentialLoginOptions{SecretSelector: "#password", SubmitSelector: "button.login", SuccessSelector: "#dashboard", SuccessURL: "https://app.example/wp-admin/"}
	if err := valid.validate("https://app.example"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []credentialLoginOptions{
		{SecretSelector: "#password", SubmitSelector: "button.login"},
		{SecretSelector: "#password", SubmitSelector: "button.login", SuccessURL: "https://evil.example/callback"},
		{SecretSelector: strings.Repeat("X", 600), SubmitSelector: "a", SuccessSelector: "#dashboard"},
	} {
		if err := c.validate("https://app.example"); err == nil {
			t.Fatalf("unsafe login options accepted: %+v", c)
		}
	}
}

func TestMissingOptionalIncusDoesNotReportMutationSuccess(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), InstanceID: "asa_verified_host"}
	s := &Server{cfg: cfg}
	req := Request{Action: "managed_environment", PrincipalID: "client-id", PrincipalClass: "direct", ManagedEnv: managedincus.Request{Operation: "create", Project: "default", Instance: "t1"}}
	// The optional native CLI is absent on standard source/test machines.
	if _, err := os.Stat("/usr/bin/incus"); err == nil {
		t.Skip("host has native Incus installed; exercise unready state on a separate fixture")
	}
	resp := s.managedEnvironmentAction(context.Background(), req)
	if resp.OK || resp.ErrorCode != "incus_unavailable" {
		t.Fatalf("missing Incus claimed a successful create: %+v", resp)
	}
	req.ManagedEnv.Operation = "status"
	resp = s.managedEnvironmentAction(context.Background(), req)
	if !resp.OK || !strings.Contains(resp.Output, "\"available\":false") {
		t.Fatalf("status should honestly report missing optional Incus: %+v", resp)
	}
}

// Protected login must leave the same fully authorized Browser usable for
// screenshots, traces, diagnostics and arbitrary Playwright, while still
// enforcing the credential's actual validity and principal/session identity.
func TestCredentialDerivedBrowserEvidenceAndScriptAvailable(t *testing.T) {
	s := &Server{cfg: config.Config{StateDir: t.TempDir(), InstanceID: "asa_host"}}
	st := serviceaccess.New(s.cfg.StateDir)
	meta, err := st.Register(serviceaccess.Registration{HostID: s.cfg.InstanceID, Service: "aaPanel", Endpoint: "http://localhost:8080/login"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll(meta.ID, "password", "admin", "synthetic-private-password", 30, false); err != nil {
		t.Fatal(err)
	}
	const sid = "stdio_0123456789abcdef0123456789abcdef"
	s.sessions = newStdioSessionBrokerForSlots(2)
	s.sessions.sessions = map[string]*stdioSession{
		sid: {id: sid, kind: "browser", ownerID: "owner", ownerClass: "direct", workspace: "/authorized/worktree", browserMode: "isolated",
			browser: &browserStdioState{credentialRef: meta.ID, credentialAuthOK: true}},
	}
	s.browserAdmission.holders = map[string]string{sid: "isolated"}
	req := Request{SessionID: sid, Workspace: "/authorized/worktree", PrincipalID: "owner", PrincipalClass: "direct"}
	for _, action := range []string{"browser_session_capture", "browser_session_image", "browser_session_trace", "browser_session_script", "browser_session_flow", "browser_session_status"} {
		req.Action = action
		req.Content = ""
		if g := s.credentialSessionGate(req); !g.OK {
			t.Fatalf("authorized %s denied after login: %+v", action, g)
		}
	}
	req.Action = "browser_session_flow"
	for _, step := range []string{"snapshot", "console", "network"} {
		req.Content = `[{"action":"` + step + `"}]`
		if g := s.credentialSessionGate(req); !g.OK {
			t.Fatalf("post-login %s evidence denied: %+v", step, g)
		}
	}
	req.Action = "browser_session_script"
	req.Content = "return await page.title()"
	req.PrincipalID = "another"
	if g := s.credentialSessionGate(req); g.OK || g.ErrorCode != "browser_session_not_found" {
		t.Fatalf("cross-principal script allowed: %+v", g)
	}
	req.PrincipalID = "owner"
	s.sessions.sessions[sid].browser.credentialAuthOK = false
	if g := s.credentialSessionGate(req); g.OK || g.ErrorCode != "browser_credential_login_unverified" {
		t.Fatalf("unverified credential unlocked script: %+v", g)
	}
	s.sessions.sessions[sid].browser.credentialAuthOK = true
	if _, err := st.Revoke(meta.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"browser_session_script", "browser_session_capture", "browser_session_flow"} {
		req.Action = action
		if g := s.credentialSessionGate(req); g.OK {
			t.Fatalf("revoked credential unlocked %s: %+v", action, g)
		}
	}
	req.Action = "browser_session_close"
	if g := s.credentialSessionGate(req); !g.OK {
		t.Fatalf("revoked credential prevented closing session: %+v", g)
	}
}

func TestNativeIdentityConflictHasTypedManagedError(t *testing.T) {
	for _, nativeError := range []string{
		"create_intent_native_uuid_conflict: original native guest has been replaced",
		"create_intent_abandoned_identity_conflict: retired operation was reused",
		"create_intent_identity_mismatch: guest operation tags differ",
	} {
		r := managedErr(errors.New(nativeError))
		if r.OK || r.ErrorCode != "identity_conflict" {
			t.Fatalf("unexpected identity error classification for %q: %+v", nativeError, r)
		}
	}
}

// Registration and private credential use are shared; Browser has the narrower
// HTTP(S) transport binding and cannot consume SSH/database/app target refs.
func TestGenericCredentialRefCannotBeSubmittedToBrowser(t *testing.T) {
	s := &Server{cfg: config.Config{StateDir: t.TempDir(), InstanceID: "asa_generic_host"}}
	st := serviceaccess.New(s.cfg.StateDir)
	meta, err := st.Register(serviceaccess.Registration{
		HostID: s.cfg.InstanceID, Service: "SSH", Endpoint: "ssh://git.example.org:22",
	})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := "PRIVATE-SYNTHETIC-DO-NOT-EXPOSE"
	if _, err = st.Enroll(meta.ID, "password", "git", sentinel, 7, false); err != nil {
		t.Fatal(err)
	}
	const sid = "stdio_0123456789abcdef0123456789abcdef"
	s.sessions = newStdioSessionBrokerForSlots(2)
	s.sessions.sessions = map[string]*stdioSession{sid: {
		id: sid, kind: "browser", ownerID: "owner", ownerClass: "direct",
		workspace: "/authorized/worktree", browserMode: "isolated", browser: &browserStdioState{},
	}}
	s.browserAdmission.holders = map[string]string{sid: "isolated"}
	req := Request{Action: "browser_session_credential_login", SessionID: sid,
		CredentialRef: meta.ID, Workspace: "/authorized/worktree",
		PrincipalID: "owner", PrincipalClass: "direct",
		Content: `{"secret_selector":"#password","submit_selector":"button","success_selector":"#app"}`,
	}
	resp := s.browserSessionCredentialLogin(context.Background(), req)
	if resp.OK || resp.ErrorCode != "credential_web_endpoint_required" || strings.Contains(resp.Error, sentinel) {
		t.Fatalf("non-HTTP target incorrectly used in Browser: %+v", resp)
	}
	if s.sessions.sessions[sid].browser.credentialRef != "" {
		t.Fatal("Browser received secret-derived state")
	}
}
