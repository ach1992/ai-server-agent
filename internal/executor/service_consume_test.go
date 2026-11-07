package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

func serviceFixture(t *testing.T, endpoint, kind, user, secret string) (*Server, string) {
	t.Helper()
	s := newSyncOutputTestServer(t)
	s.cfg.InstanceID = "asa_service_test"
	store := serviceaccess.New(s.cfg.StateDir)
	meta, err := store.Register(serviceaccess.Registration{HostID: s.cfg.InstanceID, Service: "test", Endpoint: endpoint, Username: user})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enroll(meta.ID, kind, user, secret, 30, false); err != nil {
		t.Fatal(err)
	}
	return s, meta.ID
}
func useService(s *Server, ref string, in ServiceUseSpec) Response {
	return s.serviceUseAction(context.Background(), Request{PrincipalID: "caller", PrincipalClass: "direct", CredentialRef: ref, ServiceUse: in})
}
func TestServiceHTTPRegisteredOriginNoRedirectAndRevocation(t *testing.T) {
	const key = "hidden-token-test"
	var leak atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leak.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credential forwarded across origin")
		}
	}))
	defer other.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("wrong bearer token: %q", got)
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL+"/steal", 302)
			return
		}
		fmt.Fprint(w, "prefix "+key+" suffix")
	}))
	defer app.Close()
	s, ref := serviceFixture(t, app.URL+"/api", "token", "", key)
	got := useService(s, ref, ServiceUseSpec{Path: "/status"})
	if !got.OK || got.Output != "prefix [REDACTED] suffix" {
		t.Fatalf("HTTP output/secret failure: %+v", got)
	}
	redirect := useService(s, ref, ServiceUseSpec{Path: "/redirect"})
	if leak.Load() != 0 || redirect.Status != "http_302" || redirect.OK {
		t.Fatalf("cross-origin redirect followed: %+v", redirect)
	}
	if bad := useService(s, ref, ServiceUseSpec{Path: "//other.invalid/leak"}); bad.OK || bad.ErrorCode != "invalid_http_path" {
		t.Fatalf("invalid path accepted: %+v", bad)
	}
	if _, err := serviceaccess.New(s.cfg.StateDir).Revoke(ref); err != nil {
		t.Fatal(err)
	}
	if revoked := useService(s, ref, ServiceUseSpec{Path: "/status"}); revoked.OK {
		t.Fatalf("revoked secret reused: %+v", revoked)
	}
}
func TestServiceHTTPNonAllowlistedMethodAndLargeAuthorizedBody(t *testing.T) {
	const bodySize = 300 << 10
	const token = "registered-api-credential"
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Accept") != "application/vnd.custom+json" {
			t.Errorf("legitimate API method/auth rejected: %s", r.Method)
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil || len(payload) != bodySize {
			t.Errorf("body size changed: %d, err %v", len(payload), err)
		}
		fmt.Fprint(w, "authorized")
	}))
	defer service.Close()
	s, ref := serviceFixture(t, service.URL, "opaque_vendor_kind", "", token)
	ok := useService(s, ref, ServiceUseSpec{Method: "PROPFIND", Body: strings.Repeat("x", bodySize), AuthMode: "bearer", Headers: map[string]string{"Accept": "application/vnd.custom+json"}})
	if !ok.OK || ok.Output != "authorized" {
		t.Fatalf("arbitrary authorized HTTP method/body rejected: %+v", ok)
	}
	if forbidden := useService(s, ref, ServiceUseSpec{Headers: map[string]string{"Authorization": "Bearer untrusted-override"}}); forbidden.OK || forbidden.ErrorCode != "invalid_http_header" {
		t.Fatalf("HTTP auth override accepted: %+v", forbidden)
	}
	if invalid := useService(s, ref, ServiceUseSpec{Method: "bad method"}); invalid.OK || invalid.ErrorCode != "invalid_http_request" {
		t.Fatalf("invalid HTTP method accepted: %+v", invalid)
	}
}

func TestServiceHTTPBasicAndAPIKey(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/basic" {
			user, pwd, ok := r.BasicAuth()
			if !ok || user != "admin" || pwd != "pass-example" {
				t.Error("basic auth not sent")
			}
		} else if r.Header.Get("X-API-Key") != "test-api-key" {
			t.Error("API key not sent")
		}
		fmt.Fprint(w, "ok")
	}))
	defer app.Close()
	s, ref := serviceFixture(t, app.URL, "password", "admin", "pass-example")
	if r := useService(s, ref, ServiceUseSpec{Path: "/basic"}); !r.OK {
		t.Fatalf("basic failed: %+v", r)
	}
	s2, ref2 := serviceFixture(t, app.URL, "api_key", "", "test-api-key")
	if r := useService(s2, ref2, ServiceUseSpec{Path: "/key"}); !r.OK {
		t.Fatalf("API key failed: %+v", r)
	}
	if r := useService(s2, ref2, ServiceUseSpec{AuthHeader: "Host"}); r.OK || r.ErrorCode != "invalid_service_auth" {
		t.Fatalf("invalid header accepted: %+v", r)
	}
}
func TestServiceHTTPLargeBodyIsRecoverableAndSecretRedacted(t *testing.T) {
	const key = "secret-high-volume"
	raw := bytes.Repeat([]byte("hello "), 200000)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
		_, _ = w.Write([]byte(key + "TAIL"))
	}))
	defer app.Close()
	s, ref := serviceFixture(t, app.URL, "token", "", key)
	got := useService(s, ref, ServiceUseSpec{})
	if !got.OK || got.JobID == "" || !got.Truncated {
		t.Fatalf("large output cannot resume: %+v", got)
	}
	var restored []byte
	offset := int64(0)
	for {
		part := s.jobOutputBounded(Request{JobID: got.JobID, Offset: offset, Limit: 4096})
		if !part.OK || part.NextOffset == nil || *part.NextOffset <= offset {
			t.Fatalf("bad continuation at %d: %+v", offset, part)
		}
		chunk := []byte(part.Output)
		if part.OutputEncoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(part.Output)
			if err != nil {
				t.Fatal(err)
			}
			chunk = decoded
		}
		restored = append(restored, chunk...)
		offset = *part.NextOffset
		if part.EOF != nil && *part.EOF {
			break
		}
	}
	expected := append(append([]byte(nil), raw...), []byte("[REDACTED]TAIL")...)
	if !bytes.Equal(restored, expected) {
		t.Fatalf("lost or exposed data: got %d want %d", len(restored), len(expected))
	}
}
func TestServiceOutputRedactionAcrossWrites(t *testing.T) {
	var b bytes.Buffer
	r := credentialOutputRedactor{to: &b, secret: []byte("splitsecret")}
	for _, p := range []string{"a split", "secr", "et z splitse", "cret b"} {
		if _, err := r.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if b.String() != "a [REDACTED] z [REDACTED] b" {
		t.Fatalf("split secret not redacted: %q", b.String())
	}
}
func nativeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture-client")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func noServiceTemps(t *testing.T, s *Server) {
	t.Helper()
	files, err := os.ReadDir(serviceaccess.New(s.cfg.StateDir).Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".asa-") {
			t.Fatalf("left credential temp %s", f.Name())
		}
	}
}
func TestServiceSSHNativeKeyConsumer(t *testing.T) {
	const key = "private-key-fixture-content"
	s, ref := serviceFixture(t, "ssh://server.example:2222", "private_key", "operator", key)
	s.serviceSSHBinary = nativeScript(t, "f=''\np=''\nfor arg in \"$@\"; do if [ \"$p\" = '-i' ]; then f=\"$arg\"; fi; p=\"$arg\"; done\n[ -n \"$f\" ]\n[ \"$(stat -c %a \"$f\")\" = '600' ]\ncat \"$f\"")
	r := useService(s, ref, ServiceUseSpec{Command: "id"})
	if !r.OK || strings.Contains(r.Output, key) || !strings.Contains(r.Output, "[REDACTED]") {
		t.Fatalf("SSH native key failed or leaked: %+v", r)
	}
	noServiceTemps(t, s)
}
func TestServicePostgresNativeQueryAndPrivateEnv(t *testing.T) {
	const key = "postgres-private"
	s, ref := serviceFixture(t, "postgresql://db.example:5432/inventory", "password", "dbadmin", key)
	s.servicePSQLBinary = nativeScript(t, "[ \"$PGUSER\" = 'dbadmin' ]\nread query\nprintf '%s %s\\n' \"$query\" \"$PGPASSWORD\"")
	r := useService(s, ref, ServiceUseSpec{Query: "select 4;"})
	if !r.OK || r.Output != "select 4; [REDACTED]\n" {
		t.Fatalf("Postgres consumer failed: %+v", r)
	}
}
func TestServiceSSHPasswordUsesOptionalNativeClient(t *testing.T) {
	const pwd = "ssh-password-private"
	s, ref := serviceFixture(t, "ssh://server.example:2222", "password", "operator", pwd)
	s.serviceSSHBinary = nativeScript(t, "exit 0")
	s.serviceSSHPassBinary = nativeScript(t, "[ \"$1\" = '-e' ]\n[ \"$SSHPASS\" = 'ssh-password-private' ]\nprintf 'sshpass %s\\n' \"$SSHPASS\"")
	r := useService(s, ref, ServiceUseSpec{Command: "id"})
	if !r.OK || r.Output != "sshpass [REDACTED]\n" || strings.Contains(r.Output, pwd) {
		t.Fatalf("private password SSH consumer failed: %+v", r)
	}
}

func TestServiceHTTPAuthModeIndependentOfKindLabel(t *testing.T) {
	const key = "credential-custom-label"
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture-Authorization") != key {
			t.Error("custom header with arbitrary credential kind missing")
		}
		fmt.Fprint(w, "authorized")
	}))
	defer app.Close()
	s, ref := serviceFixture(t, app.URL, "custom_provider_code", "", key)
	r := useService(s, ref, ServiceUseSpec{AuthMode: "header", AuthHeader: "X-Fixture-Authorization"})
	if !r.OK || r.Output != "authorized" {
		t.Fatalf("custom kind rejected: %+v", r)
	}
	bad := useService(s, ref, ServiceUseSpec{AuthMode: "unrecognized"})
	if bad.OK || bad.ErrorCode != "invalid_service_auth" {
		t.Fatalf("unsupported auth mode silently accepted: %+v", bad)
	}
}

func TestServiceMySQLNativeQueryAndPrivateOptionFile(t *testing.T) {
	s, ref := serviceFixture(t, "mysql://db.example:3307/sales", "token", "dbadmin", "mysql-private")
	s.serviceMySQLBinary = nativeScript(t, "f=$(printf %s \"$1\" | cut -d= -f2-)\n[ \"$(stat -c %a \"$f\")\" = '600' ]\ngrep -q 'password=\"mysql-private\"' \"$f\"\nread query\nprintf 'mysql %s\\n' \"$query\"")
	r := useService(s, ref, ServiceUseSpec{Query: "select 9;"})
	if !r.OK || r.Output != "mysql select 9;\n" {
		t.Fatalf("MySQL consumer failed: %+v", r)
	}
	noServiceTemps(t, s)
}
