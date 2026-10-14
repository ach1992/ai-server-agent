package systemexec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

const (
	BashLSPVersion        = "5.8.1"
	BashShellCheckVersion = "0.11.0"
	BashLSPDir            = "/usr/local/lib/ai-server-agent/lsp/bash/5.8.1"
	BashLSPModules        = BashLSPDir + "/node_modules"
	BashLSPCLI            = BashLSPModules + "/bash-language-server/out/cli.js"
	BashShellCheck        = BashLSPDir + "/shellcheck"
	BashNodeOptional      = BashLSPDir + "/node"
	bashNodeBrowser       = "/opt/ai-server-agent/browser/node/bin/node"
	bashModuleFileCount   = 2393
	bashModuleDirCount    = 172
	bashModuleMaxBytes    = 32 << 20
)

// Pinned npm dependencies are installed only by an authorized operator using
// npm ci --ignore-scripts --omit=dev from the exact audited lockfile. The
// complete extracted JavaScript dependency tree is verified before execution,
// not merely a trusted Node binary. .bin wrappers must be absent: the Agent
// starts one fixed CLI path and never executes project/npm PATH commands.
// bash-language-server@5.8.1 npm lock SHA-256 (test reference):
// 8eecfb1d5ab59df4672238e28df81ddc91487aa68143184ea83212b0379d524c
const bashModuleTreeDigest = "e71ba3e3e988f831e42a02711f30cd07a48fb06832bae90398e9feb1232a0e7a"

// Published immutable native binary digests:
// Node v24.18.1 from official node-v24.18.1-linux-{x64,arm64}.tar.xz.
// ShellCheck v0.11.0 from official shellcheck-v0.11.0.linux.{x86_64,aarch64}.tar.xz.
// ShellCheck is GPLv3: operator-managed optional binary, not a core dependency.
var bashNodeDigests = map[string]string{
	"amd64": "f3432a45b03b2da0d270095fdd8813dc34cbea73f5fc8b18c7a384b7cf9b333a",
	"arm64": "a990a8ae388fc285ddbce280e63fca48cfd7695f632b66aec6ed581566eace99",
}
var bashShellCheckDigests = map[string]string{
	"amd64": "4da528ddb3a4d1b7b24a59d4e16eb2f5fd960f4bd9a3708a15baddbdf1d5a55b",
	"arm64": "127f13925eadd52c341bca0ebaf9ab0dbd78c6468f30a8f262a528bf8de47546",
}

func BashLSP() (string, error) {
	if runtime.GOOS != "linux" || bashNodeDigests[runtime.GOARCH] == "" {
		return "", errors.New("bash_lsp_unsupported_platform")
	}
	node := First(BashNodeOptional, bashNodeBrowser)
	if node == "" {
		return "", errors.New("bash_lsp_trusted_node_unavailable")
	}
	if err := verifyShellNativeBinary(node, bashNodeDigests[runtime.GOARCH], 100<<20, 145<<20); err != nil {
		return "", errors.New("bash_lsp_node_integrity_invalid")
	}
	if !Trusted(BashShellCheck) {
		return "", errors.New("bash_lsp_trusted_shellcheck_unavailable")
	}
	if err := verifyShellNativeBinary(BashShellCheck, bashShellCheckDigests[runtime.GOARCH], 1<<20, 64<<20); err != nil {
		return "", errors.New("bash_lsp_shellcheck_integrity_invalid")
	}
	if !Trusted(BashLSPCLI) {
		return "", errors.New("bash_lsp_untrusted_server_entrypoint")
	}
	if err := verifyBashLSPModules(BashLSPModules, true); err != nil {
		return "", fmt.Errorf("bash_lsp_untrusted_modules: %w", err)
	}
	return node, nil
}

func verifyShellNativeBinary(path, want string, smallest, largest int64) error {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() < smallest || fi.Size() > largest {
		return errors.New("bash_lsp_invalid_native_binary")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("bash_lsp_native_binary_unreadable")
	}
	hash := sha256.New()
	n, copyErr := io.Copy(hash, io.LimitReader(file, fi.Size()+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || n != fi.Size() {
		return errors.New("bash_lsp_native_binary_unstable")
	}
	if hex.EncodeToString(hash.Sum(nil)) != want || want == "" {
		return errors.New("bash_lsp_native_binary_checksum_mismatch")
	}
	return nil
}

// An untrusted package tree cannot supply any executable JS modules. This
// wrapper enforces the exact Bash transitive package snapshot while sharing
// the filesystem/type/owner/byte verifier with optional HTML/CSS LSP.
func verifyBashLSPModules(root string, requireRoot bool) error {
	return verifyPinnedNodeModules(root, requireRoot, nodeModulePolicy{
		label: "bash_lsp", fileCount: bashModuleFileCount,
		dirCount: bashModuleDirCount, maxBytes: bashModuleMaxBytes,
		digest: bashModuleTreeDigest,
	})
}
