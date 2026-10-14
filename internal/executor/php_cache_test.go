package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPHPIndexStoreRejectsSymlinksAndWorldWritablePaths(t *testing.T) {
	cache := t.TempDir()
	uid := uint32(os.Geteuid())
	if !phpIndexStoreSafe(cache, uid) {
		t.Fatal("absent index payload should be creatable")
	}
	store := filepath.Join(cache, "store")
	if err := os.Symlink(t.TempDir(), store); err != nil {
		t.Fatal(err)
	}
	if phpIndexStoreSafe(cache, uid) {
		t.Fatal("symlinked index store accepted")
	}
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store, 0755); err != nil {
		t.Fatal(err)
	}
	if !phpIndexStoreSafe(cache, uid) || phpIndexStoreSafe(cache, uid+1) {
		t.Fatal("worker-owned private-parent index rejected, or foreign owner accepted")
	}
	if err := os.Chmod(store, 0777); err != nil {
		t.Fatal(err)
	}
	if phpIndexStoreSafe(cache, uid) {
		t.Fatal("world-writable index store accepted")
	}
}

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
