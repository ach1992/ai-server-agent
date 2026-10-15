package systemexec

import (
	"errors"
	"runtime"
)

// Optional independently tested Chromium from the Agent's existing pinned
// managed Browser install. DAP may launch only this exact local executable in
// a new private containment and disposable profile, never remote CDP or PATH.
// ARM64/other native distributions are unavailable until separately verified.
const (
	JavaScriptBrowserExecutableAMD64 = "/opt/ai-server-agent/browser/browsers/chromium-1228/chrome-linux64/chrome"
	javaScriptBrowserSHA256AMD64     = "2d18db9d8608b052b6a552ee00ec1e830f93692e928b65ecc67d693bd33fe801"
)

func JavaScriptBrowser() (string, error) {
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
