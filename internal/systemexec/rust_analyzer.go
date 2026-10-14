package systemexec

import (
	"errors"
	"path/filepath"
	"runtime"
)

const (
	RustAnalyzerVersion       = "2026-10-12"
	RustCompilerVersion       = "1.99.0"
	RustAnalyzerDir           = "/usr/local/lib/ai-server-agent/lsp/rust/2026-10-12"
	RustAnalyzerBinary        = RustAnalyzerDir + "/rust-analyzer"
	RustCompilerDir           = "/usr/local/lib/ai-server-agent/lsp/rust/toolchain/1.99.0"
	RustCompilerBinary        = RustCompilerDir + "/bin/rustc"
	RustCompilerSysrootSource = RustCompilerDir + "/lib/rustlib/src/rust/library"
)

// rust-analyzer official release compressed archives:
// amd64 040cab4e2abc490fadd95c3f6cbc60b941a78761dbc3acf0ec66fd7a97f31db3
// arm64 d7219d5d8f5537bb227b521157aebddfe7e4a90f4a85a74f31e53722f5691025
// The following hashes are of the extracted immutable executables.
var rustAnalyzerExecutables = map[string]string{
	"amd64": "fd89304c85f6c1996b036ea8f2c9ad9d3e25a197dbd713661c7fe51ec9dc8e49",
	"arm64": "3cd133963bdb0c99ec1d5c7db9615df5e97dc2149e81661bd87ee2bb72bde9f6",
}
var rustCompilerExecutables = map[string]string{
	"amd64": "f3834d26669b03f6855fa54bd2381443123e860167bc0c7a8dd137e5cf6e5e4f",
	"arm64": "b3a6b57674bb153f842dbed5de751b35fe9464768cb986f469451b6480b1ba5f",
}

// RustAnalyzerResources chooses a root-owned optional toolchain.
// No worker-controlled PATH or rustup resolution is used by Agent.
func RustAnalyzerResources() (string, string, string, error) {
	if runtime.GOOS != "linux" || rustAnalyzerExecutables[runtime.GOARCH] == "" {
		return "", "", "", errors.New("rust_lsp_unsupported_platform")
	}
	if !Trusted(RustAnalyzerBinary) || !Trusted(RustCompilerBinary) ||
		!TrustedStaticFile(filepath.Join(RustCompilerSysrootSource, "core", "src", "lib.rs")) {
		return "", "", "", errors.New("rust_lsp_untrusted_optional_resources")
	}
	if err := verifyShellNativeBinary(RustAnalyzerBinary, rustAnalyzerExecutables[runtime.GOARCH], 35<<20, 48<<20); err != nil {
		return "", "", "", errors.New("rust_lsp_analyzer_binary_integrity_invalid")
	}
	if err := verifyShellNativeBinary(RustCompilerBinary, rustCompilerExecutables[runtime.GOARCH], 500<<10, 3<<20); err != nil {
		return "", "", "", errors.New("rust_lsp_compiler_binary_integrity_invalid")
	}
	return RustAnalyzerBinary, RustCompilerBinary, RustCompilerDir, nil
}
