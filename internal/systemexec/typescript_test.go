package systemexec

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTypeScript7OptionalAndFailClosed(t *testing.T) {
	path, err := TypeScript7()
	if err == nil && path != filepath.Join(TypeScript7Library, "tsc") {
		t.Fatalf("trusted adapter selected unexpected path %q", path)
	}
	// A complete, correct but worker-owned package is not a production
	// Agent-owned adapter. Host trusted paths cannot be substituted by CWD.
	fixture := os.Getenv("AGENT_TS7_PROOF_BIN")
	if fixture != "" {
		library := filepath.Dir(fixture)
		if err := verifyTypeScript7Tree(library, false, typeScript7PinnedTree[runtime.GOARCH]); err != nil {
			t.Fatalf("actual pinned npm package proof failed: %v", err)
		}
		if err := verifyTypeScript7Tree(library, true, typeScript7PinnedTree[runtime.GOARCH]); err == nil {
			t.Fatal("worker-owned adapter resources received root trust")
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tsc"), []byte("not actual TS7"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyTypeScript7Tree(dir, false, typeScript7PinnedTree[runtime.GOARCH]); err == nil ||
		!strings.Contains(err.Error(), "count") {
		t.Fatalf("incomplete adapter accepted: %v", err)
	}
}
