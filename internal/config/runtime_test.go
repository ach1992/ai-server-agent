package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeDefaultBackwardsCompatibilityAndPartialOverrides(t *testing.T) {
	cfg := Default()
	if got := cfg.EffectiveRuntime(); got.HTTPReadHeaderTimeoutSeconds != 10 || got.HTTPIdleTimeoutSeconds != 90 || got.CommandTimeoutSeconds != 300 || got.WorkspaceFileTimeoutSeconds != 30 || got.TextFallbackBytes != 32768 {
		t.Fatalf("unexpected current-production-compatible defaults: %+v", got)
	}
	cfg.Runtime = nil
	if got := cfg.EffectiveRuntime(); got != DefaultRuntimeSettings() {
		t.Fatalf("nil legacy runtime did not retain defaults: %+v", got)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	// Standalone legacy configs omitted runtime settings entirely.
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.EffectiveRuntime() != DefaultRuntimeSettings() {
		t.Fatalf("old config changed runtime behavior: %+v", legacy.EffectiveRuntime())
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	overridden := strings.Replace(string(blob), "\n}", ",\n  \"runtime\": {\"http_idle_timeout_seconds\":300,\"text_fallback_bytes\":65536}\n}", 1)
	if err := os.WriteFile(path, []byte(overridden), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.EffectiveRuntime(); got.HTTPIdleTimeoutSeconds != 300 || got.TextFallbackBytes != 65536 || got.CommandTimeoutSeconds != 300 || got.HTTPReadHeaderTimeoutSeconds != 10 {
		t.Fatalf("partial override reset unrelated defaults: %+v", got)
	}
}

func TestRuntimeLimitsAndNoSilentUnknownKeys(t *testing.T) {
	for _, spec := range RuntimeSettingSpecs() {
		t.Run(spec.Key, func(t *testing.T) {
			for _, value := range []int{spec.Minimum, spec.Maximum} {
				cfg := Default()
				override(t, cfg.Runtime, spec.Key, value)
				if err := cfg.Validate(); err != nil {
					t.Fatalf("boundary %d rejected: %v", value, err)
				}
			}
			for _, value := range []int{spec.Minimum - 1, spec.Maximum + 1} {
				cfg := Default()
				override(t, cfg.Runtime, spec.Key, value)
				if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), spec.Key) {
					t.Fatalf("unsafe %d did not reject with key: %v", value, err)
				}
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(path)
	for _, fragment := range []string{`"unknown_timeout":300`, `"command_timeout_seconds":"90"`} {
		body := strings.Replace(string(blob), `"command_timeout_seconds": 300`, `"command_timeout_seconds": 300,`+fragment, 1)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("silently accepted malformed runtime field %s", fragment)
		}
	}
}

func TestRuntimeNullCannotBypassValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := Default()
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(blob, &root); err != nil {
		t.Fatal(err)
	}
	root["runtime"] = nil
	invalidNull, _ := json.Marshal(root)
	if err := os.WriteFile(path, invalidNull, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("explicit runtime:null bypassed validation")
	}
}

func override(t *testing.T, r *RuntimeSettings, key string, value int) {
	t.Helper()
	switch key {
	case "http_read_header_timeout_seconds":
		r.HTTPReadHeaderTimeoutSeconds = value
	case "http_idle_timeout_seconds":
		r.HTTPIdleTimeoutSeconds = value
	case "command_timeout_seconds":
		r.CommandTimeoutSeconds = value
	case "workspace_file_timeout_seconds":
		r.WorkspaceFileTimeoutSeconds = value
	case "text_fallback_bytes":
		r.TextFallbackBytes = value
	default:
		t.Fatalf("new runtime key %q lacks boundary tests", key)
	}
}
