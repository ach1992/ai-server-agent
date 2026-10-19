package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
	"golang.org/x/sys/unix"
)

// shellDAPTrustedRuntime uses the original installed Agent binary itself as
// the adapter. This avoids third-party JS/bashdb packages and duplicated
// per-language launch brokers. A replaced binary while the Executor is still
// running requires an ordinary managed restart, never an unverified fork.
func shellDAPTrustedRuntime() (agent, bash string, err error) {
	bash = systemexec.First("/usr/bin/bash")
	if bash == "" {
		return "", "", errors.New("trusted root-owned /usr/bin/bash unavailable")
	}
	agent, err = os.Executable()
	if err != nil || !systemexec.Trusted(agent) {
		return "", "", errors.New("trusted original Agent executable unavailable")
	}
	running, e1 := os.Stat("/proc/self/exe")
	path, e2 := os.Stat(agent)
	if e1 != nil || e2 != nil || !os.SameFile(running, path) {
		return "", "", errors.New("installed Agent binary differs from the running Executor")
	}
	return agent, bash, nil
}

// Shell DAP is deliberately Bash-only: no POSIX sh or zsh semantics, remote
// attach, eval, adapter-provided commands or terminal reverse-launch.
func (s *Server) debugLaunchShell(ctx context.Context, req Request) (response Response) {
	if req.Language != "shell" || req.DebugMode != "bash" {
		return fileError("shell_dap_unsupported_mode", "validation", errors.New("only explicit language=shell mode=bash script debugging is supported"))
	}
	if req.Workspace == "" || req.DebugProgram == "" || req.FileVersion == "" {
		return fileError("debug_identity_required", "validation", errors.New("workspace, script and exact source file_version required"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || workspace != filepath.Clean(req.Workspace) {
		return fileError("invalid_workspace", "validation", errors.New("exact workspace identity required"))
	}
	relative, err := safeWorkspaceRelativeFile(req.DebugProgram)
	ext := filepath.Ext(relative)
	if err != nil || ext != ".sh" && ext != ".bash" {
		return fileError("shell_dap_invalid_source", "validation", errors.New("only workspace-relative .sh/.bash scripts are supported"))
	}
	program := filepath.Join(workspace, relative)
	info, err := os.Lstat(program)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fileError("shell_dap_invalid_source", "validation", errors.New("script must be a regular, non-symlink workspace file"))
	}
	statReq := req
	statReq.Action, statReq.Path, statReq.FileVersion = "workspace_stat", relative, ""
	statReq.Offset, statReq.Limit = 0, 0
	stat := s.workerWorkspaceFile(ctx, statReq)
	if !stat.OK {
		return stat
	}
	if stat.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("script changed before launch"))
	}
	if os.Geteuid() != 0 {
		return fileError("dap_containment_unavailable", "dependency", errors.New("Shell DAP requires root Executor-owned private cgroup-v2"))
	}
	agent, bash, err := shellDAPTrustedRuntime()
	if err != nil {
		return fileError("shell_dap_runtime_unavailable", "dependency", err)
	}
	auditObject := workspace + "\x00" + relative + "\x00shell/bash"
	if blocked := s.beginActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger"); blocked != nil {
		return *blocked
	}
	start := time.Now()
	defer func() {
		response = s.finishActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger", start, response)
	}()
	group, err := createDAPCgroup()
	if err != nil {
		return fileError("dap_containment_unavailable", "dependency", err)
	}
	attached := false
	defer func() {
		if !attached && group.path != "" {
			if e := group.abortBeforeLaunch(); e != nil {
				response.OK = false
				response.ErrorCode, response.ErrorClass = "dap_containment_cleanup_uncertain", "state"
				response.Error = fmt.Sprintf("Shell private cgroup cleanup unverified: %v", e)
			}
		}
	}()
	id, err := s.workerContainedDAPSession(req, workspace, agent, group, "shell-dap-helper")
	if err != nil {
		failed := fileError("shell_dap_start_failed", "runtime", err)
		if id != "" {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				failed.SessionID = id
				failed.Status, failed.ErrorCode = "cleanup_uncertain", "debug_cleanup_uncertain"
				failed.Error += "; " + closeErr.Error()
			}
		}
		return failed
	}
	cleanup := true
	defer func() {
		if cleanup {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				response.OK = false
				response.SessionID, response.Status = id, "cleanup_uncertain"
				response.ErrorCode, response.ErrorClass, response.Error = "debug_cleanup_uncertain", "state", closeErr.Error()
			}
		}
	}()
	e, err := s.sessions.get(req, id)
	if err != nil {
		return fileError("shell_dap_session_lost", "state", err)
	}
	e.stopMu.Lock()
	if !group.verified {
		e.stopMu.Unlock()
		return fileError("dap_containment_missing", "state", errors.New("Shell adapter was not atomically contained"))
	}
	e.mu.Lock()
	if e.closed || e.exited || e.pidPin == nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("shell_dap_adapter_exited", "state", errors.New("Shell adapter exited before pin"))
	}
	fd, err := unix.Dup(int(e.pidPin.Fd()))
	if err != nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("shell_dap_pidfd_unavailable", "state", err)
	}
	d := &dapState{
		conn: newStdioDAPConn(e.dapStdout, e.stdin), workspace: workspace, adapter: "shell/bash-trap",
		adapterPID: e.pid, adapterPin: os.NewFile(uintptr(fd), "shell-dap-original-adapter-pidfd"),
		workerUID: s.workerUID, program: relative, targetInterpreter: bash,
		sourceVersion: req.FileVersion, sourceVersions: map[string]string{relative: req.FileVersion},
		stage: "starting", containment: group,
	}
	e.dap = d
	e.mu.Unlock()
	e.stopMu.Unlock()
	attached = true
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = d.call(callCtx, "initialize", map[string]any{
		"adapterID": "shell/bash-trap", "clientID": "ai-server-agent",
		"linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path",
		"supportsVariableType": true, "supportsRunInTerminalRequest": false,
	})
	if err != nil {
		return fileError("shell_dap_initialize_failed", "runtime", err)
	}
	_, err = d.call(callCtx, "launch", map[string]any{
		"program": program, "cwd": workspace, "stopOnEntry": req.DebugStopOnEntry,
	})
	if err != nil {
		return fileError("shell_dap_launch_failed", "runtime", err)
	}
	after := s.workerWorkspaceFile(ctx, statReq)
	if !after.OK || after.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("Bash script changed during launch"))
	}
	cleanup = false
	body, _ := json.Marshal(map[string]any{"adapter": "shell/bash-trap", "mode": "bash", "configuration_required": true})
	return Response{OK: true, SessionID: id, Status: "starting", FileVersion: req.FileVersion, Output: string(body), OutputEncoding: "json", BytesSeen: int64(len(body)), BytesReturned: int64(len(body))}
}
