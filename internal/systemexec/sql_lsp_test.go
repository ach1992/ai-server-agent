package systemexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLLensPinnedModuleTreeAndExecutionTrust(t *testing.T) {
	if node, err := SQLLens(); err == nil && node != SQLLensNodeOptional &&
		node != BashNodeOptional && node != WebNodeOptional && node != bashNodeBrowser {
		t.Fatalf("SQL Lens unrecognized root-trusted Node %q", node)
	}
	if modules := os.Getenv("AGENT_SQL_LSP_PROOF_MODULES"); modules != "" {
		if err := verifySQLLensModules(modules, false); err != nil {
			t.Fatalf("pinned multi-dialect SQL LSP dependency tree failed: %v", err)
		}
		if err := verifySQLLensModules(modules, true); err == nil {
			t.Fatal("worker-owned SQL LSP modules incorrectly got production root trust")
		}
	}
	dir := t.TempDir()
	if err := verifySQLLensModules(dir, false); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("incomplete SQL adapter accepted: %v", err)
	}
	if err := os.Symlink("malicious", filepath.Join(dir, ".bin")); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLLensModules(dir, false); err == nil || !strings.Contains(err.Error(), "unwanted_package_bin") {
		t.Fatalf("project-controlled npm executable symlink accepted: %v", err)
	}
}
