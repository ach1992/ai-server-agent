package systemexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebLSPTrustedTransitiveModulesAndBounds(t *testing.T) {
	if node, err := WebLSP(); err == nil && node != WebNodeOptional &&
		node != BashNodeOptional && node != bashNodeBrowser {
		t.Fatalf("unsafe Web LSP node path %q", node)
	}
	fixture := os.Getenv("AGENT_WEB_LSP_PROOF_MODULES")
	if fixture != "" {
		if err := verifyWebLSPModules(fixture, false); err != nil {
			t.Fatalf("vscode HTML/CSS real pinned npm tree rejected: %v", err)
		}
		if err := verifyWebLSPModules(fixture, true); err == nil {
			t.Fatal("worker-owned HTML/CSS Node resources acquired root trust")
		}
	}
	scratch := t.TempDir()
	if err := verifyWebLSPModules(scratch, false); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("incomplete package accepted: %v", err)
	}
	if err := os.Symlink("../../foreign", filepath.Join(scratch, ".bin")); err != nil {
		t.Fatal(err)
	}
	if err := verifyWebLSPModules(scratch, false); err == nil || !strings.Contains(err.Error(), "unwanted_package_bin") {
		t.Fatalf("dependency symlink accepted: %v", err)
	}
}
