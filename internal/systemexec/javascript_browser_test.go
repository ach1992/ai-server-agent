package systemexec

import (
	"runtime"
	"testing"
)

func TestJavaScriptBrowserOptionalPinnedExecutable(t *testing.T) {
	browser, err := JavaScriptBrowser()
	if err == nil && (runtime.GOOS != "linux" || browser != expectedOptionalTestEntry(t, "javascript-browser", "binary", JavaScriptBrowserExecutableAMD64)) {
		t.Fatalf("untrusted browser executable admitted: %q", browser)
	}
	if err != nil && browser != "" {
		t.Fatalf("unverified browser path exposed: %s %v", browser, err)
	}
	if len(javaScriptBrowserSHA256AMD64) != 64 || JavaScriptBrowserExecutableAMD64[0] != '/' {
		t.Fatal("browser missing immutable native provenance")
	}
}
