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
