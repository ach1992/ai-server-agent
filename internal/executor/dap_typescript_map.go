package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Node executes ONLY explicitly precompiled JavaScript. For TypeScript the
// exact generated JS, adjacent source map, and every referenced original
// source must be version-pinned under the same worker/workspace identity.
// There is no automatic project compiler, npm hook, ts-node or remote map.
const tsDAPMaxSourceMapBytes = 512 << 10
const tsDAPMaxMappedSources = 16

func (s *Server) readVersionedDebugSource(ctx context.Context, owner Request, relative, expected string, maxBytes int) (string, error) {
	check := owner
	check.Action, check.Path, check.FileVersion = "workspace_stat", relative, ""
	check.Offset, check.Limit = 0, 0
	meta := s.workerWorkspaceFile(ctx, check)
	if !meta.OK || meta.FileVersion == "" || meta.FileSize == nil ||
		*meta.FileSize < 1 || *meta.FileSize > int64(maxBytes) ||
		(expected != "" && meta.FileVersion != expected) {
		return "", errors.New("debug_source_missing_unbounded_or_changed")
	}
	check.Action = "workspace_read"
	check.FileVersion = meta.FileVersion
	check.Limit = maxBytes
	payload := s.workerWorkspaceFile(ctx, check)
	if !payload.OK || payload.FileVersion != meta.FileVersion || payload.EOF == nil || !*payload.EOF ||
		payload.Truncated || payload.OutputEncoding != "utf-8" ||
		payload.BytesSeen != *meta.FileSize {
		return "", errors.New("debug_source_read_incomplete_or_changed")
	}
	return payload.Output, nil
}

func (s *Server) inspectPrecompiledTypeScriptMap(ctx context.Context, owner Request, compiledRelative, compiledVersion string) (map[string]string, error) {
	compiled, err := s.readVersionedDebugSource(ctx, owner, compiledRelative, compiledVersion, tsDAPMaxSourceMapBytes)
	if err != nil {
		return nil, fmt.Errorf("typescript_dap_compiled_javascript: %w", err)
	}
	mapRelative := compiledRelative + ".map"
	// The executable must explicitly name *this* adjacent map, not an
	// externally fetched or independently injected map payload.
	if !strings.HasSuffix(strings.TrimSpace(compiled), "//# sourceMappingURL="+filepath.Base(mapRelative)) {
		return nil, errors.New("typescript_dap_compiled_source_map_link_missing")
	}
	mapReq := owner
	mapReq.Action, mapReq.Path, mapReq.FileVersion = "workspace_stat", mapRelative, ""
	mapReq.Offset, mapReq.Limit = 0, 0
	meta := s.workerWorkspaceFile(ctx, mapReq)
	if !meta.OK || meta.FileVersion == "" {
		return nil, errors.New("typescript_dap_source_map_not_worker_readable")
	}
	payload, err := s.readVersionedDebugSource(ctx, owner, mapRelative, meta.FileVersion, tsDAPMaxSourceMapBytes)
	if err != nil {
		return nil, fmt.Errorf("typescript_dap_source_map: %w", err)
	}
	var mapped struct {
		Version    int      `json:"version"`
		File       string   `json:"file"`
		SourceRoot string   `json:"sourceRoot"`
		Sources    []string `json:"sources"`
	}
	if err := json.Unmarshal([]byte(payload), &mapped); err != nil ||
		mapped.Version != 3 || mapped.File != filepath.Base(compiledRelative) ||
		(mapped.SourceRoot != "" && mapped.SourceRoot != ".") ||
		len(mapped.Sources) < 1 || len(mapped.Sources) > tsDAPMaxMappedSources {
		return nil, errors.New("typescript_dap_invalid_source_map_format_or_bounds")
	}
	versioned := map[string]string{compiledRelative: compiledVersion, mapRelative: meta.FileVersion}
	for _, src := range mapped.Sources {
		if src == "" || filepath.IsAbs(src) || strings.ContainsAny(src, ":\\\x00") ||
			strings.Contains(src, "://") {
			return nil, errors.New("typescript_dap_external_or_unsafe_source_map_target")
		}
		rel, err := safeWorkspaceRelativeFile(filepath.Join(filepath.Dir(compiledRelative), src))
		if err != nil || (filepath.Ext(rel) != ".ts" && filepath.Ext(rel) != ".tsx") {
			return nil, errors.New("typescript_dap_source_map_outside_typescript_workspace")
		}
		statReq := owner
		statReq.Action, statReq.Path, statReq.FileVersion = "workspace_stat", rel, ""
		statReq.Offset, statReq.Limit = 0, 0
		source := s.workerWorkspaceFile(ctx, statReq)
		if !source.OK || source.FileVersion == "" {
			return nil, errors.New("typescript_dap_source_map_target_unavailable")
		}
		versioned[rel] = source.FileVersion
	}
	return versioned, nil
}
