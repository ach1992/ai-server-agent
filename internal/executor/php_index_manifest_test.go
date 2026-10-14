package executor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPHPIndexManifestBoundedSymlinkFreeAndContentFresh(t *testing.T) {
	root := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("src/Source.php", "<?php class SafeClass {}")
	write("vendor/autoload.php", "<?php echo 'no execute';")
	write("composer.json", `{"scripts":{"post-install-cmd":"touch MUST-NOT-RUN"}}`)
	external := filepath.Join(t.TempDir(), "leak.php")
	if err := os.WriteFile(external, []byte("<?php echo 'private';"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "linked.php")); err != nil {
		t.Fatal(err)
	}
	op := workspaceFileOperation{Workspace: root}
	first := workerPHPIndexManifest(op)
	if !first.OK || !strings.HasPrefix(first.FileVersion, "php-index-sha256:") {
		t.Fatalf("first manifest: %+v", first)
	}
	if err := os.WriteFile(external, []byte("<?php echo 'changed';"), 0600); err != nil {
		t.Fatal(err)
	}
	second := workerPHPIndexManifest(op)
	if !second.OK || second.FileVersion != first.FileVersion {
		t.Fatal("followed an external symlink")
	}
	target := filepath.Join(root, "src/Source.php")
	stat, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	write("src/Source.php", "<?php class FailClass {}")
	if err := os.Chtimes(target, stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}
	third := workerPHPIndexManifest(op)
	if !third.OK || third.FileVersion == second.FileVersion {
		t.Fatal("did not hash same-length source edits")
	}
	if _, err := os.Stat(filepath.Join(root, "MUST-NOT-RUN")); !os.IsNotExist(err) {
		t.Fatal("manifest executed Composer scripts")
	}
	// An oversized file is refused, not silently omitted to produce stale navigation.
	write("large.php", strings.Repeat("X", phpIndexMaxFileBytes+1))
	if got := workerPHPIndexManifest(op); got.OK || got.ErrorCode != "php_index_unbounded_or_unsafe" {
		t.Fatalf("oversized source accepted: %+v", got)
	}
}

func TestPHPIndexManifestFileCountLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= phpIndexMaxFiles; i++ {
		path := filepath.Join(root, "p", strings.Repeat("n", (i%4)+1), "unit-"+strconv.Itoa(i)+".php")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<?php"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result := workerPHPIndexManifest(workspaceFileOperation{Workspace: root})
	if result.OK || result.ErrorCode != "php_index_unbounded_or_unsafe" {
		t.Fatalf("file count unbounded: %+v", result)
	}
}
