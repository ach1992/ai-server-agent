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
	if actualLanguage == "go" {
		binary = s.goplsBinary
		if binary == "" {
			// Go retains the previously shipped trusted-path policy.
			binary = systemexec.First("/usr/local/bin/gopls", "/usr/bin/gopls")
		}
		if binary == "" {
			return fileError("gopls_unavailable", "dependency", errors.New("trusted optional gopls is not installed"))
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
	response := Response{OK: true, Status: "code_" + req.CodeMethod, OutputEncoding: "json", Output: string(data), FileVersion: req.FileVersion, BytesReturned: int64(len(data)), BytesSeen: int64(len(data))}
	return response
}

// A private, fixed adapter profile lets one bounded LSP consumer support
// distinct language IDs and launch conventions without a second broker.
// It never accepts executable paths, arguments or configuration from a
// repository or external MCP caller. Entrypoint trust is checked upstream.
type lspInspectionProfile struct {
	arguments          []string
	extensions         map[string]string // recognized suffix -> LSP languageId
	initOptions        map[string]any    // pinned, never project-controlled
	waitForInitialPush bool              // fixed adapters needing async document indexing
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
	if pythonTyLSPProfile.languageForPath(path) != "" {
		return pythonTyLSPProfile, "python", true
	}
	if bashLSPProfile.languageForPath(path) != "" {
		return bashLSPProfile, "shell", true
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
	uri := fileURI(file)
	root := fileURI(workspace)
	initialized, err := wire.call(ctx, "initialize", map[string]any{
		"processId": nil, "rootUri": root,
		"initializationOptions": profile.initOptions,
		"workspaceFolders":      []map[string]string{{"uri": root, "name": filepath.Base(workspace)}},
		"capabilities": map[string]any{
			"workspace": map[string]any{"configuration": true},
			"general":   map[string]any{"positionEncodings": []string{"utf-16"}},
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{"versionSupport": true},
				"diagnostic":         map[string]any{"dynamicRegistration": false},
			},
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
	if err = wire.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": profile.languageForPath(file), "version": 1, "text": source},
	}); err != nil {
		return nil, err
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
			if serverCapabilities.pullDiagnostics() {
				raw, queryErr = wire.call(ctx, "textDocument/diagnostic", map[string]any{"textDocument": doc})
			} else {
				// Push-only servers publish asynchronously after didOpen. The
				// broker owns the process; only this request's opened URI and
				// worker-version-verified snapshot are accepted.
				raw, queryErr = wire.waitPublishedDiagnostics(ctx, uri)
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
