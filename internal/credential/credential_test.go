package credential

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func verifier(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestStoreAuthenticateNamedPrincipal(t *testing.T) {
	direct := strings.Repeat("a", TokenHexLength)
	secondary := strings.Repeat("b", TokenHexLength)
	store := Store{
		Version: StoreVersion,
		Credentials: []Record{
			{Principal: Principal{ID: "direct-default", Class: "direct", Name: "direct/default"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: verifier(direct), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true},
			{Principal: Principal{ID: "direct-secondary", Class: "direct", Name: "direct/secondary"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: verifier(secondary), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true},
		},
	}
	if err := store.Validate(); err != nil {
		t.Fatal(err)
	}
	p, ok := store.Authenticate(secondary)
	if !ok {
		t.Fatal("secondary client credential rejected")
	}
	if p.ID != "direct-secondary" || p.Class != "direct" || p.Name != "direct/secondary" {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if _, ok := store.Authenticate(strings.Repeat("c", TokenHexLength)); ok {
		t.Fatal("unknown credential accepted")
	}
	if _, ok := store.Authenticate("not-a-token"); ok {
		t.Fatal("invalid credential format accepted")
	}
}

func TestStoreRejectsAmbiguousAndRevokedState(t *testing.T) {
	token := strings.Repeat("a", TokenHexLength)
	base := Record{Principal: Principal{ID: "direct-default", Class: "direct", Name: "direct/default"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: verifier(token), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true}
	tests := []struct {
		name  string
		store Store
	}{
		{
			name: "duplicate verifier",
			store: Store{Version: StoreVersion, Credentials: []Record{
				base,
				{Principal: Principal{ID: "other", Class: "invalid", Name: "other"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: base.Verifier, CreatedAt: base.CreatedAt, Enabled: true},
			}},
		},
		{
			name: "enabled and revoked",
			store: Store{Version: StoreVersion, Credentials: []Record{
				{Principal: base.Principal, VerifierAlgorithm: base.VerifierAlgorithm, Verifier: base.Verifier, CreatedAt: base.CreatedAt, Enabled: true, RevokedAt: "2026-10-08T01:00:00Z"},
			}},
		},
		{
			name: "no active credential",
			store: Store{Version: StoreVersion, Credentials: []Record{
				{Principal: base.Principal, VerifierAlgorithm: base.VerifierAlgorithm, Verifier: base.Verifier, CreatedAt: base.CreatedAt, Enabled: false, RevokedAt: "2026-10-08T01:00:00Z"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.store.Validate(); err == nil {
				t.Fatal("expected invalid store to fail closed")
			}
		})
	}
}

func TestLoadDoesNotRequirePlaintextBearer(t *testing.T) {
	token := strings.Repeat("d", TokenHexLength)
	path := filepath.Join(t.TempDir(), "credentials.json")
	data := `{
  "version": 1,
  "credentials": [
    {
      "principal": {"id":"direct-default","class":"direct","name":"direct/default"},
      "verifier_algorithm":"sha256-v1",
      "verifier":"` + verifier(token) + `",
      "created_at":"2026-10-08T00:00:00Z",
      "enabled":true
    }
  ]
}`
	if strings.Contains(data, token) {
		t.Fatal("test fixture unexpectedly contains plaintext token")
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := store.Authenticate(token); !ok || p.ID != "direct-default" {
		t.Fatalf("loaded store authentication failed: %+v ok=%v", p, ok)
	}
}

func TestStoreRejectsPrincipalsOutsideBoundedSchema(t *testing.T) {
	token := strings.Repeat("e", TokenHexLength)
	tests := []Record{
		{Principal: Principal{ID: "unknown", Class: "direct", Name: "unknown"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: verifier(token), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true},
		{Principal: Principal{ID: "direct-default", Class: "invalid", Name: "direct/default"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: verifier(token), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true},
	}
	for _, rec := range tests {
		store := Store{Version: StoreVersion, Credentials: []Record{rec}}
		if err := store.Validate(); err == nil {
			t.Fatalf("unexpectedly accepted principal: %+v", rec.Principal)
		}
	}
}

func TestSelectedPrincipalReplacementHasImmediateCutover(t *testing.T) {
	oldToken := strings.Repeat("1", TokenHexLength)
	newToken := strings.Repeat("2", TokenHexLength)
	store := Store{Version: StoreVersion, Credentials: []Record{{
		Principal:         Principal{ID: "direct-default", Class: "direct", Name: "direct/default"},
		VerifierAlgorithm: VerifierAlgorithm,
		Verifier:          verifier(newToken),
		CreatedAt:         "2026-10-08T00:00:00Z",
		RotatedAt:         "2026-10-08T01:00:00Z",
		Enabled:           true,
	}}}
	if err := store.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Authenticate(oldToken); ok {
		t.Fatal("old credential remained valid after immediate replacement")
	}
	if p, ok := store.Authenticate(newToken); !ok || p.ID != "direct-default" {
		t.Fatalf("replacement credential not active: principal=%+v ok=%v", p, ok)
	}
}

func TestRevokedPrincipalFailsClosedWhileAnotherPrincipalRemainsActive(t *testing.T) {
	direct := strings.Repeat("3", TokenHexLength)
	secondary := strings.Repeat("4", TokenHexLength)
	store := Store{
		Version: StoreVersion,
		Credentials: []Record{
			{Principal: Principal{ID: "direct-default", Class: "direct", Name: "direct/default"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: verifier(direct), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true},
			{Principal: Principal{ID: "direct-secondary", Class: "direct", Name: "direct/secondary"}, VerifierAlgorithm: VerifierAlgorithm, Verifier: verifier(secondary), CreatedAt: "2026-10-08T00:00:00Z", Enabled: false, RevokedAt: "2026-10-08T01:00:00Z"},
		},
	}
	if err := store.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Authenticate(secondary); ok {
		t.Fatal("revoked secondary credential remained valid")
	}
	if p, ok := store.Authenticate(direct); !ok || p.ID != "direct-default" {
		t.Fatalf("remaining direct credential rejected: principal=%+v ok=%v", p, ok)
	}
}

func TestStoreRequiresDirectDefaultPrincipalRecord(t *testing.T) {
	token := strings.Repeat("5", TokenHexLength)
	store := Store{Version: StoreVersion, Credentials: []Record{{
		Principal:         Principal{ID: "direct-secondary", Class: "direct", Name: "direct/secondary"},
		VerifierAlgorithm: VerifierAlgorithm,
		Verifier:          verifier(token),
		CreatedAt:         "2026-10-08T00:00:00Z",
		Enabled:           true,
	}}}
	if err := store.Validate(); err == nil || !strings.Contains(err.Error(), "missing the direct-default principal") {
		t.Fatalf("Validate() error = %v, want missing direct-default rejection", err)
	}
}

// A legacy second-credential record may survive an upgrade. Keeping its
// original verifier metadata parseable avoids a service-start failure, but the
// retired Gateway bearer must never authorize a request.
func TestRetiredGatewayPrincipalCannotAuthenticate(t *testing.T) {
	direct := strings.Repeat("6", TokenHexLength)
	retired := strings.Repeat("7", TokenHexLength)
	old := Record{
		Principal:         Principal{ID: "mcp-gateway", Class: "gateway", Name: "mcp-gateway"},
		VerifierAlgorithm: VerifierAlgorithm,
		Verifier:          verifier(retired), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true,
	}
	directRecord := Record{
		Principal:         Principal{ID: "direct-default", Class: "direct", Name: "direct/default"},
		VerifierAlgorithm: VerifierAlgorithm,
		Verifier:          verifier(direct), CreatedAt: "2026-10-08T00:00:00Z", Enabled: true,
	}
	s := Store{Version: StoreVersion, Credentials: []Record{directRecord, old}}
	if err := s.Validate(); err != nil {
		t.Fatalf("old store should stay readable: %v", err)
	}
	if got, ok := s.Authenticate(retired); ok {
		t.Fatalf("retired principal authenticated: %+v", got)
	}
	if got, ok := s.Authenticate(direct); !ok || got.ID != "direct-default" {
		t.Fatalf("direct credential lost during legacy compatibility: %+v, ok=%v", got, ok)
	}
	// The retired record does not count toward the minimum active credentials.
	directRecord.Enabled = false
	directRecord.RevokedAt = "2026-10-08T01:00:00Z"
	s.Credentials[0] = directRecord
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "no active credential") {
		t.Fatalf("legacy-only live bearer was incorrectly counted as usable: %v", err)
	}
}
