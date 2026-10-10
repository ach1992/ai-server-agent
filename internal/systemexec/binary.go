// Package systemexec validates optional, administrator-provisioned executables.
// It does not resolve a user-controlled executable via PATH.
package systemexec

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Trusted returns true only for an unlinked root-owned, executable regular
// file beneath root-owned, non-writable-by-others directory components.
// Parent validation prevents worker-controlled path replacement between
// checking and executing a conventional system executable.
func Trusted(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	current := string(filepath.Separator)
	pieces := strings.Split(strings.TrimPrefix(path, current), current)
	for i, part := range pieces {
		if part == "" || part == "." || part == ".." {
			return false
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return false
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			return false
		}
		if i == len(pieces)-1 {
			return info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
		}
		if !info.IsDir() {
			return false
		}
	}
	return false
}

func First(paths ...string) string {
	for _, path := range paths {
		if Trusted(path) {
			return path
		}
	}
	return ""
}
