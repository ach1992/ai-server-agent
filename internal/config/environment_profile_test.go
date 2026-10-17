package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentProfileValidationAndPersistence(t *testing.T) {
	cfg := Default()
	if cfg.EnvironmentProfile != "development" {
		t.Fatalf("default environment profile = %q", cfg.EnvironmentProfile)
	}
	cfg.EnvironmentProfile = "validation"
	cfg.ServerLabel = "QA آزمایش"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid purpose/Unicode label rejected: %v", err)
	}
	path := filepath.Join(t.TempDir(), "agent-config.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EnvironmentProfile != "validation" || loaded.ServerLabel != cfg.ServerLabel {
		t.Fatalf("profile round-trip changed: %q / %q", loaded.EnvironmentProfile, loaded.ServerLabel)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(original), `"environment_profile": "validation"`, `"environment_profile": "invalid"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("loaded invalid profile")
	}
}

func TestProductionPurposeRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.EnvironmentProfile = "production"
	cfg.ServerLabel = "Live API"
	path := filepath.Join(t.TempDir(), "production-config.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.EnvironmentProfile != "production" || got.ServerLabel != "Live API" {
		t.Fatalf("production purpose not persisted: %+v", got)
	}
}

func TestEnvironmentProfileRejectsInvalidValuesWithoutLosingAuthority(t *testing.T) {
	for _, profile := range []string{"", "test", "Development", "validation\n"} {
		c := Default()
		c.EnvironmentProfile = profile
		if err := c.Validate(); err == nil {
			t.Errorf("accepted invalid purpose %q", profile)
		}
	}
	for _, label := range []string{" leading", "trailing ", "one\ntwo", "x\t", strings.Repeat("a", 81), strings.Repeat("ت", 90)} {
		c := Default()
		c.ServerLabel = label
		if err := c.Validate(); err == nil {
			t.Errorf("accepted invalid label %q", label)
		}
	}
	for _, profile := range []string{"development", "validation", "production"} {
		c := Default()
		c.EnvironmentProfile = profile
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.AuthMode != "bearer" || c.WorkerUser != "aiworker" {
			t.Fatal("purpose changed the core authorization identity")
		}
	}
}
