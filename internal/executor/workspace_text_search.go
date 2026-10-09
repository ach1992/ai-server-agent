package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type rgTextField struct {
	Text  string `json:"text"`
	Bytes string `json:"bytes"`
}

type rgJSONMatch struct {
	Type string `json:"type"`
	Data struct {
		Path           rgTextField `json:"path"`
		Lines          rgTextField `json:"lines"`
		LineNumber     int         `json:"line_number"`
		AbsoluteOffset int64       `json:"absolute_offset"`
		Submatches     []struct {
			Match rgTextField `json:"match"`
			Start int         `json:"start"`
			End   int         `json:"end"`
		} `json:"submatches"`
	} `json:"data"`
}

func validateWorkerTextSearch(op workspaceFileOperation) error {
	if op.Action != "workspace_text_search" || len(op.Pattern) == 0 || len(op.Pattern) > maxWorkspaceSearchPattern || strings.ContainsRune(op.Pattern, 0) || !utf8.ValidString(op.Pattern) {
		return errors.New("invalid text search pattern")
	}
	if op.TimeoutMS < 0 || op.TimeoutMS > int64(maxWorkspaceSearchTimeout/time.Millisecond) {
		return errors.New("invalid text search timeout")
	}
	if op.SearchLimit < 0 || op.SearchLimit > maxWorkspaceSearchLimit {
		return errors.New("invalid result limit")
	}
	if len(op.SearchPaths) > maxWorkspaceSearchPaths || len(op.Globs) > maxWorkspaceSearchGlobs {
		return errors.New("too many search filters")
	}
	for _, path := range op.SearchPaths {
		if len(path) > maxWorkspaceSearchPathBytes {
			return errors.New("search path too long")
		}
		if _, err := safeWorkspaceRelativeFile(path); err != nil {
			return err
		}
	}
	for _, glob := range op.Globs {
		if glob == "" || len(glob) > maxWorkspaceSearchGlobBytes || strings.ContainsAny(glob, "\x00\r\n") {
			return errors.New("invalid search glob")
		}
	}
	return nil
}

func workerTextSearch(op workspaceFileOperation) Response {
	started := time.Now()
	if err := validateWorkerTextSearch(op); err != nil {
		return workspaceSearchError("invalid_text_search", "validation", err.Error(), false)
	}
	root, err := openWorkerWorkspace(op)
	if err != nil {
		return workspaceSearchError("invalid_workspace", "validation", err.Error(), false)
	}
	defer root.Close()
	binary, err := filepath.EvalSymlinks("/usr/bin/rg")
	if err != nil || !trustedWorkspaceSearchExecutable(binary) {
		return workspaceSearchError("search_engine_unavailable", "dependency", "trusted system ripgrep is not installed", false)
	}
	limit := op.SearchLimit
	if limit == 0 {
		limit = defaultWorkspaceSearchLimit
	}
	args := []string{"--json", "--no-config", "--no-messages", "--no-follow", "--color=never"}
	if op.Literal {
		args = append(args, "--fixed-strings")
	}
	for _, glob := range append([]string{"!.git", "!**/.git/**"}, op.Globs...) {
		args = append(args, "-g", glob)
	}
	args = append(args, "-e", op.Pattern, "--")
	if len(op.SearchPaths) == 0 {
		args = append(args, ".")
	} else {
		args = append(args, op.SearchPaths...)
	}
	timeout := defaultWorkspaceSearchTimeout
	if op.TimeoutMS > 0 {
		timeout = time.Duration(op.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	// Use the validated absolute workspace identity, not a client-supplied
	// relative cwd that might resolve against the executor's directory.
	cmd.Dir = root.Name()
	cmd.Env = []string{"HOME=/nonexistent", "RIPGREP_CONFIG_PATH=", "PATH=" + safeCommandPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "NO_COLOR=1"}
	// Reuse inherited descriptors: opening /dev/null after the sandbox is
	// intentionally denied by Landlock, and stderr never goes to MCP output.
	cmd.Stdin = strings.NewReader("")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return workspaceSearchError("search_engine_unavailable", "process", "ripgrep output pipe unavailable", true)
	}
	if err := cmd.Start(); err != nil {
		return workspaceSearchError("search_engine_unavailable", "process", fmt.Sprintf("ripgrep failed to start in sandbox: %v", err), true)
	}
	result := WorkspaceSearchResult{Mode: "text", Workspace: op.Workspace, Engine: "ripgrep", Limit: limit, Complete: true, Matches: []WorkspaceSearchMatch{}}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 32<<10), maxWorkspaceSearchLineBytes)
	resultBytes := 0
	for scanner.Scan() {
		var event rgJSONMatch
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			result.Complete = false
			result.Truncated = true
			result.TruncationReason = "invalid_engine_event"
			break
		}
		if event.Type != "match" {
			continue
		}
		if event.Data.Path.Bytes != "" || event.Data.LineNumber < 1 {
			result.Complete = false
			result.Truncated = true
			result.TruncationReason = "unsupported_file_metadata"
			break
		}
		path, err := safeWorkspaceRelativeFile(event.Data.Path.Text)
		if err != nil {
			result.Complete = false
			result.Truncated = true
			result.TruncationReason = "unsafe_engine_path"
			break
		}
		for _, sub := range event.Data.Submatches {
			if sub.Start < 0 || sub.End < sub.Start || event.Data.AbsoluteOffset < 0 || event.Data.Lines.Bytes != "" {
				result.Complete = false
				result.Truncated = true
				result.TruncationReason = "unsupported_match_metadata"
				break
			}
			text := sub.Match.Text
			if sub.Match.Bytes != "" {
				result.Complete = false
				result.Truncated = true
				result.TruncationReason = "non_utf8_match"
				break
			}
			truncatedText := false
			if len(text) > maxWorkspaceSearchTextBytes {
				end := maxWorkspaceSearchTextBytes
				for end > 0 && !utf8.RuneStart(text[end]) {
					end--
				}
				text = text[:end]
				truncatedText = true
			}
			result.Matches = append(result.Matches, WorkspaceSearchMatch{
				File: path, Language: "text", Text: text, TextTruncated: truncatedText,
				Range: SearchRange{
					ByteOffset: SearchByteOffset{Start: event.Data.AbsoluteOffset + int64(sub.Start), End: event.Data.AbsoluteOffset + int64(sub.End)},
					Start:      SearchPosition{Line: event.Data.LineNumber - 1, Column: sub.Start},
					End:        SearchPosition{Line: event.Data.LineNumber - 1, Column: sub.End},
				},
			})
			resultBytes += len(path) + len(text) + 256
			if len(result.Matches) >= limit || resultBytes > maxWorkspaceSearchResultBytes-16384 {
				result.Complete = false
				result.Truncated = true
				result.TruncationReason = "result_limit"
				break
			}
		}
		if result.Truncated {
			break
		}
	}
	if scanner.Err() != nil && !result.Truncated {
		result.Complete = false
		result.Truncated = true
		result.TruncationReason = "engine_event_too_large"
	}
	if result.Truncated {
		cancel()
	}
	waitErr := cmd.Wait()
	timedOut := ctx.Err() == context.DeadlineExceeded
	if timedOut {
		result.Complete = false
		result.Truncated = true
		result.TimedOut = true
		result.TruncationReason = "timeout"
	}
	if waitErr != nil && !result.Truncated {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
			return workspaceSearchError("search_engine_error", "process", fmt.Sprintf("ripgrep search ended unsuccessfully: %T", waitErr), false)
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	payload, err := json.Marshal(result)
	if err != nil || len(payload) > maxWorkspaceSearchResultBytes {
		return workspaceSearchError("search_encode_failed", "resource", "bounded text search response could not be encoded", false)
	}
	return Response{OK: true, Status: workspaceSearchStatus(result), Output: string(payload), OutputEncoding: "utf-8", BytesSeen: int64(len(payload)), BytesReturned: int64(len(payload)), DurationMS: result.DurationMS, TimedOut: result.TimedOut, Truncated: result.Truncated}
}
