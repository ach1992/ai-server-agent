package systemexec

import (
	"errors"
	"fmt"
	"runtime"
)

const (
	WebLSPVersion   = "4.10.0"
	WebLSPDir       = "/usr/local/lib/ai-server-agent/lsp/web/4.10.0"
	WebLSPModules   = WebLSPDir + "/node_modules"
	WebLSPHTMLCLI   = WebLSPModules + "/vscode-langservers-extracted/bin/vscode-html-language-server"
	WebLSPCSSCLI    = WebLSPModules + "/vscode-langservers-extracted/bin/vscode-css-language-server"
	WebNodeOptional = WebLSPDir + "/node"
)

// Exact transitive npm snapshot from vscode-langservers-extracted@4.10.0,
// installed without scripts, using a lockfile of SHA-256:
// e55987fc1ef27b3d5af84432381ca837843a30ff9ea35241172c53d525ccf67c
// This is a read-only analysis engine; no arbitrary client extension handlers,
// workspace scripts, project npm tools or auto-downloads are ever selected.
const webLSPModulesDigest = "3d7a363ea9cbbb86d04dc9c104b2ae1535d915763f73275f6c7f27bf7eaada69"

func verifyWebLSPModules(dir string, rootOwned bool) error {
	return verifyPinnedNodeModules(dir, rootOwned, nodeModulePolicy{
		label: "web_lsp", fileCount: 4948, dirCount: 375,
		maxBytes: 100 << 20, digest: webLSPModulesDigest,
	})
}

func WebLSP() (string, error) {
	if runtime.GOOS != "linux" || bashNodeDigests[runtime.GOARCH] == "" {
		return "", errors.New("web_lsp_unsupported_platform")
	}
	node := First(WebNodeOptional, BashNodeOptional, bashNodeBrowser)
	if node == "" {
		return "", errors.New("web_lsp_trusted_node_unavailable")
	}
	if err := verifyShellNativeBinary(node, bashNodeDigests[runtime.GOARCH], 100<<20, 145<<20); err != nil {
		return "", errors.New("web_lsp_node_integrity_invalid")
	}
	if !Trusted(WebLSPHTMLCLI) || !Trusted(WebLSPCSSCLI) {
		return "", errors.New("web_lsp_untrusted_entrypoint")
	}
	if err := verifyWebLSPModules(WebLSPModules, true); err != nil {
		return "", fmt.Errorf("web_lsp_untrusted_modules: %w", err)
	}
	return node, nil
}
