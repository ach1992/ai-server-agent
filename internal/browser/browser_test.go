package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBrowserEnginePermissionsAllowWorkerReadExecuteWithoutWrite(t *testing.T) {
	engine := filepath.Join(t.TempDir(), "engine")
	nodeDir := filepath.Join(engine, "node")
	binDir := filepath.Join(nodeDir, "bin")
	nodePath := filepath.Join(binDir, "node")
	packagePath := filepath.Join(engine, "package.json")

	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nodeDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodePath, []byte("node"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packagePath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/bash", "-lc", browserEnginePermissionsCommand)
	cmd.Env = append(os.Environ(), "engine="+engine)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("normalize browser engine permissions: %v: %s", err, out)
	}

	assertPerm := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s permissions = %04o, want %04o", path, got, want)
		}
	}

	assertPerm(nodeDir, 0755)
	assertPerm(nodePath, 0755)
	assertPerm(packagePath, 0644)
}
