package credential

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	StoreVersion      = 1
	VerifierAlgorithm = "sha256-v1"
	TokenHexLength    = 64
)

type Principal struct {
	ID    string `json:"id"`
	Class string `json:"class"`
	Name  string `json:"name"`
}

type Record struct {
	Principal         Principal `json:"principal"`
	VerifierAlgorithm string    `json:"verifier_algorithm"`
	Verifier          string    `json:"verifier"`
	CreatedAt         string    `json:"created_at"`
	RotatedAt         string    `json:"rotated_at,omitempty"`
	Enabled           bool      `json:"enabled"`
	RevokedAt         string    `json:"revoked_at,omitempty"`
}

type Store struct {
	Version     int      `json:"version"`
	Credentials []Record `json:"credentials"`
}

func Load(path string) (*Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read MCP credential store: %w", err)
	}
	var s Store
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse MCP credential store: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Store) Validate() error {
	if s.Version != StoreVersion {
		return fmt.Errorf("unsupported MCP credential store version %d", s.Version)
	}
	if len(s.Credentials) == 0 {
		return errors.New("MCP credential store has no credential records")
	}
	ids := make(map[string]struct{}, len(s.Credentials))
	names := make(map[string]struct{}, len(s.Credentials))
	verifiers := make(map[string]struct{}, len(s.Credentials))
	active := 0
	for i, rec := range s.Credentials {
		if strings.TrimSpace(rec.Principal.ID) == "" || strings.TrimSpace(rec.Principal.Class) == "" || strings.TrimSpace(rec.Principal.Name) == "" {
			return fmt.Errorf("MCP credential record %d has empty principal metadata", i)
		}
		if _, ok := ids[rec.Principal.ID]; ok {
			return fmt.Errorf("duplicate MCP principal id %q", rec.Principal.ID)
		}
		ids[rec.Principal.ID] = struct{}{}
		if _, ok := names[rec.Principal.Name]; ok {
			return fmt.Errorf("duplicate MCP principal name %q", rec.Principal.Name)
		}
		names[rec.Principal.Name] = struct{}{}
		if rec.VerifierAlgorithm != VerifierAlgorithm {
			return fmt.Errorf("MCP credential %q uses unsupported verifier algorithm %q", rec.Principal.ID, rec.VerifierAlgorithm)
		}
		raw, err := hex.DecodeString(rec.Verifier)
		if err != nil || len(raw) != sha256.Size {
			return fmt.Errorf("MCP credential %q has invalid verifier", rec.Principal.ID)
		}
		canonical := hex.EncodeToString(raw)
		if _, ok := verifiers[canonical]; ok {
			return fmt.Errorf("duplicate MCP credential verifier")
		}
		verifiers[canonical] = struct{}{}
		if rec.Enabled {
			if rec.RevokedAt != "" {
				return fmt.Errorf("MCP credential %q is both enabled and revoked", rec.Principal.ID)
			}
			active++
		}
	}
	if active == 0 {
		return errors.New("MCP credential store has no active credential")
	}
	return nil
}

func VerifyToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if len(token) != TokenHexLength {
		return "", errors.New("invalid MCP bearer token format")
	}
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != TokenHexLength/2 {
		return "", errors.New("invalid MCP bearer token format")
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) Authenticate(token string) (Principal, bool) {
	digest, err := VerifyToken(token)
	if err != nil {
		// Still compare a fixed-size value across every record so invalid input
		// does not select a credential-specific early-exit path.
		sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
		digest = hex.EncodeToString(sum[:])
	}
	got, _ := hex.DecodeString(digest)
	var principal Principal
	matches := 0
	for _, rec := range s.Credentials {
		want, decErr := hex.DecodeString(rec.Verifier)
		cmp := 0
		if decErr == nil && len(want) == sha256.Size {
			cmp = subtle.ConstantTimeCompare(got, want)
		}
		active := 0
		if rec.Enabled && rec.RevokedAt == "" {
			active = 1
		}
		match := cmp & active
		matches += match
		if match == 1 {
			principal = rec.Principal
		}
	}
	return principal, matches == 1 && err == nil
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	if !ok || p.ID == "" {
		return Principal{}, false
	}
	return p, true
}
