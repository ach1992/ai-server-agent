package systemexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrustedSystemExecutable(t *testing.T) {
	if !Trusted("/usr/bin/true") {
		t.Skip("no trusted /usr/bin/true in this test image")
	}
	dir := t.TempDir()
	unsafePath := filepath.Join(dir, "executable")
	if err := os.WriteFile(unsafePath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if Trusted(unsafePath) {
		t.Fatal("untrusted worker-owned fixture accepted")
	}
	if err := os.Chmod(unsafePath, 0777); err != nil {
		t.Fatal(err)
	}
	if Trusted(unsafePath) {
		t.Fatal("writable executable accepted")
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink("/usr/bin/true", link); err != nil {
		t.Fatal(err)
	}
	if Trusted(link) {
		t.Fatal("symbolic executable accepted")
	}
	if Trusted("/usr/bin/../bin/true") || Trusted("true") {
		t.Fatal("noncanonical executable path accepted")
	}
	if result := First(unsafePath, "/usr/bin/true"); result != "/usr/bin/true" {
		t.Fatalf("failed to choose trusted binary: %q", result)
	}
}
