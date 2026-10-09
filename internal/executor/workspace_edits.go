package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxWorkspaceBatchFiles       = 12
	maxWorkspaceBatchBytes       = 3 << 20
	maxWorkspaceEditReplacements = 24
)

// WorkspaceReplace is an exact, unambiguous text replacement (not a custom
// diff language). Each expected substring must appear exactly once when its
// edit is applied. LSP WorkspaceEdit and future structural rewrite consumers
// can plan immutable versioned edits into this same bounded application path.
type WorkspaceReplace struct {
	Old string `json:"old"`
	New string `json:"new"`
}

type WorkspaceFileEdit struct {
	Path         string             `json:"path"`
	FileVersion  string             `json:"file_version,omitempty"`
	MustNotExist bool               `json:"must_not_exist,omitempty"`
	Content      *string            `json:"content,omitempty"`
	Replacements []WorkspaceReplace `json:"replacements,omitempty"`
}

type WorkspaceEditFileOutcome struct {
	Path        string `json:"path"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	FileVersion string `json:"file_version,omitempty"`
}

type WorkspaceEditResult struct {
	Complete bool                       `json:"complete"`
	Partial  bool                       `json:"partial"`
	Applied  int                        `json:"applied"`
	Files    []WorkspaceEditFileOutcome `json:"files"`
}

type plannedWorkspaceEdit struct {
	input   WorkspaceFileEdit
	content string
}

func workspaceEditResponse(outcome WorkspaceEditResult, failure string) Response {
	b, err := json.Marshal(outcome)
	if err != nil {
		return fileError("response_encode_failed", "internal", err)
	}
	resp := Response{
		OK:     outcome.Complete,
		Output: string(b), OutputEncoding: "utf-8",
		BytesSeen: int64(len(b)), BytesReturned: int64(len(b)),
	}
	if outcome.Complete {
		resp.Status = "complete"
		return resp
	}
	if outcome.Partial {
		resp.Status = "partial"
		resp.ReasonCode = "partial_workspace_edit"
		resp.ErrorCode = "partial_workspace_edit"
		resp.ErrorClass = "state"
		resp.Error = "Some files were changed; inspect exact outcomes before retrying"
		return resp
	}
	resp.Status = "not_started"
	resp.ReasonCode = failure
	resp.ErrorCode = failure
	resp.ErrorClass = "conflict"
	resp.Error = "No workspace files were changed"
	return resp
}

// Preflight ALL targets and file versions before the first mutation. After
// preflight, each per-file write repeats the optimistic check at the atomic
// commit boundary. Concurrent changes may still cause a truthful partial
// result, never an all-or-nothing claim or a silent destructive rollback.
func applyWorkerWorkspaceEdits(op workspaceFileOperation) Response {
	return applyWorkerWorkspaceEditsWithHook(op, nil)
}

// Hook exists only for deterministic local concurrency tests. The production
// worker helper never supplies one.
func applyWorkerWorkspaceEditsWithHook(op workspaceFileOperation, beforeCommit func(int)) Response {
	entries := op.Edits
	if len(entries) == 0 || len(entries) > maxWorkspaceBatchFiles {
		return fileError("invalid_edit_count", "validation", fmt.Errorf("edit batch must contain between 1 and %d files", maxWorkspaceBatchFiles))
	}
	seen := make(map[string]bool, len(entries))
	planned := make([]plannedWorkspaceEdit, 0, len(entries))
	outcomes := make([]WorkspaceEditFileOutcome, len(entries))
	for i := range entries {
		outcomes[i].Path = entries[i].Path
	}
	totalBytes := 0
	failAt := func(i int, code string) Response {
		outcomes[i].Status = "failed"
		outcomes[i].ErrorCode = code
		for j := range outcomes {
			if outcomes[j].Status == "" {
				outcomes[j].Status = "not_attempted"
			}
		}
		return workspaceEditResponse(WorkspaceEditResult{Files: outcomes}, "workspace_preflight_failed")
	}
	for i, edit := range entries {
		outcomes[i].Path = edit.Path
		clean, err := safeWorkspaceRelativeFile(edit.Path)
		if err != nil {
			return failAt(i, "invalid_path")
		}
		if seen[clean] {
			return failAt(i, "duplicate_path")
		}
		seen[clean] = true
		if edit.MustNotExist == (edit.FileVersion != "") {
			return failAt(i, "invalid_precondition")
		}
		if (edit.Content == nil) == (len(edit.Replacements) == 0) {
			return failAt(i, "invalid_edit")
		}
		if len(edit.Replacements) > maxWorkspaceEditReplacements {
			return failAt(i, "too_many_replacements")
		}
		content := ""
		if edit.MustNotExist {
			if edit.Content == nil {
				return failAt(i, "create_requires_content")
			}
			content = *edit.Content
			// Check destination absence now, before ANY batch mutation.
			ws, err := openWorkerWorkspace(op)
			if err != nil {
				return failAt(i, "invalid_workspace")
			}
			parent, base, err := openWorkerParent(int(ws.Fd()), clean)
			if err != nil {
				ws.Close()
				return failAt(i, "invalid_path")
			}
			_, exists, stateErr := destinationState(int(parent.Fd()), base)
			parent.Close()
			ws.Close()
			if stateErr != nil {
				return failAt(i, "unsafe_file_type")
			}
			if exists {
				return failAt(i, "file_exists")
			}
		} else {
			read := readWorkerWorkspaceFile(workspaceFileOperation{
				Action: "workspace_read", WorkspaceRoot: op.WorkspaceRoot, Workspace: op.Workspace,
				Path: clean, Limit: maxFileReadBytes, FileVersion: edit.FileVersion,
			})
			if !read.OK {
				return failAt(i, read.ErrorCode)
			}
			if read.EOF == nil || !*read.EOF || read.OutputEncoding != "utf-8" {
				return failAt(i, "unsupported_content")
			}
			if edit.Content != nil {
				content = *edit.Content
			} else {
				content = read.Output
				for _, replace := range edit.Replacements {
					if replace.Old == "" || !utf8.ValidString(replace.Old) || !utf8.ValidString(replace.New) {
						return failAt(i, "invalid_replacement")
					}
					if strings.Count(content, replace.Old) != 1 {
						return failAt(i, "patch_context_mismatch")
					}
					content = strings.Replace(content, replace.Old, replace.New, 1)
					if len(content) > maxFileWriteBytes {
						return failAt(i, "input_too_large")
					}
				}
			}
		}
		if !utf8.ValidString(content) || len(content) > maxFileWriteBytes {
			return failAt(i, "invalid_content")
		}
		totalBytes += len(content)
		if totalBytes > maxWorkspaceBatchBytes {
			return failAt(i, "batch_too_large")
		}
		planned = append(planned, plannedWorkspaceEdit{input: edit, content: content})
	}
	applied := 0
	for i, plan := range planned {
		if beforeCommit != nil {
			beforeCommit(i)
		}
		result := writeWorkerWorkspaceFile(workspaceFileOperation{
			Action: "workspace_write", WorkspaceRoot: op.WorkspaceRoot, Workspace: op.Workspace,
			Path: plan.input.Path, Content: plan.content,
			FileVersion: plan.input.FileVersion, MustNotExist: plan.input.MustNotExist,
		})
		if !result.OK {
			outcomes[i].Status = "failed"
			outcomes[i].ErrorCode = result.ErrorCode
			// Unknown completion is NOT proven unchanged. It must not be
			// counted as applied, but callers need explicit reconciliation.
			if result.ErrorCode == "unknown_completion" {
				outcomes[i].Status = "unknown_completion"
			}
			for j := i + 1; j < len(outcomes); j++ {
				outcomes[j].Status = "not_attempted"
			}
			return workspaceEditResponse(WorkspaceEditResult{Complete: false, Partial: applied > 0 || result.ErrorCode == "unknown_completion", Applied: applied, Files: outcomes}, "workspace_apply_failed")
		}
		outcomes[i].Status = "applied"
		outcomes[i].FileVersion = result.FileVersion
		applied++
	}
	return workspaceEditResponse(WorkspaceEditResult{Complete: true, Applied: applied, Files: outcomes}, "")
}

func decodeWorkspaceEditResult(resp Response) (WorkspaceEditResult, error) {
	var out WorkspaceEditResult
	if resp.Output == "" {
		return out, errors.New("missing workspace edit outcome")
	}
	err := json.Unmarshal([]byte(resp.Output), &out)
	return out, err
}
