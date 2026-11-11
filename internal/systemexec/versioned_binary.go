package systemexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// newestTrustedVersioned selects the highest installed minor release in one
// explicitly selected major series. A major bound comes from the adapter's
// upstream runtime requirements (e.g. Phpactor requires ^8.2), not an
// arbitrary list of currently known minor releases. PATH, distro alternatives,
// user-owned executables and symlinks never participate.
func newestTrustedVersioned(directory, name string, major, minMinor int) string {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return ""
	}
	return newestVersionedPath(directory, entries, name, major, minMinor, Trusted)
}

func newestVersionedPath(directory string, entries []os.DirEntry, name string, major, minMinor int, trusted func(string) bool) string {
	prefix := fmt.Sprintf("%s%d.", name, major)
	bestMinor, bestPath := minMinor-1, ""
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok {
			continue
		}
		minor, err := strconv.Atoi(suffix)
		if err != nil || strconv.Itoa(minor) != suffix || minor <= bestMinor {
			continue
		}
		candidate := filepath.Join(directory, entry.Name())
		if trusted(candidate) {
			bestMinor, bestPath = minor, candidate
		}
	}
	return bestPath
}
