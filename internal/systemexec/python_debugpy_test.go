package systemexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonDebugpyPinnedTrustedOptionalRuntime(t *testing.T) {
	if p, root, err := PythonDebugpy(); err == nil {
		if !strings.HasPrefix(p, "/usr/bin/python3.") || root != PythonDebugpyModules {
			t.Fatalf("production selected unexpected Python adapter %s %s", p, root)
		}
	}
	fixture := os.Getenv("AGENT_DEBUGPY_PROOF_PACKAGE")
	if fixture == "" {
		t.Skip("optional exact verified debugpy wheel extraction fixture not provided")
	}
	if err := verifyPythonDebugpyModules(fixture, false); err != nil {
		t.Fatalf("debugpy 1.8.22 full transitive tree checksum mismatch: %v", err)
	}
	if TrustedStaticFile(filepath.Join(fixture, "debugpy", "adapter", "__main__.py")) {
		t.Fatal("worker-owned proof mistakenly passed administrator root trust")
	}
	// A byte-identical unprivileged extracted package must not acquire
	// executable adapter trust just by matching a publisher digest.
	if fixture == PythonDebugpyModules {
		t.Fatal("test-owned proof replaced production adapter path")
	}
}

func TestPythonDebugpyMissingDependenciesFailClosed(t *testing.T) {
	if err := verifyPythonDebugpyModules(t.TempDir(), false); err == nil {
		t.Fatal("empty debugpy adapter dependency tree accepted")
	}
}
