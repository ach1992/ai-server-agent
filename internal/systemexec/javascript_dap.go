package systemexec

import (
	"errors"
	"fmt"
	"runtime"
)

const (
	JavaScriptDAPVersion = "1.140.0"
	JavaScriptDAPDir     = "/usr/local/lib/ai-server-agent/dap/javascript/js-debug/1.140.0"
	JavaScriptDAPBundle  = JavaScriptDAPDir + "/js-debug"
	JavaScriptDAPCLI     = JavaScriptDAPBundle + "/src/dapDebugServer.js"
	// Verified official Microsoft vscode-js-debug v1.140.0 release asset:
	// js-debug-dap-v1.140.0.tar.gz SHA256
	// 27dab92937ec1ab35821ae955aac867544fe06a1b6307229049f2d789af10968.
	// After a safe symlink-free extraction, the COMPLETE official bundle has
	// 60 files and 9 directories. Pin all source, WASM/native resources and
	// dependencies (not only the Node runtime or standalone entrypoint).
	javascriptDAPTreeDigest = "cc53f6d1cd9c1b264dc797fde8f36aedae880fab5e56cba906fbb6f007df1f0c"
)

func verifyJavaScriptDAPResources(dir string, rootOwned bool) error {
	return verifyPinnedNodeModules(dir, rootOwned, nodeModulePolicy{
		label: "javascript_dap", fileCount: 60, dirCount: 9,
		maxBytes: 4 << 20, digest: javascriptDAPTreeDigest,
	})
}

// JavaScriptDAP verifies adapter provenance only; it does NOT create a
// debugger session or grant any reverse DAP request. Node/TS child-target
// handling, local Browser/extension modes and cgroup cleanup have separate
// acceptance gates in #140. A complete upstream bundle alone is not readiness
// for a connected MCP debugger.
func JavaScriptDAP() (node, adapter string, err error) {
	if runtime.GOOS != "linux" || bashNodeDigests[runtime.GOARCH] == "" {
		return "", "", errors.New("javascript_dap_unsupported_platform")
	}
	node = First(bashNodeBrowser, BashNodeOptional, WebNodeOptional)
	if node == "" {
		return "", "", errors.New("javascript_dap_trusted_node_unavailable")
	}
	if err = verifyShellNativeBinary(node, bashNodeDigests[runtime.GOARCH], 100<<20, 145<<20); err != nil {
		return "", "", errors.New("javascript_dap_node_integrity_invalid")
	}
	if !TrustedStaticFile(JavaScriptDAPCLI) {
		return "", "", errors.New("javascript_dap_untrusted_entrypoint")
	}
	if err = verifyJavaScriptDAPResources(JavaScriptDAPBundle, true); err != nil {
		return "", "", fmt.Errorf("javascript_dap_invalid_bundle_provenance: %w", err)
	}
	return node, JavaScriptDAPCLI, nil
}
