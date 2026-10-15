package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real, opt-in Rust proof through the actual four code_inspect operations
// using an unprivileged worker workspace and the existing private stdio broker.
// Neither this fixture nor production starts Cargo metadata/build/check as an
// incidental passive read. A malicious workspace rustc wrapper must stay inert.
func TestCodeInspectRealRustAnalyzerSafe(t *testing.T) {
	analyzer := os.Getenv("AGENT_RUST_PROOF_ANALYZER")
	rustc := os.Getenv("AGENT_RUST_PROOF_COMPILER")
	sysroot := os.Getenv("AGENT_RUST_PROOF_SYSROOT")
	if analyzer == "" && rustc == "" && sysroot == "" {
		t.Skip("requires real pinned Rust source analysis fixture")
	}
	if analyzer == "" || rustc == "" || sysroot == "" {
		t.Fatal("incomplete Rust fixture")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.rustAnalyzerBinary = analyzer
	s.rustCompilerBinary = rustc
	s.rustSysrootDir = sysroot
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-rust-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("file helper: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	for _, dir := range []string{"src", ".cargo"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := "pub struct Record { pub count: i32 }\npub fn double(value: i32) -> i32 { value * 2 }\npub fn run() -> i32 { double(5) }\n"
	for path, data := range map[string]string{
		"src/lib.rs":         source,
		"Cargo.toml":         "[package]\nname=\"rust-proof\"\nversion=\"0.1.0\"\nedition=\"2021\"\nbuild=\"build.rs\"\n",
		"build.rs":           "fn main(){std::fs::write(\"RUST-BUILD-EXECUTED\",\"bad\").unwrap();}\n",
		".cargo/config.toml": "[build]\nrustc=\"./untrusted-rustc.sh\"\n",
		"untrusted-rustc.sh": "#!/bin/sh\ntouch RUST-WRAPPER-EXECUTED\nexit 1\n",
		"rust-analyzer.toml": "[check]\nenable=true\noverrideCommand=[\"/bin/sh\",\"-c\",\"touch RUST-CHECK-EXECUTED\"]\n[cargo.buildScripts]\nenable=true\n[procMacro]\nenable=true\n",
	} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(workspace, "untrusted-rustc.sh"), 0700); err != nil {
		t.Fatal(err)
	}
	stat := func() Response {
		t.Helper()
		r := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "src/lib.rs"})
		if !r.OK || r.FileVersion == "" {
			t.Fatalf("worker Rust stat missing: %+v", r)
		}
		return r
	}
	first := stat()
	owner.Action, owner.Path, owner.Language, owner.FileVersion = "code_inspect", "src/lib.rs", "rust", first.FileVersion
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	call := func(method string, line, char int) Response {
		t.Helper()
		owner.CodeMethod, owner.Line, owner.Character = method, line, char
		out := s.codeInspect(ctx, owner)
		if !out.OK || out.FileVersion != owner.FileVersion {
			t.Fatalf("%s Rust semantic inspector: %+v", method, out)
		}
		return out
	}
	if out := call("symbols", 0, 0); !strings.Contains(out.Output, "Record") || !strings.Contains(out.Output, "double") {
		t.Fatalf("Rust symbol table unavailable: %s", out.Output)
	}
	if out := call("definition", 2, 24); !strings.Contains(out.Output, `"uri":"src/lib.rs"`) || !strings.Contains(out.Output, first.FileVersion) {
		t.Fatalf("Rust definition not resolved/versioned: %s", out.Output)
	}
	if out := call("references", 2, 24); !strings.Contains(out.Output, "file_version") {
		t.Fatalf("Rust references not resolved: %s", out.Output)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src/lib.rs"), []byte("pub fn broken( {\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.FileVersion = stat().FileVersion
	if out := call("diagnostics", 0, 0); !strings.Contains(out.Output, "syntax-error") {
		t.Fatalf("Rust deliberately invalid syntax unreported: %s", out.Output)
	}
	owner.FileVersion = first.FileVersion
	if bad := s.codeInspect(ctx, owner); bad.OK || bad.ErrorCode != "file_changed" {
		t.Fatalf("stale Rust source accepted: %+v", bad)
	}
	for _, marker := range []string{"RUST-BUILD-EXECUTED", "RUST-WRAPPER-EXECUTED", "RUST-CHECK-EXECUTED"} {
		if _, err := os.Stat(filepath.Join(workspace, marker)); !os.IsNotExist(err) {
			t.Fatalf("read-only rust-analyzer executed untrusted source config %s: %v", marker, err)
		}
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("Rust session leak: %d", active)
	}
}
