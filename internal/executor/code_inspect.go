package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ach1992/ai-server-agent/internal/systemexec"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Semantic inspection remains bounded and worker-only: it uses the
// existing #55 worker-authority bounded source read and the #66 stdio broker.
// Neither the privileged executor nor the network process reads project source
// directly, and no LSP suggestion is auto-applied to disk.
const codeMaxDocumentBytes = (lspMaxFrameBytes / 2) - 1
const codeMaxTargetFiles = 16

var errCodeTargetUnstable = errors.New("code_target_changed")

func (s *Server) codeInspect(parent context.Context, req Request) Response {
	if req.Root || req.Approval || req.Workspace == "" || req.FileVersion == "" {
		return fileError("code_requires_worker_version", "validation", errors.New("worker authority, workspace, and exact file_version are required"))
	}
	if req.CodeMethod != "definition" && req.CodeMethod != "references" && req.CodeMethod != "symbols" && req.CodeMethod != "diagnostics" {
		return fileError("unsupported_code_method", "validation", errors.New("supported code methods are definition, references, symbols, diagnostics"))
	}
	if req.Line < 0 || req.Character < 0 || req.Line > 1<<20 || req.Character > 1<<20 {
		return fileError("invalid_code_position", "validation", errors.New("position must be zero-based, bounded and nonnegative"))
	}
	path, err := safeWorkspaceRelativeFile(req.Path)
	if err != nil {
		return fileError("invalid_code_path", "validation", errors.New("source path must be an ordinary workspace-relative file"))
	}
	profile, actualLanguage, supported := inspectProfileForPath(path)
	if isSQLSourcePath(path) {
		actualLanguage = "sql"
		if req.SQLDialect == "" {
			return fileError("sql_dialect_required", "validation", errors.New("SQL semantics require explicit sql_dialect postgres, mysql or mariadb; no implied default/database access"))
		}
		profile, supported = sqlLSPProfile(req.SQLDialect, "", "")
		if !supported {
			return fileError("unsupported_sql_dialect", "validation", errors.New("SQL dialect must be postgres, mysql or mariadb"))
		}
	} else if req.SQLDialect != "" {
		return fileError("invalid_sql_dialect_scope", "validation", errors.New("SQL dialect cannot be supplied for a non-SQL source"))
	}
	if !supported {
		return fileError("unsupported_code_language", "validation", errors.New("source extension is not supported by the installed first-class LSP profiles"))
	}
	if req.Language != "" && req.Language != actualLanguage {
		return fileError("code_language_mismatch", "validation", errors.New("requested language disagrees with the source path"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || filepath.Clean(req.Workspace) != workspace {
		return fileError("invalid_workspace", "validation", errors.New("workspace must be an exact resolved absolute path"))
	}
	// Reject huge files without reading all of them. A bounded first range
	// is never mistaken for a complete LSP textDocument/didOpen document.
	check := req
	check.Action = "workspace_stat"
	check.Path = path
	check.Limit = 0
	check.Offset = 0
	stat := s.workerWorkspaceFile(parent, check)
	if !stat.OK {
		return stat
	}
	if stat.FileSize == nil || *stat.FileSize > codeMaxDocumentBytes {
		return fileError("code_document_too_large", "resource", fmt.Errorf("source exceeds %d-byte reference-adapter limit; use workspace_read/CLI for large files", codeMaxDocumentBytes))
	}
	check.Action = "workspace_read"
	check.Limit = codeMaxDocumentBytes
	source := s.workerWorkspaceFile(parent, check)
	if !source.OK {
		return source
	}
	if source.FileVersion != req.FileVersion || source.EOF == nil || !*source.EOF || source.OutputEncoding != "utf-8" {
		return fileError("code_source_unstable", "conflict", errors.New("version mismatch, binary source, or incomplete read"))
	}
	binary := ""
	var phpArchive, phpCache string
	if actualLanguage == "go" {
		binary = s.goplsBinary
		if binary == "" {
			// Go retains the previously shipped trusted-path policy.
			binary = systemexec.First("/usr/local/bin/gopls", "/usr/bin/gopls")
		}
		if binary == "" {
			return fileError("gopls_unavailable", "dependency", errors.New("trusted optional gopls is not installed"))
		}
	} else if actualLanguage == "php" {
		binary = s.phpBinary // test-only; production selects trusted OS PHP.
		phpArchive = s.phpActorPHAR
		if binary != "" && phpArchive == "" {
			return fileError("php_lsp_fixture_incomplete", "dependency", errors.New("PHP test adapter requires the fixed PHAR fixture"))
		}
		if binary == "" {
			var trustErr error
			binary, phpArchive, trustErr = systemexec.PHPActor()
			if trustErr != nil {
				return fileError("php_lsp_unavailable", "dependency", errors.New("trusted optional pinned PHP CLI and Phpactor resource unavailable"))
			}
		}
		var cacheErr error
		phpCache, cacheErr = s.phpCacheDir(parent, req, workspace)
		if cacheErr != nil {
			return fileError("php_lsp_cache_unavailable", "dependency", cacheErr)
		}
		if !phpIndexStoreSafe(phpCache, s.workerUID) {
			return fileError("php_lsp_index_store_untrusted", "security", errors.New("PHP index store is not a worker-owned nonsymlink directory"))
		}
		profile = phpLSPProfile(phpArchive, filepath.Join(phpCache, "store"))
	} else if actualLanguage == "rust" {
		if !systemexec.Trusted(systemexec.SQLLensEnv) {
			return fileError("rust_lsp_unavailable", "dependency", errors.New("trusted fixed executable environment launcher unavailable"))
		}
		analyzer, rustc, sysroot := s.rustAnalyzerBinary, s.rustCompilerBinary, s.rustSysrootDir
		if analyzer == "" {
			var trustErr error
			analyzer, rustc, sysroot, trustErr = systemexec.RustAnalyzerResources()
			if trustErr != nil {
				return fileError("rust_lsp_unavailable", "dependency", errors.New("trusted optional Rust analyzer, rustc and sysroot are unavailable"))
			}
		} else if rustc == "" || sysroot == "" {
			return fileError("rust_lsp_fixture_incomplete", "dependency", errors.New("Rust test fixture requires native analyzer, pinned rustc and sysroot"))
		}
		binary = systemexec.SQLLensEnv
		profile = rustLSPProfile(filepath.Join(workspace, path), analyzer, rustc, sysroot)
	} else if actualLanguage == "sql" {
		if !systemexec.Trusted(systemexec.SQLLensEnv) {
			return fileError("sql_lsp_unavailable", "dependency", errors.New("trusted environment executable for fixed SQL interpreter settings is unavailable"))
		}
		binary = systemexec.SQLLensEnv
		node, cli := s.sqlNodeBinary, s.sqlCLIBinary // test-only, never MCP inputs
		if node != "" {
			if cli == "" {
				return fileError("sql_lsp_fixture_incomplete", "dependency", errors.New("SQL fixture must provide trusted interpreter and fixed CLI"))
			}
		} else {
			var trustErr error
			node, trustErr = systemexec.SQLLens()
			if trustErr != nil {
				return fileError("sql_lsp_unavailable", "dependency", errors.New("trusted optional pinned SQL language server and resources are unavailable"))
			}
			cli = systemexec.SQLLensCLI
		}
		profile, _ = sqlLSPProfile(req.SQLDialect, node, cli)
	} else if actualLanguage == "html" || actualLanguage == "css" {
		binary = s.webNodeBinary // test-only fixture, never callable from MCP input
		if binary != "" {
			if s.webHTMLCLIBinary == "" || s.webCSSCLIBinary == "" {
				return fileError("web_lsp_fixture_incomplete", "dependency", errors.New("the test Web LSP fixture must provide both adapters"))
			}
			profile = webLSPProfile(actualLanguage)
			if actualLanguage == "html" {
				profile.arguments[0] = s.webHTMLCLIBinary
			} else {
				profile.arguments[0] = s.webCSSCLIBinary
			}
		} else {
			var trustErr error
			binary, trustErr = systemexec.WebLSP()
			if trustErr != nil {
				return fileError("web_lsp_unavailable", "dependency", errors.New("trusted optional HTML/CSS language-server resources unavailable"))
			}
		}
	} else if actualLanguage == "shell" {
		binary = s.bashNodeBinary // test-only three-component fixture override
		if binary != "" {
			if s.bashCLIBinary == "" || s.bashShellcheckBinary == "" {
				return fileError("bash_lsp_fixture_incomplete", "dependency", errors.New("a test Bash LSP fixture must provide all components"))
			}
			profile = shellLSPProfile(s.bashCLIBinary, s.bashShellcheckBinary)
		} else {
			var trustErr error
			binary, trustErr = systemexec.BashLSP()
			if trustErr != nil {
				return fileError("bash_lsp_unavailable", "dependency", errors.New("trusted optional Bash LSP, pinned Node and ShellCheck resources are unavailable"))
			}
		}
	} else if actualLanguage == "python" {
		binary = s.tyBinary // test fixture only; production is always root owned.
		if binary == "" {
			var trustErr error
			binary, trustErr = systemexec.TyPython()
			if trustErr != nil {
				return fileError("python_ty_unavailable", "dependency", errors.New("trusted pinned optional native Python ty 0.0.87 adapter is unavailable"))
			}
		}
	} else {
		binary = s.ts7Binary // test fixture only; production cannot override this path.
		if binary == "" {
			var trustErr error
			binary, trustErr = systemexec.TypeScript7()
			if trustErr != nil {
				return fileError("typescript7_unavailable", "dependency", errors.New("trusted pinned optional TypeScript 7.0.2 native adapter and resources are unavailable"))
			}
		}
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	indexFingerprint := ""
	if actualLanguage == "php" {
		if !s.phpIndexMu.TryLock() {
			return fileError("php_lsp_index_busy", "resource", errors.New("another PHP index/navigation operation is active"))
		}
		defer s.phpIndexMu.Unlock()
		if req.CodeMethod == "definition" || req.CodeMethod == "references" {
			indexFingerprint, err = s.buildPHPIndex(ctx, req, workspace, binary, phpArchive, phpCache)
			if err != nil {
				return fileError("php_lsp_index_unavailable", "runtime", err)
			}
		}
	}
	canonicalReq := req
	canonicalReq.Path = path
	data, err := s.workerLSPInspection(ctx, canonicalReq, profile, binary, filepath.Join(workspace, path), source.Output, req.CodeMethod, req.Line, req.Character)
	if err != nil {
		if errors.Is(err, errCodeTargetUnstable) {
			return fileError("code_target_changed", "conflict", err)
		}
		if strings.HasPrefix(err.Error(), "lsp_capability_not_supported: ") {
			return fileError("code_capability_unsupported", "dependency", errors.New("the selected language server does not implement the requested semantic operation"))
		}
		return fileError("code_inspection_failed", "runtime", err)
	}
	// Recheck worker-authority file identity AFTER semantic computation. Do
	// not claim a location against an edit made concurrently by a shell/AI.
	check.Action = "workspace_stat"
	check.Limit = 0
	check.Offset = 0
	stable := s.workerWorkspaceFile(parent, check)
	if !stable.OK {
		return stable
	}
	if stable.FileVersion != req.FileVersion {
		return fileError("file_changed", "conflict", errors.New("source changed during semantic inspection"))
	}
	if indexFingerprint != "" {
		manifestReq := req
		manifestReq.Action, manifestReq.Path, manifestReq.FileVersion = "workspace_php_index_manifest", "", ""
		manifest := s.workerWorkspaceFile(parent, manifestReq)
		if !manifest.OK || manifest.FileVersion != indexFingerprint {
			return fileError("php_lsp_index_source_changed", "conflict", errors.New("PHP workspace changed during navigation"))
		}
	}
	response := Response{OK: true, Status: "code_" + req.CodeMethod, OutputEncoding: "json", Output: string(data), FileVersion: req.FileVersion, BytesReturned: int64(len(data)), BytesSeen: int64(len(data))}
	return response
}

// A private, fixed adapter profile lets one bounded LSP consumer support
// distinct language IDs and launch conventions without a second broker.
// It never accepts executable paths, arguments or configuration from a
// repository or external MCP caller. Entrypoint trust is checked upstream.
type lspInspectionProfile struct {
	arguments                     []string
	extensions                    map[string]string // recognized suffix -> LSP languageId
	initOptions                   map[string]any    // pinned, never project-controlled
	staticConfiguration           map[string]any    // fixed responses to server workspace/configuration
	waitForInitialPush            bool              // fixed adapters needing async document indexing
	pushOnlyDiagnostics           bool              // adapter advertised pull but fails to implement it
	syntheticChangeForDiagnostics bool              // in-memory document edit, never writes project source
	diagnosticSettle              time.Duration     // ignore premature empty reports for an adapter's debounce window
	waitForRustWorkspace          bool              // rust-analyzer semantic graph readiness is independent of diagnostics
}

var goLSPProfile = lspInspectionProfile{
	arguments:  []string{"serve"},
	extensions: map[string]string{".go": "go"},
}

// ShellCheck recognizes #!/bin/sh versus #!/usr/bin/env bash and returns
// dialect-appropriate diagnostics. All initialization options are fixed by
// the Agent; shell startup never interprets repository config as commands.
func shellLSPProfile(cli, shellcheck string) lspInspectionProfile {
	return lspInspectionProfile{
		arguments:          []string{cli, "start"},
		extensions:         map[string]string{".sh": "shellscript", ".bash": "shellscript"},
		waitForInitialPush: true,
		initOptions: map[string]any{
			"backgroundAnalysisMaxFiles":   0,
			"enableSourceErrorDiagnostics": true,
			"shellcheckExternalSources":    false,
			"shellcheckPath":               shellcheck,
			"explainshellEndpoint":         "",
			"shfmt":                        map[string]any{"path": ""},
		},
	}
}

var bashLSPProfile = shellLSPProfile(systemexec.BashLSPCLI, systemexec.BashShellCheck)

// Both VS Code web language services request document configuration. Use
// bounded immutable settings rather than forwarding repository settings or
// pretending their advertised document pull-diagnostics method works.
func webLSPProfile(language string) lspInspectionProfile {
	p := lspInspectionProfile{
		waitForInitialPush:  true,
		pushOnlyDiagnostics: true,
		staticConfiguration: map[string]any{
			"html": map[string]any{"validate": true},
			"css": map[string]any{"validate": true,
				"lint": map[string]any{"unknownProperties": "warning"}},
		},
	}
	switch language {
	case "html":
		p.extensions = map[string]string{".html": "html", ".htm": "html"}
		p.arguments = []string{systemexec.WebLSPHTMLCLI, "--stdio"}
	case "css":
		p.extensions = map[string]string{".css": "css"}
		p.arguments = []string{systemexec.WebLSPCSSCLI, "--stdio"}
	}
	return p
}

var htmlLSPProfile = webLSPProfile("html")
var cssLSPProfile = webLSPProfile("css")

func isSQLSourcePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sql", ".pgsql", ".psql", ".mysql", ".mariadb":
		return true
	}
	return false
}

// SQL dialects are explicit and independent of untrusted project configuration.
// The trusted env program execs a fixed, checksum-verified Node package.
// In particular, SQLLENS_NO_PLUGINS=1 disables project/user-supplied dynamic
// imports and SQLLENS_USER_CONFIG=/dev/null prevents implicit user credentials.
func sqlLSPProfile(dialect, node, cli string) (lspInspectionProfile, bool) {
	var languageID string
	switch dialect {
	case "postgres", "postgresql":
		languageID = "sql-postgres"
	case "mysql", "mariadb":
		languageID = "sql-mysql" // MariaDB compatibility subset; no full-vendor promise
	default:
		return lspInspectionProfile{}, false
	}
	return lspInspectionProfile{
		arguments: []string{
			"SQLLENS_NO_PLUGINS=1",
			"SQLLENS_USER_CONFIG=/dev/null",
			"NODE_ENV=production",
			node, cli, "--stdio",
		},
		extensions: map[string]string{
			".sql":     languageID,
			".pgsql":   languageID,
			".psql":    languageID,
			".mysql":   languageID,
			".mariadb": languageID,
		},
		waitForInitialPush:  true,
		pushOnlyDiagnostics: true,
	}, true
}

// The read-only Rust project graph is constructed from the already validated
// source path rather than untrusted Cargo.toml, build.rs, rustup or project
// config. PATH disables name-based program lookup; only a root-pinned rustc
// is available for toolchain metadata. The worker process is never privileged.
func rustLSPProfile(file, analyzer, rustc, sysroot string) lspInspectionProfile {
	settings := map[string]any{
		"linkedProjects": []any{map[string]any{
			"sysroot_src": filepath.Join(sysroot, "lib", "rustlib", "src", "rust", "library"),
			"crates": []any{map[string]any{
				"root_module":         file,
				"edition":             "2021",
				"deps":                []any{},
				"is_workspace_member": true,
			}},
		}},
		"cargo": map[string]any{
			"sysroot":      sysroot,
			"sysrootSrc":   filepath.Join(sysroot, "lib", "rustlib", "src", "rust", "library"),
			"buildScripts": map[string]any{"enable": false},
			"autoreload":   false, "noDeps": true,
		},
		"check":        map[string]any{"enable": false},
		"checkOnSave":  false,
		"procMacro":    map[string]any{"enable": false},
		"diagnostics":  map[string]any{"enable": true},
		"cachePriming": map[string]any{"enable": false},
		"workspace":    map[string]any{"discoverConfig": nil},
	}
	return lspInspectionProfile{
		arguments:            []string{"PATH=/dev/null", "RUSTC=" + rustc, "RUSTUP_AUTO_INSTALL=0", "RUSTUP_TOOLCHAIN=1.99.0", analyzer},
		extensions:           map[string]string{".rs": "rust"},
		initOptions:          settings,
		staticConfiguration:  map[string]any{"rust-analyzer": settings, "": settings},
		waitForRustWorkspace: true,
		pushOnlyDiagnostics:  true,
	}
}

// Phpactor is invoked without php.ini, PHP project settings or Composer
// autoloaders. Project startup hooks must never run in a read-only code tool.
func phpLSPProfile(phar, privateIndexPath string) lspInspectionProfile {
	safeConfig := map[string]any{
		"core.trusted":                       false,
		"core.project_config_candidates":     []string{},
		"composer.enable":                    false,
		"indexer.enabled_watchers":           []string{},
		"indexer.parallel_workers":           1,
		"indexer.max_filesize_to_index":      phpIndexMaxFileBytes,
		"indexer.include_patterns":           []string{"/**/*.php"},
		"indexer.follow_symlinks":            false,
		"indexer.index_path":                 privateIndexPath,
		"language_server.enable_trust_check": true,
	}
	config, _ := json.Marshal(safeConfig) // fixed serializable built-in types
	return lspInspectionProfile{
		arguments: []string{"-n",
			"-d", "extension=phar.so", "-d", "extension=posix.so",
			"-d", "extension=tokenizer.so", "-d", "extension=iconv.so",
			"-d", "extension=mbstring.so",
			phar, "language-server", "--no-interaction", "--no-ansi",
			"--config-extra=" + string(config),
		},
		extensions:                    map[string]string{".php": "php"},
		pushOnlyDiagnostics:           true,
		syntheticChangeForDiagnostics: true,
		diagnosticSettle:              1700 * time.Millisecond,
	}
}

var phpLSPSelectionProfile = lspInspectionProfile{extensions: map[string]string{".php": "php"}}

var rustLSPSelectionProfile = lspInspectionProfile{extensions: map[string]string{".rs": "rust"}}

var pythonTyLSPProfile = lspInspectionProfile{
	arguments:  []string{"server"},
	extensions: map[string]string{".py": "python", ".pyi": "python"},
}

// The native TypeScript 7 profile is selected only with its pinned,
// operator-owned executable and full standard-library resource set.
var typeScript7LSPProfile = lspInspectionProfile{
	arguments: []string{"--lsp", "--stdio"},
	extensions: map[string]string{
		".ts": "typescript", ".mts": "typescript", ".cts": "typescript",
		".tsx": "typescriptreact",
		".js":  "javascript", ".mjs": "javascript", ".cjs": "javascript",
		".jsx": "javascriptreact",
	},
	initOptions: map[string]any{"disableAutomaticTypingAcquisition": true},
}

func (profile lspInspectionProfile) languageForPath(path string) string {
	return profile.extensions[strings.ToLower(filepath.Ext(path))]
}

func inspectProfileForPath(path string) (lspInspectionProfile, string, bool) {
	if goLSPProfile.languageForPath(path) != "" {
		return goLSPProfile, "go", true
	}
	if phpLSPSelectionProfile.languageForPath(path) != "" {
		return phpLSPSelectionProfile, "php", true
	}
	if rustLSPSelectionProfile.languageForPath(path) != "" {
		return rustLSPSelectionProfile, "rust", true
	}
	if pythonTyLSPProfile.languageForPath(path) != "" {
		return pythonTyLSPProfile, "python", true
	}
	if bashLSPProfile.languageForPath(path) != "" {
		return bashLSPProfile, "shell", true
	}
	if htmlLSPProfile.languageForPath(path) != "" {
		return htmlLSPProfile, "html", true
	}
	if cssLSPProfile.languageForPath(path) != "" {
		return cssLSPProfile, "css", true
	}
	language := typeScript7LSPProfile.languageForPath(path)
	if language == "" {
		return lspInspectionProfile{}, "", false
	}
	if strings.HasPrefix(language, "typescript") {
		return typeScript7LSPProfile, "typescript", true
	}
	return typeScript7LSPProfile, "javascript", true
}

func (s *Server) workerGoInspection(ctx context.Context, req Request, goplsPath, file, source, method string, line, character int) (json.RawMessage, error) {
	return s.workerLSPInspection(ctx, req, goLSPProfile, goplsPath, file, source, method, line, character)
}

func (s *Server) workerLSPInspection(ctx context.Context, req Request, profile lspInspectionProfile, binary, file, source, method string, line, character int) (json.RawMessage, error) {
	// LSP framing and authorization remain in the same internal broker used
	// by the existing real-gopls definition proof.
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil {
		return nil, err
	}
	if len(source) > codeMaxDocumentBytes {
		return nil, errors.New("lsp_document_too_large")
	}
	if !filepath.IsAbs(file) || profile.languageForPath(file) == "" ||
		!strings.HasPrefix(filepath.Clean(file), workspace+string(filepath.Separator)) {
		return nil, errors.New("lsp_invalid_or_outside_source")
	}
	id, err := s.workerStdioSession(req, "lsp", workspace, binary, profile.arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.workerStdioClose(req, id) }()
	wire := newLSPWire(s, req, id)
	wire.staticConfiguration = profile.staticConfiguration
	uri := fileURI(file)
	root := fileURI(workspace)
	textDocumentCapabilities := map[string]any{
		"publishDiagnostics": map[string]any{"versionSupport": true},
	}
	if !profile.pushOnlyDiagnostics {
		// VS Code HTML/CSS language services switch to pull-only when this
		// client capability exists, but their current pull endpoint rejects
		// requests. An explicitly push-only adapter must not advertise it.
		textDocumentCapabilities["diagnostic"] = map[string]any{"dynamicRegistration": false}
	}
	initialized, err := wire.call(ctx, "initialize", map[string]any{
		"processId": nil, "rootUri": root,
		"initializationOptions": profile.initOptions,
		"workspaceFolders":      []map[string]string{{"uri": root, "name": filepath.Base(workspace)}},
		"capabilities": map[string]any{
			"workspace":    map[string]any{"configuration": true},
			"general":      map[string]any{"positionEncodings": []string{"utf-16"}},
			"textDocument": textDocumentCapabilities,
		},
	})
	if err != nil {
		return nil, err
	}
	serverCapabilities, err := parseLSPServerCapabilities(initialized)
	if err != nil {
		return nil, err
	}
	if !serverCapabilities.supports(method) {
		return nil, fmt.Errorf("lsp_capability_not_supported: %s", method)
	}
	if err = wire.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	openedContent := source
	if (profile.waitForRustWorkspace || profile.syntheticChangeForDiagnostics) && method == "diagnostics" {
		// A real didChange is needed for rust-analyzer syntax diagnostics.
		// It is strictly in-memory and followed by a pinned version check.
		openedContent = "\n"
	}
	if err = wire.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": profile.languageForPath(file), "version": 1, "text": openedContent},
	}); err != nil {
		return nil, err
	}
	if profile.waitForRustWorkspace {
		// A valid Rust source may have no diagnostics at all. The pinned
		// analyzer's own read-only status reports when its graph is loaded.
		if err := wire.waitRustWorkspaceReady(ctx); err != nil {
			return nil, fmt.Errorf("rust_lsp_workspace_not_ready: %w", err)
		}
	}
	if openedContent != source {
		// A bounded semantic query guarantees that the synthetic first-open
		// has been processed before applying the exact worker source in memory.
		if _, err := wire.call(ctx, "textDocument/documentSymbol",
			map[string]any{"textDocument": map[string]string{"uri": uri}}); err != nil {
			return nil, fmt.Errorf("lsp_synthetic_document_not_ready: %w", err)
		}
		if profile.waitForRustWorkspace {
			// rust-analyzer can finish graph load before VFS diagnostics
			// subscription. This bounded interval never invents results.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if err := wire.replaceSyntheticDiagnosticSource(uri, source); err != nil {
			return nil, err
		}
	}
	// Some real push-only servers (notably bash-language-server) parse and
	// index didOpen asynchronously. A semantic request before the first
	// version-correlated publishDiagnostics falsely reports an empty symbol
	// table. Await the document's first push as a mandatory readiness proof;
	// no unsolicited result from another URI can satisfy this gate.
	if profile.waitForInitialPush {
		if _, err := wire.waitPublishedDiagnostics(ctx, uri); err != nil {
			return nil, fmt.Errorf("lsp_initial_document_not_ready: %w", err)
		}
	}
	doc := map[string]string{"uri": uri}
	position := map[string]int{"line": line, "character": character}
	query := func() (json.RawMessage, error) {
		var raw json.RawMessage
		var queryErr error
		switch method {
		case "definition":
			raw, queryErr = wire.call(ctx, "textDocument/definition", map[string]any{"textDocument": doc, "position": position})
		case "references":
			raw, queryErr = wire.call(ctx, "textDocument/references", map[string]any{"textDocument": doc, "position": position, "context": map[string]bool{"includeDeclaration": true}})
		case "symbols":
			raw, queryErr = wire.call(ctx, "textDocument/documentSymbol", map[string]any{"textDocument": doc})
		case "diagnostics":
			if serverCapabilities.pullDiagnostics() && !profile.pushOnlyDiagnostics {
				raw, queryErr = wire.call(ctx, "textDocument/diagnostic", map[string]any{"textDocument": doc})
			} else {
				// Push-only servers publish asynchronously after didOpen. The
				// broker owns the process; only this request's opened URI and
				// worker-version-verified snapshot are accepted.
				if profile.diagnosticSettle > 0 {
					raw, queryErr = wire.waitPublishedDiagnosticsSettled(ctx, uri, profile.diagnosticSettle)
				} else {
					raw, queryErr = wire.waitPublishedDiagnostics(ctx, uri)
				}
			}
		default:
			return nil, errors.New("unsupported method")
		}
		if queryErr != nil {
			return nil, queryErr
		}
		if len(raw) > 16<<10 || !json.Valid(raw) || strings.TrimSpace(string(raw)) == "" {
			return nil, errors.New("invalid_or_oversized_lsp_result")
		}
		return normalizeCodeLocations(workspace, raw)
	}
	first, err := query()
	if err != nil {
		return nil, err
	}
	paths, err := codeTargetPathsForProfile(first, profile)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return first, nil
	}
	versions := make(map[string]string, len(paths))
	haveCrossFileTarget := false
	for _, target := range paths {
		if target == req.Path {
			versions[target] = req.FileVersion
			continue
		}
		haveCrossFileTarget = true
		targetReq := req
		targetReq.Path = target
		targetReq.Action = "workspace_stat"
		targetReq.FileVersion = ""
		targetReq.Offset = 0
		targetReq.Limit = 0
		stat := s.workerWorkspaceFile(ctx, targetReq)
		if !stat.OK || stat.FileVersion == "" || stat.FileSize == nil || *stat.FileSize > codeMaxDocumentBytes {
			return nil, fmt.Errorf("%w: target file is not safely inspectable", errCodeTargetUnstable)
		}
		targetReq.Action = "workspace_read"
		targetReq.Limit = codeMaxDocumentBytes
		read := s.workerWorkspaceFile(ctx, targetReq)
		if !read.OK || read.FileVersion != stat.FileVersion || read.EOF == nil || !*read.EOF || read.OutputEncoding != "utf-8" {
			return nil, fmt.Errorf("%w: target file snapshot not stable", errCodeTargetUnstable)
		}
		versions[target] = stat.FileVersion
		if err := wire.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": fileURI(filepath.Join(workspace, target)), "languageId": profile.languageForPath(target), "version": 1, "text": read.Output},
		}); err != nil {
			return nil, err
		}
	}
	if haveCrossFileTarget {
		if s.codeTargetSnapshotHook != nil {
			s.codeTargetSnapshotHook()
		}
		// Re-query with explicit didOpen overlays for EVERY in-workspace
		// target. The first query only discovers candidates; it is not
		// returned as exact semantic truth. If results diverge, fail closed.
		second, err := query()
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(first, second) {
			return nil, fmt.Errorf("%w: semantic targets changed across snapshot validation", errCodeTargetUnstable)
		}
	}
	// A file modified during the second semantic query invalidates the
	// returned line/range even when the request source remains identical.
	for _, target := range paths {
		targetReq := req
		targetReq.Action = "workspace_stat"
		targetReq.Path = target
		targetReq.FileVersion = ""
		targetReq.Limit = 0
		targetReq.Offset = 0
		stable := s.workerWorkspaceFile(ctx, targetReq)
		if !stable.OK || stable.FileVersion != versions[target] {
			return nil, fmt.Errorf("%w: target file changed during semantic resolution", errCodeTargetUnstable)
		}
	}
	return bindCodeLocationVersions(first, versions)
}

// Only normalized, in-workspace supported source targets may be inspected.
// Raw LSP URI is never filesystem read authority, and references are bounded.
func codeTargetPaths(normalized json.RawMessage) ([]string, error) {
	return codeTargetPathsForProfile(normalized, goLSPProfile)
}

func codeTargetPathsForProfile(normalized json.RawMessage, profile lspInspectionProfile) ([]string, error) {
	var data any
	if err := json.Unmarshal(normalized, &data); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var visit func(any, int) error
	visit = func(value any, depth int) error {
		if depth > 32 {
			return errors.New("lsp_result_too_deep")
		}
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				if err := visit(item, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, item := range v {
				if key == "uri" || key == "targetUri" {
					path, ok := item.(string)
					if !ok {
						return errors.New("invalid_normalized_location")
					}
					if path == "external" {
						continue
					}
					relative, err := safeWorkspaceRelativeFile(path)
					if err != nil || relative != path || profile.languageForPath(path) == "" {
						return fmt.Errorf("%w: unsupported semantic target", errCodeTargetUnstable)
					}
					seen[path] = true
					if len(seen) > codeMaxTargetFiles {
						return errors.New("code_target_file_limit")
					}
					continue
				}
				if err := visit(item, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(data, 0); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	// Stable order bounds resource consumption and makes repeated probes reliable.
	sort.Strings(paths)
	return paths, nil
}

func bindCodeLocationVersions(normalized json.RawMessage, versions map[string]string) (json.RawMessage, error) {
	var data any
	if err := json.Unmarshal(normalized, &data); err != nil {
		return nil, err
	}
	var visit func(any, int) error
	visit = func(value any, depth int) error {
		if depth > 32 {
			return errors.New("lsp_result_too_deep")
		}
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				if err := visit(item, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, item := range v {
				if key == "uri" || key == "targetUri" {
					path, ok := item.(string)
					if !ok {
						return errors.New("invalid_normalized_location")
					}
					if path != "external" {
						version, ok := versions[path]
						if !ok || version == "" {
							return fmt.Errorf("%w: location version unavailable", errCodeTargetUnstable)
						}
						if existing, has := v["file_version"]; has && existing != version {
							return fmt.Errorf("%w: ambiguous nested location versions", errCodeTargetUnstable)
						}
						v["file_version"] = version
					}
					continue
				}
				if err := visit(item, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(data, 0); err != nil {
		return nil, err
	}
	clean, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(clean) > 16<<10 {
		return nil, errors.New("code_result_too_large")
	}
	return clean, nil
}

func fileURI(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

// LSP URI identities are user-visible results, not a license to leak the
// server's home/toolchain paths. Keep in-workspace locations as relative paths
// while marking all external locations without exporting absolute host paths.
func normalizeCodeLocations(workspace string, raw json.RawMessage) (json.RawMessage, error) {
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	var normalize func(any, int) (any, error)
	normalize = func(value any, depth int) (any, error) {
		if depth > 32 {
			return nil, errors.New("lsp_result_too_deep")
		}
		switch v := value.(type) {
		case []any:
			for i, item := range v {
				changed, err := normalize(item, depth+1)
				if err != nil {
					return nil, err
				}
				v[i] = changed
			}
		case map[string]any:
			for key, item := range v {
				if key == "uri" || key == "targetUri" {
					str, ok := item.(string)
					if !ok {
						return nil, errors.New("lsp_location_malformed")
					}
					parsed, err := url.Parse(str)
					if err != nil || parsed.Scheme != "file" || parsed.Host != "" || !filepath.IsAbs(parsed.Path) {
						v[key] = "external"
						v["external"] = true
						continue
					}
					relative, err := filepath.Rel(workspace, filepath.Clean(parsed.Path))
					if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
						v[key] = "external"
						v["external"] = true
						continue
					}
					v[key] = filepath.ToSlash(relative)
					continue
				}
				changed, err := normalize(item, depth+1)
				if err != nil {
					return nil, err
				}
				v[key] = changed
			}
		}
		return value, nil
	}
	value, err := normalize(data, 0)
	if err != nil {
		return nil, err
	}
	clean, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(clean) > 16<<10 {
		return nil, errors.New("code_result_too_large")
	}
	return clean, nil
}
