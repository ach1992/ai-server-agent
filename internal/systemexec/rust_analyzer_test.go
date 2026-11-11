package systemexec

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestRustAnalyzerPinnedNativeExecutables(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("optional Linux amd64/arm64 adapter")
	}
	root := RustCompilerDir
	if profile, configured, err := ActiveOptionalAdapterProfile("rust-lsp"); err == nil && configured {
		root = strings.TrimSuffix(profile.Entrypoints["sysroot-source"], "/lib/rustlib/src/rust/library/core/src/lib.rs")
	}
	if b, c, r, err := RustAnalyzerResources(); err == nil && (b != expectedOptionalTestEntry(t, "rust-lsp", "analyzer", RustAnalyzerBinary) ||
		c != expectedOptionalTestEntry(t, "rust-lsp", "rustc", RustCompilerBinary) || r != root) {
		t.Fatalf("unexpected production Rust resource selection: %s %s %s", b, c, r)
	}
	ra, rustc := os.Getenv("AGENT_RUST_PROOF_ANALYZER"), os.Getenv("AGENT_RUST_PROOF_COMPILER")
	if ra == "" && rustc == "" {
		return
	}
	if ra == "" || rustc == "" {
		t.Fatal("partial Rust native fixture")
	}
	if err := verifyShellNativeBinary(ra, rustAnalyzerExecutables[runtime.GOARCH], 35<<20, 48<<20); err != nil {
		t.Fatalf("real rust-analyzer checksum mismatch: %v", err)
	}
	if err := verifyShellNativeBinary(rustc, rustCompilerExecutables[runtime.GOARCH], 500<<10, 3<<20); err != nil {
		t.Fatalf("real rustc checksum mismatch: %v", err)
	}
	if Trusted(ra) || Trusted(rustc) {
		t.Fatal("worker-controlled toolchain incorrectly trusted")
	}
}
