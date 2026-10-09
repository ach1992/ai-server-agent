package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"golang.org/x/sys/unix"
)

// The helper has *only* the aiworker Unix credentials. It receives no bearer,
// executor token, policy grant, or protected process environment from the root
// executor. A purpose-specific child is necessary: Go cannot safely switch
// per-goroutine Unix credentials inside the concurrent root executor.
type workspaceFileOperation struct {
	Action        string              `json:"action"`
	WorkspaceRoot string              `json:"workspace_root"`
	Workspace     string              `json:"workspace"`
	Path          string              `json:"path"`
	Offset        int64               `json:"offset,omitempty"`
	Limit         int                 `json:"limit,omitempty"`
	Content       string              `json:"content,omitempty"`
	FileVersion   string              `json:"file_version,omitempty"`
	MustNotExist  bool                `json:"must_not_exist,omitempty"`
	Edits         []WorkspaceFileEdit `json:"edits,omitempty"`
	Pattern       string              `json:"pattern,omitempty"`
	SearchLimit   int                 `json:"search_limit,omitempty"`
	SearchPaths   []string            `json:"search_paths,omitempty"`
	Globs         []string            `json:"globs,omitempty"`
	Literal       bool                `json:"literal,omitempty"`
	TimeoutMS     int64               `json:"timeout_ms,omitempty"`
}

const (
	workerWorkspaceTimeout = 30 * time.Second
	// Same bounded wire-budget as the executor request. The parent must
	// serialize/check before audit admission and helper process launch.
	// JSON escaping can turn 1 MiB of valid UTF-8 content into >2 MiB.
	maxWorkspaceHelperRequestBytes = maxExecutorRequestBytes
)

func (s *Server) workerWorkspaceFile(parent context.Context, req Request) (resp Response) {
	write := req.Action == "workspace_write" || req.Action == "workspace_apply_edits"
	if req.Action != "workspace_read" && req.Action != "workspace_write" && req.Action != "workspace_apply_edits" && req.Action != "workspace_text_search" {
		return fileError("invalid_action", "validation", errors.New("unsupported worker workspace action"))
	}
	if strings.TrimSpace(req.Workspace) == "" || (req.Action != "workspace_text_search" && req.Action != "workspace_apply_edits" && strings.TrimSpace(req.Path) == "") {
		return fileError("invalid_workspace", "validation", errors.New("workspace and relative file path are required"))
	}
	if req.Root || req.Approval || req.Mode != 0 {
		return fileError("invalid_request", "validation", errors.New("workspace files always use worker authority without privilege/mode override"))
	}
	if req.Action == "workspace_text_search" {
		probe := workspaceFileOperation{Action: req.Action, Pattern: req.Pattern, SearchLimit: req.SearchLimit, SearchPaths: req.SearchPaths, Globs: req.Globs, Literal: req.Literal, TimeoutMS: req.TimeoutMS}
		if err := validateWorkerTextSearch(probe); err != nil {
			return fileError("invalid_text_search", "validation", err)
		}
	} else if req.Action == "workspace_apply_edits" {
		if len(req.WorkspaceEdits) == 0 || len(req.WorkspaceEdits) > maxWorkspaceBatchFiles {
			return fileError("invalid_edit_count", "validation", errors.New("invalid multi-file edit count"))
		}
	} else if write {
		if !utf8.ValidString(req.Content) {
			return fileError("invalid_content", "validation", errors.New("workspace write content must be valid UTF-8"))
		}
		if len(req.Content) > maxFileWriteBytes {
			return fileError("input_too_large", "validation", errors.New("workspace content exceeds limit"))
		}
		if req.MustNotExist == (req.FileVersion != "") {
			return fileError("invalid_precondition", "validation", errors.New("write requires exactly one of must_not_exist or file_version"))
		}
	} else if req.Offset < 0 || req.Limit < 0 || req.Limit > maxFileReadBytes {
		return fileError("invalid_range", "validation", errors.New("invalid bounded read offset or limit"))
	}
	if req.Action != "workspace_apply_edits" && req.Action != "workspace_text_search" {
		if _, err := safeWorkspaceRelativeFile(req.Path); err != nil {
			return fileError("invalid_path", "validation", err)
		}
	}
	// Serialize and validate the exact helper wire frame BEFORE worker slot,
	// required intent audit, or any subprocess side effect. The public
	// content limits refer to raw bytes, but JSON escaping can be much larger.
	op := workspaceFileOperation{
		Action: req.Action, WorkspaceRoot: s.cfg.WorkspaceDir,
		Workspace: req.Workspace, Path: req.Path,
		Offset: req.Offset, Limit: req.Limit, Content: req.Content,
		FileVersion: req.FileVersion, MustNotExist: req.MustNotExist,
		Edits: req.WorkspaceEdits, Pattern: req.Pattern, SearchLimit: req.SearchLimit,
		SearchPaths: req.SearchPaths, Globs: req.Globs, Literal: req.Literal,
		TimeoutMS: req.TimeoutMS,
	}
	payload, err := json.Marshal(op)
	if err != nil {
		return fileError("invalid_request", "validation", err)
	}
	if len(payload) > maxWorkspaceHelperRequestBytes {
		return fileError("input_too_large", "resource", fmt.Errorf("serialized workspace helper request exceeds %d-byte frame", maxWorkspaceHelperRequestBytes))
	}
	// Keep a single bounded read/write in the established worker command slot.
	release, admitted := s.runs.acquire(false)
	if !admitted {
		return runCapacityResponse(false)
	}
	defer release()
	auditStart := time.Now()
	if write {
		if blocked := s.beginActionAudit(req, req.Action, "worker", req.Workspace+"\x00"+req.Path, "workspace"); blocked != nil {
			return *blocked
		}
		defer func() {
			resp = s.finishActionAudit(req, req.Action, "worker", req.Workspace+"\x00"+req.Path, "workspace", auditStart, resp)
		}()
	}

	exe, err := os.Executable()
	if err != nil {
		return fileError("workspace_helper_unavailable", "state", err)
	}
	if s.workspaceHelperBinary != "" {
		exe = s.workspaceHelperBinary
	}
	helperTimeout := workerWorkspaceTimeout
	if req.Action == "workspace_text_search" {
		helperTimeout = defaultWorkspaceSearchTimeout + 5*time.Second
		if req.TimeoutMS > 0 {
			helperTimeout = time.Duration(req.TimeoutMS)*time.Millisecond + 5*time.Second
		}
	}
	ctx, cancel := context.WithTimeout(parent, helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "workspace-helper")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = sanitizedCommandEnv(s.cfg.WorkspaceDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The helper may run ripgrep. Cancel the entire worker process group,
	// not just its leader, on disconnect/timeout to avoid orphaned search.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
	} else if uint32(os.Geteuid()) != s.workerUID {
		return fileError("worker_identity_unavailable", "policy", errors.New("executor cannot adopt configured worker identity"))
	}
	stdout := &boundedGitBuffer{limit: maxExecutorResponseBytes}
	cmd.Stdout = stdout
	// No raw stderr or request content is included in audit/error responses.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || stdout.overflow || ctx.Err() != nil {
		return unknownFileCompletion("worker file operation outcome could not be verified; inspect workspace before retrying", nil)
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return unknownFileCompletion("worker file helper returned an invalid response; inspect before retrying", nil)
	}
	if !write && resp.OK && s.audit != nil {
		// Read-only audit is diagnostic, never an authorization gate.
		// The pathname is fingerprinted; source content is not recorded.
		_ = s.audit.Write(audit.Entry{
			Phase: "complete", Action: req.Action, Mode: "worker",
			Command: req.Workspace + "\x00" + req.Path, Success: audit.Bool(true),
			RequestID: req.RequestID, PrincipalID: req.PrincipalID,
			PrincipalClass: req.PrincipalClass, PrincipalName: req.PrincipalName,
		})
	}
	return resp
}

func safeWorkspaceRelativeFile(path string) (string, error) {
	if len(path) > 4096 || strings.IndexByte(path, 0) >= 0 || filepath.IsAbs(path) {
		return "", errors.New("file path must be a bounded relative path")
	}
	clean := filepath.Clean(path)
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".git" {
			return "", errors.New("Git administrative files are not ordinary source files")
		}
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("file path must remain within source files, outside Git administrative metadata")
	}
	return clean, nil
}

// RunWorkspaceFileHelper is an internal self-exec entry, before config loading.
// Security comes from the actual aiworker OS identity and Openat2 containment,
// not from a privileged in-process filesystem prefix check.
func RunWorkspaceFileHelper() int {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxWorkspaceHelperRequestBytes+1))
	if err != nil {
		return 2
	}
	if len(raw) > maxWorkspaceHelperRequestBytes {
		// This is a known pre-execution rejection, not an unknown filesystem
		// completion. The parent validates the same bound before auditing.
		_ = json.NewEncoder(os.Stdout).Encode(fileError("input_too_large", "resource", errors.New("workspace helper wire frame exceeds limit")))
		return 0
	}
	var op workspaceFileOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		return 2
	}
	// No workspace path is opened before the kernel sandbox is in place.
	// Fail closed if this host cannot enforce the per-request boundary.
	unlock, sandboxErr := workerLandlockRestrict(op.WorkspaceRoot, op.Workspace, op.Action == "workspace_text_search")
	if sandboxErr != nil {
		_ = json.NewEncoder(os.Stdout).Encode(fileError("workspace_sandbox_unavailable", "security", sandboxErr))
		return 0
	}
	defer unlock()
	var resp Response
	if op.Action == "workspace_read" {
		resp = readWorkerWorkspaceFile(op)
	} else if op.Action == "workspace_write" {
		resp = writeWorkerWorkspaceFile(op)
	} else if op.Action == "workspace_apply_edits" {
		resp = applyWorkerWorkspaceEdits(op)
	} else if op.Action == "workspace_text_search" {
		resp = workerTextSearch(op)
	} else {
		return 2
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return 2
	}
	return 0
}

// Anchor both workspace and file resolution to directory descriptors. No
// symlink is followed, including intermediate components, and no traversal
// outside the configured Agent workspace can be resolved under concurrency.
func openWorkerWorkspace(op workspaceFileOperation) (*os.File, error) {
	if !filepath.IsAbs(op.WorkspaceRoot) || op.Workspace == "" || strings.IndexByte(op.WorkspaceRoot, 0) >= 0 {
		return nil, errors.New("configured workspace root is invalid")
	}
	root := filepath.Clean(op.WorkspaceRoot)
	selected := op.Workspace
	if !filepath.IsAbs(selected) {
		selected = filepath.Join(root, selected)
	}
	relative, err := filepath.Rel(root, filepath.Clean(selected))
	if err != nil || !withinPath(root, filepath.Clean(selected)) {
		return nil, errors.New("selected workspace is outside configured workspace")
	}
	rootFD, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{Flags: uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC), Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, fmt.Errorf("open configured workspace: %w", err)
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, relative, &unix.OpenHow{Flags: uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, fmt.Errorf("open selected workspace: %w", err)
	}
	return os.NewFile(uintptr(fd), selected), nil
}

func openWorkerParent(workspaceFD int, relative string) (*os.File, string, error) {
	clean, err := safeWorkspaceRelativeFile(relative)
	if err != nil {
		return nil, "", err
	}
	fd, err := unix.Openat2(workspaceFD, filepath.Dir(clean), &unix.OpenHow{Flags: uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, "", fmt.Errorf("open worker file parent: %w", err)
	}
	return os.NewFile(uintptr(fd), filepath.Dir(clean)), filepath.Base(clean), nil
}

func readWorkerWorkspaceFile(op workspaceFileOperation) Response {
	if op.Offset < 0 || op.Limit < 0 || op.Limit > maxFileReadBytes {
		return fileError("invalid_range", "validation", errors.New("invalid worker read range"))
	}
	workspace, err := openWorkerWorkspace(op)
	if err != nil {
		return fileError("invalid_workspace", "validation", err)
	}
	defer workspace.Close()
	parent, base, err := openWorkerParent(int(workspace.Fd()), op.Path)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	defer parent.Close()
	// Opening O_RDONLY first could block forever on a FIFO or activate a device.
	// Inspect the inode via O_PATH before opening data under worker authority.
	fd, err := unix.Openat2(int(parent.Fd()), base, &unix.OpenHow{Flags: uint64(unix.O_PATH | unix.O_CLOEXEC), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return fileError("file_unavailable", "io", err)
	}
	defer unix.Close(fd)
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return fileError("file_unavailable", "io", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileError("unsupported_file_type", "validation", errors.New("workspace reads require regular files"))
	}
	if err := rejectPseudoFilesystem(fd); err != nil {
		return fileError("unsupported_file_type", "validation", err)
	}
	// Reopen the pinned regular inode, not the mutable pathname. Kernel
	// permission checks still apply to the unprivileged worker process.
	dataFD, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", fd), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return fileError("file_unavailable", "io", err)
	}
	defer unix.Close(dataFD)
	var reopened unix.Stat_t
	if err := unix.Fstat(dataFD, &reopened); err != nil || !sameFileIdentity(before, reopened) {
		return fileError("file_changed", "conflict", errors.New("file changed during safe worker open"))
	}
	version := fileVersion(before)
	if op.FileVersion != "" && op.FileVersion != version {
		return fileError("file_changed", "conflict", errors.New("file changed before read"))
	}
	limit := op.Limit
	if limit == 0 {
		limit = maxFileReadBytes
	}
	raw := make([]byte, limit)
	n, err := unix.Pread(dataFD, raw, op.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fileError("file_read_failed", "io", err)
	}
	raw = raw[:n]
	var after unix.Stat_t
	if err := unix.Fstat(dataFD, &after); err != nil {
		return fileError("file_unavailable", "io", err)
	}
	if version != fileVersion(after) {
		return fileError("file_changed", "conflict", errors.New("file changed during read"))
	}
	output, encoding := encodeOutputBytes(raw)
	if len(output) > maxFileEncodedBytes {
		return fileError("response_too_large", "resource", errors.New("encoded workspace read exceeds response limit"))
	}
	next := op.Offset + int64(n)
	omitted := int64(0)
	if next < before.Size {
		omitted = before.Size - next
	}
	return Response{OK: true, Status: "read", Output: output, OutputEncoding: encoding, BytesSeen: int64(n), BytesReturned: int64(n), FileSize: int64Ptr(before.Size), FileVersion: version, RequestedOffset: op.Offset, NextOffset: int64Ptr(next), EOF: boolPtr(next >= before.Size), OmittedBytes: omitted, Truncated: omitted > 0}
}

func writeWorkerWorkspaceFile(op workspaceFileOperation) Response {
	if !utf8.ValidString(op.Content) {
		return fileError("invalid_content", "validation", errors.New("workspace write content must be valid UTF-8"))
	}
	if len(op.Content) > maxFileWriteBytes {
		return fileError("input_too_large", "validation", errors.New("workspace file content exceeds write limit"))
	}
	if op.MustNotExist == (op.FileVersion != "") {
		return fileError("invalid_precondition", "validation", errors.New("require one write precondition"))
	}
	workspace, err := openWorkerWorkspace(op)
	if err != nil {
		return fileError("invalid_workspace", "validation", err)
	}
	defer workspace.Close()
	parent, base, err := openWorkerParent(int(workspace.Fd()), op.Path)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	defer parent.Close()
	parentFD := int(parent.Fd())
	if err := rejectPseudoFilesystem(parentFD); err != nil {
		return fileError("unsupported_file_type", "validation", err)
	}
	original, exists, err := destinationState(parentFD, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if op.MustNotExist && exists {
		return fileError("file_exists", "conflict", errors.New("destination already exists"))
	}
	if op.FileVersion != "" && (!exists || op.FileVersion != fileVersion(original)) {
		return fileError("file_changed", "conflict", errors.New("destination has changed"))
	}
	mode := uint32(0644)
	if exists {
		mode = original.Mode & 0777
	}
	tempFD, tempName, err := createTempFileAt(parentFD)
	if err != nil {
		return fileError("file_write_failed", "io", err)
	}
	tempOwned := true
	defer func() {
		_ = unix.Close(tempFD)
		if tempOwned {
			_ = unix.Unlinkat(parentFD, tempName, 0)
		}
	}()
	if err = writeAllFD(tempFD, []byte(op.Content)); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	if err = unix.Fchmod(tempFD, mode); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	if err = unix.Fsync(tempFD); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	var prepared unix.Stat_t
	if err = unix.Fstat(tempFD, &prepared); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	current, currentExists, err := destinationState(parentFD, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if exists && (!currentExists || !sameFileSnapshot(original, current)) {
		return fileError("file_changed", "conflict", errors.New("destination changed before commit"))
	}
	if !exists && currentExists {
		return fileError("file_changed", "conflict", errors.New("destination appeared before commit"))
	}
	if exists {
		if err = unix.Renameat2(parentFD, tempName, parentFD, base, unix.RENAME_EXCHANGE); err != nil {
			return fileError("file_write_failed", "io", err)
		}
		tempOwned = false // temp name now references displaced user data; never unlink blindly.
		displaced, found, inspectErr := pathState(parentFD, tempName)
		if inspectErr != nil || !found {
			return unknownFileCompletion("displaced file state cannot be verified", nil)
		}
		if !sameFileCommitState(original, displaced) {
			if err := rollbackExistingExchange(parentFD, base, tempName, prepared, displaced); err != nil {
				return unknownFileCompletion("concurrent replacement raced with commit; rollback unproven", nil)
			}
			tempOwned = true
			return fileError("file_changed", "conflict", errors.New("destination changed during commit"))
		}
		if err = unix.Unlinkat(parentFD, tempName, 0); err != nil {
			return unknownFileCompletion("displaced file cleanup failed", &prepared)
		}
	} else {
		if err = unix.Renameat2(parentFD, tempName, parentFD, base, unix.RENAME_NOREPLACE); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return fileError("file_changed", "conflict", errors.New("destination appeared during commit"))
			}
			return fileError("file_write_failed", "io", err)
		}
		tempOwned = false
	}
	final, exists, err := destinationState(parentFD, base)
	if err != nil || !exists || !sameFileIdentity(prepared, final) {
		return unknownFileCompletion("committed workspace file identity cannot be proven", nil)
	}
	if err = unix.Fsync(parentFD); err != nil {
		return unknownFileCompletion("committed worker file directory sync failed", &final)
	}
	confirmed, exists, err := destinationState(parentFD, base)
	if err != nil || !exists || !sameFileIdentity(prepared, confirmed) {
		return unknownFileCompletion("workspace file changed after directory sync", nil)
	}
	return Response{OK: true, Status: "written", FileSize: int64Ptr(confirmed.Size), FileVersion: fileVersion(confirmed)}
}
