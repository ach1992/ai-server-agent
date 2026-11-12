package systemexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// Optional PHP CLI DAP resources are either an administrator-selected
// complete verified bundle with a runtime-tested PHP/Xdebug native ABI, or
// the legacy pinned Debian 13 bundle/extension fallback. No project adapter,
// implicit Node/PHP download, package scripts, or TCP listener is involved.
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
	if entries, _, configured, profileErr := SelectedOptionalAdapter("php-dap"); configured {
		if profileErr != nil {
			return "", "", "", "", profileErr
		}
		// This compatibility probe is deliberately version-agnostic: only a
		// native Xdebug 3 extension that can actually load in the chosen PHP
		// interpreter passes. The legacy Debian/PHP API pin remains a fallback.
		if err := compatiblePHPXdebug(entries["php"], entries["xdebug"]); err != nil {
			return "", "", "", "", err
		}
		return entries["node"], entries["cli"], entries["php"], entries["xdebug"], nil
	} else if profileErr != nil {
		return "", "", "", "", profileErr
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

// The native ABI cannot be inferred from a package version ceiling or a file
// hash. Probe one isolated, trusted CLI invocation with project/site config
// disabled, no user environment, and a hard deadline. Never load extensions
// from an unverified profile or a workspace path.
func compatiblePHPXdebug(php, module string) error {
	if !Trusted(php) || !TrustedStaticFile(module) {
		return errors.New("php_dap_native_runtime_untrusted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, php, "-n", "-d", "zend_extension="+module, "-r", "exit(extension_loaded('xdebug') && function_exists('xdebug_info') ? 0 : 1);")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "XDEBUG_MODE=off", "PHP_INI_SCAN_DIR="}
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("php_dap_xdebug_abi_probe_timeout")
		}
		return fmt.Errorf("php_dap_xdebug_abi_incompatible_or_load_failed: %w", err)
	}
	return nil
}
