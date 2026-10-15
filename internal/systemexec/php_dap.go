package systemexec

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// This optional, operator-managed PHP CLI DAP profile is a COMPLETE pinned
// production-only tree built from xdebug/vscode-php-debug at exact upstream
// commit 5cee2626f227e0ebe88df2c03a1a88b4b4295173 using scripts-disabled
// npm ci (production dependencies only). The Agent never installs Node/PHP,
// executes a project-supplied adapter, downloads modules, or opens TCP.
const (
	PHPDAPVersion      = "5cee2626f227e0ebe88df2c03a1a88b4b4295173"
	PHPDAPDir          = "/usr/local/lib/ai-server-agent/dap/php/xdebug/" + PHPDAPVersion
	PHPDAPBundle       = PHPDAPDir + "/php-debug"
	PHPDAPCLI          = PHPDAPBundle + "/out/phpDebug.js"
	PHPDAPNodeOptional = PHPDAPDir + "/node"
	PHPDAPBinary       = "/usr/bin/php8.4"
	PHPDAPXdebugModule = "/usr/lib/php/20240924/xdebug.so"
	phpDAPBundleSHA256 = "0e939bb18e6bc314e8c18862664a2b87c0bbb844b3f124bb4df96243daf49f5c"
)

var phpDAPXdebugDigests = map[string]string{
	"amd64": "748d3eb77ea155e938f0c716aeb8fc071b063708d4fa657139c1ab1922155620",
	"arm64": "71bd24b28bddaa920da0f21ffc7981789dbc95d502de01314dc28c376ffa4a83",
}

func verifyPHPDAPBundle(root string, rootOwned bool) error {
	return verifyPinnedNodeModules(root, rootOwned, nodeModulePolicy{
		label: "php_dap", fileCount: 904, dirCount: 77,
		maxBytes: 16 << 20, digest: phpDAPBundleSHA256,
	})
}

func PHPDAP() (node, entrypoint, php, module string, err error) {
	if runtime.GOOS != "linux" || phpDAPXdebugDigests[runtime.GOARCH] == "" {
		return "", "", "", "", errors.New("php_dap_unsupported_platform")
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil || !rustDapDebian13(string(release)) {
		return "", "", "", "", errors.New("php_dap_unverified_native_distribution")
	}
	node = First(PHPDAPNodeOptional, bashNodeBrowser, BashNodeOptional, WebNodeOptional)
	if node == "" {
		return "", "", "", "", errors.New("php_dap_trusted_node_unavailable")
	}
	if err := verifyShellNativeBinary(node, bashNodeDigests[runtime.GOARCH], 100<<20, 145<<20); err != nil {
		return "", "", "", "", fmt.Errorf("php_dap_node_integrity_invalid: %w", err)
	}
	if !TrustedStaticFile(PHPDAPCLI) {
		return "", "", "", "", errors.New("php_dap_untrusted_adapter_entrypoint")
	}
	if err := verifyPHPDAPBundle(PHPDAPBundle, true); err != nil {
		return "", "", "", "", fmt.Errorf("php_dap_invalid_full_bundle: %w", err)
	}
	if !Trusted(PHPDAPBinary) || !TrustedStaticFile(PHPDAPXdebugModule) {
		return "", "", "", "", errors.New("php_dap_untrusted_native_php_xdebug")
	}
	if err := verifyShellNativeBinary(PHPDAPXdebugModule, phpDAPXdebugDigests[runtime.GOARCH], 512<<10, 16<<20); err != nil {
		return "", "", "", "", fmt.Errorf("php_dap_xdebug_module_integrity_invalid: %w", err)
	}
	return node, PHPDAPCLI, PHPDAPBinary, PHPDAPXdebugModule, nil
}
