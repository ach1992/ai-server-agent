package serviceaccess

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newFixture(t *testing.T) (*Store, Metadata) {
	t.Helper()
	s := New(t.TempDir())
	r, err := s.Register(Registration{HostID: "asa_testing_host", Project: "default", Guest: "guest-one", GuestUUID: "uuid-123", Service: "wordpress", Username: "testadmin", Endpoint: "http://10.10.0.12:8080/wp-login.php"})
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}
func TestLifecycleExpirationAndNoDisclosure(t *testing.T) {
	s, registered := newFixture(t)
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if registered.Status != "unenrolled" {
		t.Fatalf("unregistered status %s", registered.Status)
	}
	secret := "ONE-TIME-PRIVATE-VALUE-DO-NOT-LEAK"
	m, err := s.Enroll(registered.ID, "password", "testadmin", secret, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "ready" || m.ExpiresAt == nil || !m.ExpiresAt.Equal(now.Add(defaultTTL)) {
		t.Fatalf("default expiry %+v", m)
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "secret") {
		t.Fatalf("metadata leak: %s", b)
	}
	rec, err := s.Resolve(registered.ID, "asa_testing_host")
	if err != nil || rec.Secret != secret {
		t.Fatalf("private resolve: %v", err)
	}
	if _, err := s.Resolve(registered.ID, "another-host"); err == nil {
		t.Fatal("host mismatch authorized")
	}
	now = now.Add(14 * 24 * time.Hour)
	if err := s.MarkUsed(registered.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if !fresh[0].ExpiresAt.Equal(*m.ExpiresAt) || fresh[0].LastUsedAt == nil {
		t.Fatal("ordinary use must never renew the expiry")
	}
	now = now.Add(17 * 24 * time.Hour)
	if _, err := s.Resolve(registered.ID, "asa_testing_host"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired secret authorized: %v", err)
	}
	count, err := s.CleanupExpired()
	if err != nil || count != 1 {
		t.Fatalf("expected one expired secret: %d %v", count, err)
	}
	final, _ := s.List()
	if final[0].Status != "expired" {
		t.Fatalf("expected retained expired diagnostic, got %s", final[0].Status)
	}
	if _, err := s.Renew(registered.ID, 90, false); err == nil {
		t.Fatal("expired wiped secret cannot be silently revived by renew")
	}
	if _, err := s.Enroll(registered.ID, "password", "testadmin", secret, 120, false); err != nil {
		t.Fatal(err)
	}
	renewed, err := s.Renew(registered.ID, 0, true)
	if err != nil || renewed.ExpiresAt != nil || renewed.Status != "ready" {
		t.Fatalf("no expiry: %+v %v", renewed, err)
	}
	revoked, err := s.Revoke(registered.ID)
	if err != nil || revoked.Status != "revoked" {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, err := s.Resolve(registered.ID, "asa_testing_host"); err == nil {
		t.Fatal("revoked credential authorized")
	}
	if err := s.Delete(registered.ID); err != nil {
		t.Fatal(err)
	}
	all, err := s.List()
	if err != nil || len(all) != 0 {
		t.Fatalf("delete: %v %+v", err, all)
	}
}
func TestGeneratedCredentialOriginAndValidation(t *testing.T) {
	s, r := newFixture(t)
	if Origin(r.Endpoint) != "http://10.10.0.12:8080" {
		t.Fatal("endpoint origin mismatch")
	}
	for _, v := range []string{"https://secret@host.local/login", "ftp://host.local/path", "https://host.local/login?secret=abc", "https://host.local/#token", "javascript:alert(1)"} {
		if ValidEndpoint(v) == nil {
			t.Errorf("accepted unsafe endpoint %q", v)
		}
	}
	m, err := s.Generate(r.ID, "token", "", 7, false)
	if err != nil || m.Status != "ready" {
		t.Fatalf("generate: %+v %v", m, err)
	}
	rec, err := s.Resolve(r.ID, "asa_testing_host")
	if err != nil || len(rec.Secret) != 64 {
		t.Fatalf("generated secret: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(s.Path(), "records.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), rec.Secret) {
		t.Fatal("private credential not stored in protected file")
	}
	info, err := os.Stat(filepath.Join(s.Path(), "records.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("private store permissions are not 0600")
	}
	dirinfo, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if dirinfo.Mode().Perm() != 0700 {
		t.Fatal("private store directory permissions are not 0700")
	}
	if _, err := s.Register(Registration{HostID: "asa_testing_host", Project: "default", Guest: "guest-one", GuestUUID: "uuid-123", Service: "wordpress", Endpoint: r.Endpoint, Username: "testadmin"}); err == nil {
		t.Fatal("duplicate registered")
	}
}
func TestConcurrentRegistrationAndSafeReplacement(t *testing.T) {
	s := New(t.TempDir())
	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Register(Registration{HostID: "asa_host", Service: "service-" + string(rune('a'+i)), Endpoint: "http://test.local/login"})
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	all, err := s.List()
	if err != nil || len(all) != 16 {
		t.Fatalf("concurrent atomic store inconsistent: %d %v", len(all), err)
	}
}
func TestTamperedPermissionsAndSymlinkFailClosed(t *testing.T) {
	s, r := newFixture(t)
	path := filepath.Join(s.Path(), "records.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("world-readable store accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".real", path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(r.ID, "asa_testing_host"); err == nil {
		t.Fatal("store symlink accepted")
	}
}
func TestStoreCorruptionFailClosed(t *testing.T) {
	s, _ := newFixture(t)
	if err := os.WriteFile(filepath.Join(s.Path(), "records.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("invalid schema accepted")
	}
	if _, err := s.Register(Registration{HostID: "host", Service: "second", Endpoint: "http://test.local"}); err == nil {
		t.Fatal("corrupt store overwritten")
	}
}

func TestUpdateServiceEndpointPreservesCredentialAndGuestBinding(t *testing.T) {
	s, r := newFixture(t)
	if _, err := s.Enroll(r.ID, "password", "testadmin", "SENSITIVE-VALUE-STAYS-LOCAL", 30, false); err != nil {
		t.Fatal(err)
	}
	change := Registration{HostID: "asa_testing_host", Endpoint: "http://10.10.0.12:8080/new-login", Purpose: "new port/path"}
	updated, err := s.Modify(r.ID, change, false)
	if err != nil || updated.Endpoint != change.Endpoint {
		t.Fatalf("same-origin edit: %+v %v", updated, err)
	}
	rec, err := s.Resolve(r.ID, "asa_testing_host")
	if err != nil || rec.Secret != "SENSITIVE-VALUE-STAYS-LOCAL" {
		t.Fatalf("metadata update lost private credential: %v", err)
	}
	if _, err := s.Modify(r.ID, Registration{HostID: "asa_testing_host", Endpoint: "https://another-host.invalid/login"}, false); err == nil || !strings.Contains(err.Error(), "approval_required") {
		t.Fatalf("cross-origin edit not gated: %v", err)
	}
	changed, err := s.Modify(r.ID, Registration{HostID: "asa_testing_host", Endpoint: "http://10.20.0.8:8080/login"}, true)
	if err != nil || changed.Endpoint != "http://10.20.0.8:8080/login" {
		t.Fatalf("authorized service move failed: %+v %v", changed, err)
	}
	if _, err := s.Modify(r.ID, Registration{HostID: "asa_testing_host", GuestUUID: "different-native-uuid"}, true); err == nil {
		t.Fatal("native guest binding mutated silently")
	}
	if _, err := s.Modify(r.ID, Registration{HostID: "different-host", Endpoint: "http://localhost/login"}, true); err == nil {
		t.Fatal("host credential binding mutated silently")
	}
}

// The same WordPress/aaPanel endpoint may legitimately have administrator,
// editor, API and other separate credentials. Only exact duplicate account
// identity is rejected, not the host/service/URL itself.
func TestMultipleServiceAccountsSameEndpoint(t *testing.T) {
	s, first := newFixture(t)
	editor, err := s.Register(Registration{HostID: first.HostID, Project: first.Project, Guest: first.Guest, GuestUUID: first.GuestUUID, Service: first.Service, Endpoint: first.Endpoint, Username: "editor", Purpose: "role-testing"})
	if err != nil {
		t.Fatalf("second service account blocked: %v", err)
	}
	if editor.ID == first.ID {
		t.Fatal("separate accounts reused identical secret reference")
	}
	if _, err := s.Enroll(editor.ID, "password", "editor", "editor-only-password", 30, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(first.ID, "password", "testadmin", "admin-only-password", 30, false); err != nil {
		t.Fatal(err)
	}
	admin, err := s.Resolve(first.ID, first.HostID)
	if err != nil || admin.Secret != "admin-only-password" {
		t.Fatal("admin credential changed by editor registration")
	}
	other, err := s.Resolve(editor.ID, first.HostID)
	if err != nil || other.Secret != "editor-only-password" {
		t.Fatal("editor credential changed by admin registration")
	}
	if _, err := s.Register(Registration{HostID: first.HostID, Project: first.Project, Guest: first.Guest, GuestUUID: first.GuestUUID, Service: first.Service, Endpoint: first.Endpoint, Username: "editor", Purpose: "role-testing"}); err == nil {
		t.Fatal("identical duplicate account allowed")
	}
}

// Credential records identify application targets independently of how the
// authorized consumer eventually uses them. No protocol/provider is promoted
// to a privileged secret retrieval API by registering its URI.
func TestGenericTargetSchemesShareProtectedCredentialLifecycle(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.example.org/v1", "ssh://git.example.org:22",
		"postgres://db.example.org:5432/reports", "mysql://db.internal:3306/app",
		"app://local/authorized-service", "grpcs://api.example.org:443",
	} {
		t.Run(endpoint, func(t *testing.T) {
			st := New(t.TempDir())
			meta, err := st.Register(Registration{
				HostID: "asa_generic_host", Service: "service", Endpoint: endpoint,
				Purpose: "authorized application integration",
			})
			if err != nil || meta.Endpoint != endpoint || meta.Status != "unenrolled" {
				t.Fatalf("register: %+v %v", meta, err)
			}
			const secret = "PRIVATE-SYNTHETIC-CREDENTIAL-DO-NOT-EXPOSE"
			enrolled, err := st.Enroll(meta.ID, "token", "", secret, 3, false)
			if err != nil || enrolled.Status != "ready" {
				t.Fatalf("enroll: %+v %v", enrolled, err)
			}
			encoded, _ := json.Marshal(enrolled)
			if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "\"secret\"") {
				t.Fatal("credential disclosed through generic metadata")
			}
			rec, err := st.Resolve(meta.ID, "asa_generic_host")
			if err != nil || rec.Secret != secret || rec.Endpoint != endpoint {
				t.Fatalf("private resolve: %v", err)
			}
			if _, err := st.Resolve(meta.ID, "another-host"); err == nil {
				t.Fatal("host-bound ref authorized elsewhere")
			}
			if _, err := st.Revoke(meta.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Resolve(meta.ID, "asa_generic_host"); err == nil {
				t.Fatal("revoked ref still usable")
			}
		})
	}
}
func TestGenericTargetURIRejectsSecretBearingAndLocalScheme(t *testing.T) {
	for _, v := range []string{
		"", "javascript:alert(1)", "file:///tmp/secret", "file://host/tmp/secret",
		"ssh://admin:password@host:22", "ssh://user@host", "ssh://host?token=abc",
		"postgres://db.example/app#token", "../relative-path", "mailto:admin@example.org",
		"ssh:///missing-host", "ssh://host\nother", "data://host/object",
	} {
		if err := ValidServiceTarget(v); err == nil {
			t.Errorf("accepted secret-bearing or unsafe target %q", v)
		}
	}
	for _, v := range []string{"ssh://host:22", "postgres://db.example:5432/app", "app://local/service"} {
		if err := ValidServiceTarget(v); err != nil {
			t.Errorf("generic target refused: %q: %v", v, err)
		}
		if err := ValidEndpoint(v); err == nil {
			t.Errorf("browser HTTP validator accepted nonweb target %q", v)
		}
	}
}

// A manual credential import must commit both the reference and material
// together, so a failed or interrupted one-command action leaves no visible
// empty reference for a future AI conversation to mistake for a ready login.
func TestRegisterWithSecretAtomicAndGenericKinds(t *testing.T) {
	st := New(t.TempDir())
	reg := Registration{HostID: "asa_fixture", Service: "service-access-test",
		Endpoint: "ssh://git.example.org:22", Purpose: "operator-import"}
	key := "-----BEGIN PRIVATE KEY-----\nSYNTHETIC-TEST-KEY-NOT-REAL\n-----END PRIVATE KEY-----\n"
	if _, err := st.RegisterWithSecret(reg, "private_key", "", 30, false); err == nil {
		t.Fatal("empty imported secret accepted")
	}
	if _, err := st.RegisterWithSecret(reg, "invalid kind/with space", "fake", 30, false); err == nil {
		t.Fatal("unsupported metadata kind accepted")
	}
	if _, err := st.RegisterWithSecret(reg, "private_key", key, -1, false); err == nil {
		t.Fatal("invalid TTL accepted")
	}
	empty, err := st.List()
	if err != nil || len(empty) != 0 {
		t.Fatalf("failed registrations retained partial state: %+v %v", empty, err)
	}
	meta, err := st.RegisterWithSecret(reg, "private_key", key, 2, false)
	if err != nil || meta.Status != "ready" || meta.Kind != "private_key" || meta.ExpiresAt == nil {
		t.Fatalf("atomic private-key registration failed: %+v %v", meta, err)
	}
	encoded, _ := json.Marshal(meta)
	if strings.Contains(string(encoded), "SYNTHETIC-TEST-KEY") || strings.Contains(string(encoded), "\"secret\"") {
		t.Fatal("secret leaked through metadata")
	}
	got, err := st.Resolve(meta.ID, reg.HostID)
	if err != nil || got.Secret != key {
		t.Fatal("multiline credential bytes not preserved in private store")
	}
	if _, err := st.RegisterWithSecret(reg, "private_key", "SECOND-SECRET", 2, false); err == nil {
		t.Fatal("duplicate registration overwrote protected credential")
	}
	again, err := st.Resolve(meta.ID, reg.HostID)
	if err != nil || again.Secret != key {
		t.Fatal("duplicate import altered previously enrolled key")
	}
	for _, kind := range []string{"api_key", "access_code", "client_secret", "certificate", "custom.text"} {
		otherReg := Registration{HostID: reg.HostID, Service: kind, Endpoint: "grpcs://api.example.net:443"}
		m, err := st.RegisterWithSecret(otherReg, kind, "SYNTHETIC-"+kind, 7, false)
		if err != nil || m.Kind != kind || m.Status != "ready" {
			t.Fatalf("generic kind %q not stored: %+v %v", kind, m, err)
		}
		if _, err := st.Resolve(m.ID, reg.HostID); err != nil {
			t.Fatalf("generic kind %q not usable by root-only resolver: %v", kind, err)
		}
	}
	if _, err := st.Generate(meta.ID, "private_key", "", 30, false); err == nil {
		t.Fatal("random token generator claimed it can issue a valid private key")
	}
	if _, err := st.Generate(meta.ID, "api_key", "", 30, false); err == nil {
		t.Fatal("random token generator claimed it can issue a provider API key")
	}
	final, err := st.Resolve(meta.ID, reg.HostID)
	if err != nil || final.Secret != key {
		t.Fatal("unsupported generation changed existing protected material")
	}
}
