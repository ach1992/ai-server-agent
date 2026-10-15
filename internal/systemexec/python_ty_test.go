package systemexec

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestNativePythonTyIntegrityAndRootTrust(t *testing.T) {
	if selected, err := TyPython(); err == nil && selected != PythonTyBinary {
		t.Fatalf("unexpected Python adapter selection: %s", selected)
	}
	local := os.Getenv("AGENT_TY_PROOF_BIN")
	if local != "" {
		if err := verifyTyPythonBinary(local, pythonTyBinarySha[runtime.GOARCH]); err != nil {
			t.Fatalf("real Python ty 0.0.87 wheel proof failed: %v", err)
		}
		if Trusted(local) {
			t.Fatal("worker-managed ty proof mistakenly trusted for production")
		}
	}
	truncated := t.TempDir() + "/ty"
	if err := os.WriteFile(truncated, []byte("untrusted source"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyTyPythonBinary(truncated, pythonTyBinarySha[runtime.GOARCH]); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Fatalf("untrusted/unpinned ty binary accepted: %v", err)
	}
}
