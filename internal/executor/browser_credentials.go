package executor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/managedincus"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

type credentialLoginOptions struct {
	UsernameSelector   string `json:"username_selector,omitempty"`
	SecretSelector     string `json:"secret_selector"`
	SubmitSelector     string `json:"submit_selector"`
	SuccessSelector    string `json:"success_selector,omitempty"`
	SuccessURL         string `json:"success_url,omitempty"`
	ChallengeSelector  string `json:"challenge_selector,omitempty"`
	LoginErrorSelector string `json:"login_error_selector,omitempty"`
}

// Never let client-supplied selectors or service URLs contain a secret or
// executable code. CSS is only used as Playwright locator input. The secret
// itself only enters the protected executor and the pinned Browser's stdin.
func (a credentialLoginOptions) validate(origin string) error {
	for _, v := range []string{a.UsernameSelector, a.SecretSelector, a.SubmitSelector, a.SuccessSelector, a.ChallengeSelector, a.LoginErrorSelector} {
		if len(v) > 512 || strings.IndexFunc(v, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return errors.New("invalid or oversized CSS selector")
		}
	}
	if a.SecretSelector == "" || a.SubmitSelector == "" || (a.SuccessSelector == "" && a.SuccessURL == "") {
		return errors.New("secret_selector, submit_selector and success_selector or success_url are required")
	}
	if a.SuccessURL != "" {
		if err := serviceaccess.ValidEndpoint(a.SuccessURL); err != nil {
			return errors.New("success_url must be an absolute HTTP(S) URL without secrets")
		}
		if serviceaccess.Origin(a.SuccessURL) != origin {
			return errors.New("success URL must have the same origin as the enrolled endpoint")
		}
	}
	return nil
}
func (s *Server) browserSessionCredentialLogin(ctx context.Context, req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	if entry.browserMode != "isolated" {
		return browserSessionError("isolated_browser_required", "security", errors.New("credential-derived Browser login requires an isolated profile so session cookies are not shared with another principal"))
	}
	if req.CredentialRef == "" || len(req.Content) == 0 || len(req.Content) > 4096 {
		return browserSessionError("invalid_credential_login", "validation", errors.New("service reference and bounded non-secret login parameters required"))
	}
	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	if state.uncertain {
		return browserSessionError("browser_session_uncertain", "state", errors.New("close uncertain Browser session before further use"))
	}
	if state.credentialAuthOK {
		return browserSessionError("browser_already_authenticated", "state", errors.New("re-use the authenticated session or open a fresh isolated session"))
	}
	if state.credentialRef != "" && state.credentialRef != req.CredentialRef {
		return browserSessionError("browser_credential_binding_conflict", "authorization", errors.New("use a fresh isolated Browser session for a different credential"))
	}
	rec, err := serviceaccess.New(s.cfg.StateDir).Resolve(req.CredentialRef, s.cfg.InstanceID)
	if err != nil {
		return managedErr(err)
	}
	if rec.Guest != "" {
		ready := s.managedBackend().Ready(ctx)
		if !ready.Available {
			return browserSessionError("credential_guest_identity_unavailable", "dependency", errors.New("Incus is not ready on this Agent host; restore native access before credential login"))
		}
		target, err := s.managedBackend().Run(ctx, managedincus.Request{Operation: "inspect", Project: rec.Project, Instance: rec.Guest}, false, "")
		if err != nil || target.Instance == nil || !target.Instance.Managed || target.Instance.UUID != rec.GuestUUID {
			return browserSessionError("credential_guest_identity_mismatch", "authorization", errors.New("native managed guest identity changed or unavailable; reconcile guest identity before Browser login"))
		}
	}
	var opts credentialLoginOptions
	if err := json.Unmarshal([]byte(req.Content), &opts); err != nil {
		return browserSessionError("invalid_credential_login", "validation", errors.New("invalid login parameters"))
	}
	origin := serviceaccess.Origin(rec.Endpoint)
	if err := opts.validate(origin); err != nil {
		return browserSessionError("invalid_credential_login", "validation", err)
	}
	if opts.UsernameSelector != "" && rec.Username == "" {
		return browserSessionError("username_unavailable", "state", errors.New("record has no username; update via local enrollment"))
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return browserSessionError("browser_nonce_unavailable", "state", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	// Mark this session as credential-derived BEFORE sending any secret. A lost
	// acknowledgement must never permit unrestricted later access after revoke.
	state.credentialRef = req.CredentialRef
	state.credentialAuthOK = false
	payload := map[string]any{
		"type": "credential_login", "nonce": nonce, "expected_origin": origin, "username": rec.Username,
		"secret":            rec.Secret,
		"username_selector": opts.UsernameSelector, "secret_selector": opts.SecretSelector,
		"submit_selector": opts.SubmitSelector, "success_selector": opts.SuccessSelector,
		"success_url": opts.SuccessURL, "challenge_selector": opts.ChallengeSelector,
		"login_error_selector": opts.LoginErrorSelector,
		"service_endpoint":     rec.Endpoint,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return browserSessionError("invalid_credential_login", "state", err)
	}
	// Keep the single broker-frame limit rather than expanding protocol buffers
	// for secrets; never include this frame or its contents in audit/log/error.
	if len(encoded) > 16000 {
		return browserSessionError("credential_login_frame_too_large", "resource", errors.New("login parameters exceed private Browser frame budget"))
	}
	if err := s.workerStdioWrite(req, entry.id, append(encoded, '\n')); err != nil {
		state.uncertain = true
		return browserSessionError("credential_login_uncertain", "state", errors.New("Browser credential delivery outcome uncertain; close session before retry"))
	}
	frameCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	frame, err := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
	if err != nil {
		state.uncertain = true
		return browserSessionError("credential_login_uncertain", "state", errors.New("Browser credential use completion unverified; close session before retry"))
	}
	if frame.Event == "error" {
		// The pinned runner returns only bounded reasons. Never proxy raw page or
		// JavaScript errors, fields or URL/query data from this secret path.
		reason := frame.Reason
		switch reason {
		case "credential_origin_mismatch", "credential_form_origin_mismatch", "credential_manual_intervention", "credential_invalid_login", "credential_service_unreachable", "credential_service_not_ready", "credential_login_not_confirmed":
		default:
			reason = "credential_login_failed"
		}
		return browserSessionError(reason, "action", errors.New(reason+"; inspect login prerequisites, credentials and MFA without posting secrets in chat"))
	}
	if frame.Event != "result" {
		state.uncertain = true
		return browserSessionError("credential_login_uncertain", "state", errors.New("unexpected Browser result"))
	}
	var outcome struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(frame.Result, &outcome); err != nil || !outcome.OK || outcome.Status != "authenticated" {
		state.uncertain = true
		return browserSessionError("credential_login_uncertain", "state", errors.New("invalid Browser login confirmation"))
	}
	state.credentialAuthOK = true
	_ = serviceaccess.New(s.cfg.StateDir).MarkUsed(req.CredentialRef)
	return Response{OK: true, Status: "authenticated", SessionID: entry.id, OutputEncoding: "json", Output: `{"status":"authenticated","credential_ref":"` + req.CredentialRef + `"}`}
}

// A bounded action may still return page-supplied URLs/assertion output.
// Reject responses containing the stored secret or common escaping forms;
// never return the offending bytes to the MCP client.
func (s *Server) credentialFlowEvidenceGate(req Request, resp Response) Response {
	if resp.Output == "" {
		return resp
	}
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return resp
	}
	entry.browser.mu.Lock()
	ref := entry.browser.credentialRef
	entry.browser.mu.Unlock()
	if ref == "" {
		return resp
	}
	rec, err := serviceaccess.New(s.cfg.StateDir).Resolve(ref, s.cfg.InstanceID)
	if err != nil {
		return managedErr(err)
	}
	secret := rec.Secret
	variants := []string{secret, url.QueryEscape(secret), url.PathEscape(secret),
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawStdEncoding.EncodeToString([]byte(secret))}
	escaped, _ := json.Marshal(secret)
	if len(escaped) > 2 {
		variants = append(variants, string(escaped[1:len(escaped)-1]))
	}
	containsSecret := func(value string) bool {
		for _, v := range variants {
			if v != "" && strings.Contains(value, v) {
				return true
			}
		}
		return false
	}
	leaked := containsSecret(resp.Output)
	// JSON escaping can hide a password from raw substring searches.
	// Traverse actual decoded string contents, not only their wire encoding.
	if !leaked {
		var decoded any
		if json.Unmarshal([]byte(resp.Output), &decoded) == nil {
			var visit func(any) bool
			visit = func(node any) bool {
				switch x := node.(type) {
				case string:
					return containsSecret(x)
				case map[string]any:
					for k, v := range x {
						if containsSecret(k) || visit(v) {
							return true
						}
					}
				case []any:
					for _, v := range x {
						if visit(v) {
							return true
						}
					}
				}
				return false
			}
			leaked = visit(decoded)
		}
	}
	if leaked {
		return browserSessionError("credential_browser_evidence_blocked", "security", errors.New("page action result contained protected credential material; no Browser evidence delivered"))
	}
	return resp
}

// Applies to all established credential-derived Browser sessions, not just
// the login tool. Expired/revoked credentials never unlock further browser
// flow, screenshots, traces or navigation via Agent-managed session handles.
func (s *Server) credentialSessionGate(req Request) Response {
	if req.Action == "browser_session_open" || req.Action == "browser_session_close" || req.Action == "browser_session_credential_login" {
		return Response{OK: true}
	}
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	state := entry.browser
	state.mu.Lock()
	ref := state.credentialRef
	authenticated := state.credentialAuthOK
	state.mu.Unlock()
	if ref != "" && !authenticated {
		return browserSessionError("browser_credential_login_unverified", "state", errors.New("credential login not verified; close this isolated Browser session before further inspection"))
	}
	if ref == "" {
		return Response{OK: true}
	}
	_, err = serviceaccess.New(s.cfg.StateDir).Resolve(ref, s.cfg.InstanceID)
	if err != nil {
		return managedErr(fmt.Errorf("credential-derived Browser access denied: %w", err))
	}
	// The original login request may remain in the page's DOM, JavaScript
	// memory or network history. There is no reliable generic way to scrub
	// pixel, ZIP, network and accessibility evidence from an arbitrary
	// third-party application. Keep bounded UI actions/assertions usable,
	// but never export raw evidence from a secret-bearing Browser session.
	switch req.Action {
	case "browser_session_capture", "browser_session_image", "browser_session_trace":
		return browserSessionError("credential_browser_evidence_unavailable", "security", errors.New("raw Browser screenshots, images and traces are disabled for protected-login sessions; use bounded UI assertions"))
	case "browser_session_flow":
		var steps []struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal([]byte(req.Content), &steps); err != nil {
			return browserSessionError("invalid_browser_flow", "validation", errors.New("invalid protected Browser steps"))
		}
		for _, step := range steps {
			if step.Action == "snapshot" || step.Action == "console" || step.Action == "network" {
				return browserSessionError("credential_browser_evidence_unavailable", "security", errors.New("raw snapshots, network events and console logs are disabled for protected-login sessions; use click/fill/assert/goto"))
			}
		}
	}
	return Response{OK: true}
}
