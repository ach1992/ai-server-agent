package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Internal worker-only manifest: there is no MCP entrypoint. This runs after
// the existing workspace-helper Landlock confinement, never as root.
const (
	phpIndexMaxEntries    = 8000
	phpIndexMaxFiles      = 1000
	phpIndexMaxFileBytes  = 1 << 20
	phpIndexMaxTotalBytes = 32 << 20
	phpIndexMaxDepth      = 16
)

func workerPHPIndexManifest(op workspaceFileOperation) Response {
	if op.Path != "" || op.Content != "" || op.FileVersion != "" {
		return fileError("php_index_invalid_request", "validation", errors.New("manifest requires a bare workspace"))
	}
	root := filepath.Clean(op.Workspace)
	rootFD, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return fileError("php_index_workspace_unavailable", "security", err)
	}
	defer unix.Close(rootFD)
	hash := sha256.New()
	entries, files, total := 0, 0, int64(0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > phpIndexMaxEntries {
			return errors.New("php_index_entry_limit")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if len(strings.Split(rel, string(filepath.Separator))) > phpIndexMaxDepth {
			return errors.New("php_index_depth_limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", ".venv", "target", ".cache", ".idea", ".worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".php") && entry.Name() != "composer.json" {
			return nil
		}
		fd, err := unix.Openat2(rootFD, rel, &unix.OpenHow{
			Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
		})
		if err != nil {
			return fmt.Errorf("php_index_unsafe_file: %w", err)
		}
		file := os.NewFile(uintptr(fd), rel)
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > phpIndexMaxFileBytes {
			_ = file.Close()
			return errors.New("php_index_file_limit")
		}
		files++
		total += info.Size()
		if files > phpIndexMaxFiles || total > phpIndexMaxTotalBytes {
			_ = file.Close()
			return errors.New("php_index_tree_limit")
		}
		_, _ = fmt.Fprintf(hash, "%d:%s:%d:", len(rel), rel, info.Size())
		read, hashErr := io.CopyN(hash, file, info.Size())
		after, statErr := file.Stat()
		closeErr := file.Close()
		if hashErr != nil || statErr != nil || closeErr != nil || read != info.Size() ||
			after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return errors.New("php_index_source_changed")
		}
		return nil
	})
	if err != nil {
		return fileError("php_index_unbounded_or_unsafe", "resource", err)
	}
	if files == 0 {
		return fileError("php_index_no_sources", "validation", errors.New("no PHP files"))
	}
	count := int64(files)
	return Response{OK: true, Status: "php_index_manifest",
		FileVersion: "php-index-sha256:" + hex.EncodeToString(hash.Sum(nil)),
		BytesSeen:   total, FileSize: &count}
}
