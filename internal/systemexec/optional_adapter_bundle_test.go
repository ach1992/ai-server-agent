package systemexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeOptionalBundleFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalAdapterFullRustAndChromiumResourceTreeCoverage(t *testing.T) {
	t.Run("rust-standard-library-source", func(t *testing.T) {
		dir := t.TempDir()
		library := filepath.Join(dir, "toolchain", "lib", "rustlib", "src", "rust", "library")
		main := filepath.Join(library, "core", "src", "lib.rs")
		other := filepath.Join(library, "alloc", "src", "collections", "vec_deque", "mod.rs")
		analyzer, rustc := filepath.Join(dir, "rust-analyzer"), filepath.Join(dir, "toolchain", "bin", "rustc")
		makeOptionalBundleFile(t, analyzer, "analyzer fixture")
		makeOptionalBundleFile(t, rustc, "compiler fixture")
		makeOptionalBundleFile(t, main, "core root")
		makeOptionalBundleFile(t, other, "non-entrypoint standard library source")
		report, err := InspectResourceTree(library, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		p := OptionalAdapterProfile{SchemaVersion: 1, Adapter: "rust-lsp", Version: "future.5",
			Artifacts: []OptionalAdapterArtifact{
				{Path: analyzer, Kind: "file", SHA256: strings.Repeat("a", 64), MaxBytes: 1000},
				{Path: rustc, Kind: "file", SHA256: strings.Repeat("b", 64), MaxBytes: 1000},
				{Path: library, Kind: "tree", SHA256: report.SHA256, FileCount: report.Files, DirCount: report.Dirs, MaxBytes: 1 << 20},
			},
			Entrypoints: map[string]string{"analyzer": analyzer, "rustc": rustc, "sysroot-source": library},
		}
		if err := validateOptionalAdapterFields("rust-lsp", p); err != nil {
			t.Fatalf("complete Rust source tree rejected: %v", err)
		}
		tree := nodeModulePolicy{label: "rust_fixture", fileCount: report.Files, dirCount: report.Dirs, maxBytes: 1 << 20, digest: report.SHA256}
		if err := verifyPinnedNodeModules(library, false, tree); err != nil {
			t.Fatalf("original Rust source rejected: %v", err)
		}
		// A legacy core/src/lib.rs-only manifest must fail structural validation.
		p.Artifacts[2] = OptionalAdapterArtifact{Path: main, Kind: "file", SHA256: strings.Repeat("c", 64), MaxBytes: 1000}
		p.Entrypoints["sysroot-source"] = main
		if err := validateOptionalAdapterFields("rust-lsp", p); err == nil || !strings.Contains(err.Error(), "complete_tree") {
			t.Fatalf("core/src/lib.rs-only Rust profile accepted: %v", err)
		}
		if err := os.WriteFile(other, []byte("tampered alloc source"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := verifyPinnedNodeModules(library, false, tree); err == nil || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("non-entrypoint Rust sysroot tamper accepted: %v", err)
		}
	})
	t.Run("chromium-runtime-bundle", func(t *testing.T) {
		dir := t.TempDir()
		chromium := filepath.Join(dir, "chromium-9999")
		binary := filepath.Join(chromium, "chrome-linux64", "chrome")
		resource := filepath.Join(chromium, "chrome-linux64", "resources.pak")
		makeOptionalBundleFile(t, binary, "chromium executable fixture")
		makeOptionalBundleFile(t, resource, "independent chromium runtime resource")
		report, err := InspectResourceTree(chromium, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		p := OptionalAdapterProfile{SchemaVersion: 1, Adapter: "javascript-browser", Version: "chromium-9999",
			Artifacts:   []OptionalAdapterArtifact{{Path: chromium, Kind: "tree", SHA256: report.SHA256, FileCount: report.Files, DirCount: report.Dirs, MaxBytes: 1 << 20}},
			Entrypoints: map[string]string{"binary": binary},
		}
		if err := validateOptionalAdapterFields("javascript-browser", p); err != nil {
			t.Fatalf("complete Chromium bundle rejected: %v", err)
		}
		tree := nodeModulePolicy{label: "chromium_fixture", fileCount: report.Files, dirCount: report.Dirs, maxBytes: 1 << 20, digest: report.SHA256}
		if err := verifyPinnedNodeModules(chromium, false, tree); err != nil {
			t.Fatalf("original Chromium bundle rejected: %v", err)
		}
		p.Artifacts = []OptionalAdapterArtifact{{Path: binary, Kind: "file", SHA256: strings.Repeat("a", 64), MaxBytes: 1000}}
		if err := validateOptionalAdapterFields("javascript-browser", p); err == nil || !strings.Contains(err.Error(), "complete_bundle") {
			t.Fatalf("executable-only Browser DAP profile accepted: %v", err)
		}
		if err := os.WriteFile(resource, []byte("tampered resource"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := verifyPinnedNodeModules(chromium, false, tree); err == nil || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("non-entrypoint Chromium resource tamper accepted: %v", err)
		}
	})
}

// Opt-in, read-only native verification: never assume these large root-owned
// sample resources are installed in ordinary CI or on arbitrary hosts.
func TestOptionalAdapterInstalledRustAndBrowserSampleResources(t *testing.T) {
	if os.Getenv("AGENT_OPTIONAL_VERIFY_NATIVE_SAMPLES") == "" {
		t.Skip("requires matching root-owned Rust and Chromium sample bundles")
	}
	for _, name := range []string{"rust-lsp", "javascript-browser"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "resources", "optional-adapters", "profiles", "linux-amd64", name+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var p OptionalAdapterProfile
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			if err := ValidateOptionalAdapterProfile(name, p); err != nil {
				t.Fatalf("native complete resource validation failed: %v", err)
			}
		})
	}
}
