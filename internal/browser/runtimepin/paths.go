// Package runtimepin defines the pinned Linux Chromium executable contract
// shared by Browser installation/readiness and the privileged Executor.
// Keep upstream Playwright platform paths and the revision in one place.
package runtimepin

import "path/filepath"

const (
	EngineDir        = "/opt/ai-server-agent/browser"
	ChromiumRevision = "1228"
)

// ChromiumRelativeExecutables returns the exact Playwright v1.61.1 Linux
// artifact layouts. Unsupported architectures cannot supply a Browser binary.
func ChromiumRelativeExecutables(arch string) (chrome, headless string, ok bool) {
	switch arch {
	case "amd64":
		return filepath.FromSlash("chrome-linux64/chrome"),
			filepath.FromSlash("chrome-headless-shell-linux64/chrome-headless-shell"), true
	case "arm64":
		return filepath.FromSlash("chrome-linux/chrome"),
			filepath.FromSlash("chrome-linux/headless_shell"), true
	default:
		return "", "", false
	}
}

// IsPinnedChromiumExecutable is an exact path identity check for /proc/PID/exe,
// not a replacement for the separately root-owned/verified runtime contract.
// Rejects wrong revisions, platforms, deleted files and out-of-tree binaries.
func IsPinnedChromiumExecutable(exe, arch string) bool {
	chrome, headless, ok := ChromiumRelativeExecutables(arch)
	if !ok {
		return false
	}
	return exe == filepath.Join(EngineDir, "browsers", "chromium-"+ChromiumRevision, chrome) ||
		exe == filepath.Join(EngineDir, "browsers", "chromium_headless_shell-"+ChromiumRevision, headless)
}
