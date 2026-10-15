package systemexec

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBashLSPTrustedDependencyChainAndBounds(t *testing.T) {
	if selected, err := BashLSP(); err == nil && selected != BashNodeOptional &&
		selected != bashNodeBrowser {
		t.Fatalf("unexpected Bash LSP executable path: %q", selected)
	}
	modules := os.Getenv("AGENT_BASH_LSP_PROOF_MODULES")
	shellcheck := os.Getenv("AGENT_BASH_LSP_PROOF_SHELLCHECK")
	node := os.Getenv("AGENT_BASH_LSP_PROOF_NODE")
	if modules != "" {
		if err := verifyBashLSPModules(modules, false); err != nil {
			t.Fatalf("real pinned Bash npm transitive module tree proof failed: %v", err)
		}
		if err := verifyBashLSPModules(modules, true); err == nil {
			t.Fatal("worker-owned Node module tree granted production root trust")
		}
	}
	if shellcheck != "" {
		if err := verifyShellNativeBinary(shellcheck, bashShellCheckDigests[runtime.GOARCH], 1<<20, 64<<20); err != nil {
			t.Fatalf("real ShellCheck fingerprint mismatched: %v", err)
		}
		if Trusted(shellcheck) {
			t.Fatal("worker-owned ShellCheck granted root trust")
		}
	}
	if node != "" {
		if err := verifyShellNativeBinary(node, bashNodeDigests[runtime.GOARCH], 100<<20, 145<<20); err != nil {
			t.Fatalf("trusted Node v24.18.1 executable fingerprint mismatched: %v", err)
		}
	}
	empty := t.TempDir()
	if err := verifyBashLSPModules(empty, false); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("unexpected empty package tree acceptance: %v", err)
	}
	link := filepath.Join(empty, ".bin")
	if err := os.Symlink("../outside", link); err != nil {
		t.Fatal(err)
	}
	if err := verifyBashLSPModules(empty, false); err == nil ||
		!strings.Contains(err.Error(), "unwanted_package_bin") {
		t.Fatalf("symlink executable wrappers unexpectedly accepted: %v", err)
	}
}
