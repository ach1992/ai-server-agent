package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPHPIndexCacheRejectsSymlinksAndGroupWritablePaths(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".cache", "phpactor", "index", "agent-012345")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	if !phpPrivateCacheReady(home, dir, uid) {
		t.Fatal("private 0700 cache incorrectly rejected")
	}
	if phpPrivateCacheReady(home, dir, uid+1) {
		t.Fatal("wrong cache owner accepted")
	}
	if err := os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if phpPrivateCacheReady(home, dir, uid) {
		t.Fatal("group-writable private cache accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, dir); err != nil {
		t.Fatal(err)
	}
	if phpPrivateCacheReady(home, dir, uid) {
		t.Fatal("worker-controlled symlink cache accepted")
	}
	if phpPrivateCacheReady(home, filepath.Join(home, "..", "outside"), uid) {
		t.Fatal("parent-relative cache escape accepted")
	}
}
