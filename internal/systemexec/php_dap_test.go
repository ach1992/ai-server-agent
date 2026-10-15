package systemexec

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPHPDAPOptionalVerifiedNativeChain(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		if len(phpDAPXdebugDigests[arch]) != 64 {
			t.Fatalf("unverified PHP native extension for %s", arch)
		}
	}
	if phpDAPXdebugDigests["386"] != "" || PHPDAPVersion == "" || phpDAPBundleSHA256 == "" {
		t.Fatal("PHP DAP accepted unknown architecture or unpinned dependency tree")
	}
	node, entry, php, module, err := PHPDAP()
	if err == nil && (runtime.GOOS != "linux" || node == "" || entry != PHPDAPCLI || php != PHPDAPBinary || module != PHPDAPXdebugModule) {
		t.Fatalf("PHP DAP readiness returned unverified runtime: %s/%s/%s/%s", node, entry, php, module)
	}
	if err != nil && (entry != "" || php != "" || module != "") {
		t.Fatal("unavailable PHP DAP exposed partial executable chain")
	}
}

func TestPHPDAPModuleTreeRejectsUntrustedFixture(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "out"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "out", "phpDebug.js"), []byte("console.log('untrusted')"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPHPDAPBundle(root, false); err == nil {
		t.Fatal("modified/partial PHP adapter supply chain was accepted")
	}
}
