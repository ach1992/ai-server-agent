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

// The public Go-only semantic consumer is deliberately narrow: it uses the
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
		return fileError("unsupported_code_method", "validation", errors.New("supported Go methods are definition, references, symbols, diagnostics"))
	}
	if req.Line < 0 || req.Character < 0 || req.Line > 1<<20 || req.Character > 1<<20 {
		return fileError("invalid_code_position", "validation", errors.New("position must be zero-based, bounded and nonnegative"))
	}
	path, err := safeWorkspaceRelativeFile(req.Path)
	if err != nil || filepath.Ext(path) != ".go" {
		return fileError("invalid_code_path", "validation", errors.New("Go source path must be workspace-relative, ordinary .go source"))
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
		return fileError("code_document_too_large", "resource", fmt.Errorf("Go source exceeds %d-byte reference-adapter limit; use workspace_read/CLI for large files", codeMaxDocumentBytes))
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
	binary := s.goplsBinary
	if binary == "" {
		// Optional Go reference server must be provisioned by the operator.
		// The default is executable code in an administrator-owned location.
		binary = systemexec.First("/usr/local/bin/gopls", "/usr/bin/gopls")
	}
	if binary == "" {
		return fileError("gopls_unavailable", "dependency", errors.New("gopls is not installed at /usr/local/bin/gopls or /usr/bin/gopls"))
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	canonicalReq := req
	canonicalReq.Path = path
	data, err := s.workerGoInspection(ctx, canonicalReq, binary, filepath.Join(workspace, path), source.Output, req.CodeMethod, req.Line, req.Character)
	if err != nil {
		if errors.Is(err, errCodeTargetUnstable) {
			return fileError("code_target_changed", "conflict", err)
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
		return fileError("file_changed", "conflict", errors.New("Go source changed during semantic inspection"))
	}
	response := Response{OK: true, Status: "code_" + req.CodeMethod, OutputEncoding: "json", Output: string(data), FileVersion: req.FileVersion, BytesReturned: int64(len(data)), BytesSeen: int64(len(data))}
	return response
}

func (s *Server) workerGoInspection(ctx context.Context, req Request, goplsPath, file, source, method string, line, character int) (json.RawMessage, error) {
	// LSP framing and authorization remain in the same internal broker used
	// by the existing real-gopls definition proof.
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil {
		return nil, err
	}
	if len(source) > codeMaxDocumentBytes {
		return nil, errors.New("lsp_document_too_large")
	}
	id, err := s.workerStdioSession(req, "lsp", workspace, goplsPath, "serve")
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.workerStdioClose(req, id) }()
	wire := newLSPWire(s, req, id)
	uri := fileURI(file)
	root := fileURI(workspace)
	if _, err = wire.call(ctx, "initialize", map[string]any{
		"processId": nil, "rootUri": root,
		"workspaceFolders": []map[string]string{{"uri": root, "name": filepath.Base(workspace)}},
		"capabilities":     map[string]any{"workspace": map[string]any{"configuration": true}},
	}); err != nil {
		return nil, err
	}
	if err = wire.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	if err = wire.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": source},
	}); err != nil {
		return nil, err
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
			raw, queryErr = wire.call(ctx, "textDocument/diagnostic", map[string]any{"textDocument": doc})
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
	paths, err := codeTargetPaths(first)
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
			"textDocument": map[string]any{"uri": fileURI(filepath.Join(workspace, target)), "languageId": "go", "version": 1, "text": read.Output},
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

// Only normalized, in-workspace Go targets may be inspected. No raw LSP
// URI is used as a filesystem read authority, and references are bounded.
func codeTargetPaths(normalized json.RawMessage) ([]string, error) {
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
					if err != nil || relative != path || filepath.Ext(path) != ".go" {
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
