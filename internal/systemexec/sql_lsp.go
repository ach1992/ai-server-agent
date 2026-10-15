package systemexec

import (
	"errors"
	"fmt"
	"runtime"
)

const (
	SQLLensVersion      = "0.5.0"
	SQLLensDir          = "/usr/local/lib/ai-server-agent/lsp/sql/sqllens/0.5.0"
	SQLLensModules      = SQLLensDir + "/node_modules"
	SQLLensCLI          = SQLLensModules + "/sqllens-language-server/bin/cli.js"
	SQLLensNodeOptional = SQLLensDir + "/node"
	// A root-trusted env(1) executes the immutable node entrypoint after
	// explicitly setting safe SQLLens process-local values. No runtime or
	// project process is allowed to choose/override these.
	SQLLensEnv = "/usr/bin/env"
)

// Locked sqllens-language-server@0.5.0 and its transitive npm package tree,
// npm ci --ignore-scripts --omit=dev (no .bin symlink wrappers).
// package-lock.json SHA256:
// 6932fab91a7d766666307482f27fbe894d3888ec0bc460acc4db5d45722fe649
const sqlLensModuleTreeDigest = "fbae6609e946b79a5537bf000c83ff9f8d7eea8ea927c2ba044d495da80f2afb"

func verifySQLLensModules(path string, rootOwned bool) error {
	return verifyPinnedNodeModules(path, rootOwned, nodeModulePolicy{
		label: "sql_lsp", fileCount: 1168, dirCount: 94,
		maxBytes: 75 << 20, digest: sqlLensModuleTreeDigest,
	})
}

// SQLLens returns only a validated administrator-owned node binary, not an
// arbitrary command line. Callers must additionally launch it through the
// trusted env binary, with SQLLENS_NO_PLUGINS=1 to block dynamic imports,
// and explicit per-document sql-postgres / sql-mysql language IDs. No DB
// connection, credentials, URL, or project executable is accepted.
func SQLLens() (string, error) {
	if runtime.GOOS != "linux" || bashNodeDigests[runtime.GOARCH] == "" {
		return "", errors.New("sql_lsp_unsupported_platform")
	}
	node := First(SQLLensNodeOptional, BashNodeOptional, WebNodeOptional, bashNodeBrowser)
	if node == "" {
		return "", errors.New("sql_lsp_trusted_node_unavailable")
	}
	if err := verifyShellNativeBinary(node, bashNodeDigests[runtime.GOARCH], 100<<20, 145<<20); err != nil {
		return "", errors.New("sql_lsp_node_integrity_invalid")
	}
	if !Trusted(SQLLensEnv) || !Trusted(SQLLensCLI) {
		return "", errors.New("sql_lsp_untrusted_entrypoint_or_env")
	}
	if err := verifySQLLensModules(SQLLensModules, true); err != nil {
		return "", fmt.Errorf("sql_lsp_untrusted_modules: %w", err)
	}
	return node, nil
}
