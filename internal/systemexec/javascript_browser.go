package systemexec

import (
	"errors"
	"runtime"
)

// Optional local Chromium DAP debug target. Operator-selected root-owned
// Chromium revisions can be verified independently of a compiled-in hash;
// the legacy browser revision remains the no-profile fallback. The coupled
// Playwright/Browser engine follows its own update/acceptance contract.
// Debugger launches stay local and contained; never remote CDP or PATH.
const (
	JavaScriptBrowserRevisionAMD64   = "chromium-1228"
	JavaScriptBrowserExecutableAMD64 = "/opt/ai-server-agent/browser/browsers/chromium-1228/chrome-linux64/chrome"
	javaScriptBrowserSHA256AMD64     = "2d18db9d8608b052b6a552ee00ec1e830f93692e928b65ecc67d693bd33fe801"
)

func JavaScriptBrowser() (string, error) {
	// Browser Debug target selection follows verified operator-managed
	// Chromium resources, not a source-compiled revision. The core Browser
	// engine/Playwright remains an independently coupled runtime.
	if runtime.GOOS == "linux" {
		if entries, _, configured, profileErr := SelectedOptionalAdapter("javascript-browser"); configured {
			if profileErr != nil {
				return "", profileErr
			}
			return entries["binary"], nil
		} else if profileErr != nil {
			return "", profileErr
		}
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", errors.New("javascript_browser_unverified_native_platform")
	}
	if !Trusted(JavaScriptBrowserExecutableAMD64) {
		return "", errors.New("javascript_browser_root_owned_chromium_unavailable")
	}
	if err := verifyShellNativeBinary(JavaScriptBrowserExecutableAMD64, javaScriptBrowserSHA256AMD64, 200<<20, 400<<20); err != nil {
		return "", errors.New("javascript_browser_pinned_chromium_integrity_invalid")
	}
	return JavaScriptBrowserExecutableAMD64, nil
}
