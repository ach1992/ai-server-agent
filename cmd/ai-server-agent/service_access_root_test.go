package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

// A real root-only file import is run only on an explicitly approved Test
// host. No real Agent, service, VM or control-plane credential is touched.
func TestOperatorLocalProtectedCredentialFileAcceptance(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("ASA_SERVICE_ACCESS_ROOT_SMOKE") != "1" {
		t.Skip("requires explicit root Test-host opt-in")
	}
	root, err := os.MkdirTemp("/root", "asa-issue65-credential-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "private-key.txt")
	secret := "-----BEGIN PRIVATE KEY-----\nSYNTHETIC-NONPRODUCTION-KEY\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(input, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{InstanceID: "asa_" + strings.Repeat("a", 32), StateDir: state}
	args := []string{"add", "--service", "synthetic-ssh", "--endpoint",
		"ssh://fixture.example:22", "--kind", "private_key",
		"--secret-file", input, "--ttl-days", "1"}
	if err := enrollServiceCredential(cfg, args); err != nil {
		t.Fatal(err)
	}
	store := serviceaccess.New(state)
	entries, err := store.List()
	if err != nil || len(entries) != 1 || entries[0].Status != "ready" ||
		entries[0].Kind != "private_key" {
		t.Fatalf("one-command import did not create exactly one ready ref: %+v %v", entries, err)
	}
	record, err := store.Resolve(entries[0].ID, cfg.InstanceID)
	if err != nil || record.Secret != secret {
		t.Fatal("root-only importer failed to preserve exact multiline material")
	}
	if err := enrollServiceCredential(cfg, args); err == nil {
		t.Fatal("duplicate add silently overwrote an existing service")
	}
	if err := os.Chmod(input, 0644); err != nil {
		t.Fatal(err)
	}
	if err := enrollServiceCredential(cfg, append([]string{"enroll", "--ref", entries[0].ID},
		"--kind", "api_key", "--secret-file", input)); err == nil {
		t.Fatal("world-readable secret file was accepted")
	}
	if err := os.Chmod(input, 0600); err != nil {
		t.Fatal(err)
	}
	if err := enrollServiceCredential(cfg, []string{"enroll", "--ref", entries[0].ID,
		"--kind", "api_key", "--secret-file", input, "--ttl-days", "1"}); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.Resolve(entries[0].ID, cfg.InstanceID)
	if err != nil || rotated.Secret != secret || rotated.Kind != "api_key" {
		t.Fatal("private rotation failed to preserve reference and exact value")
	}
	if _, err := store.Revoke(entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(entries[0].ID, cfg.InstanceID); err == nil {
		t.Fatal("revoked imported credential was still usable")
	}
}
