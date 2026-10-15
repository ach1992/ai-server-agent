package executor

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
	"golang.org/x/sys/unix"
)

// Rust is a worker-owned prebuilt ELF execution mode. An executor-created
// private cgroup v2 owns both the pinned LLDB adapter and every debuggee,
// even if LLDB crashes before emitting a trustworthy DAP process event.
// The Agent never runs cargo, rustc, build.rs, workspace overrides, or an
// adapter-provided terminal command as part of debugging.
func (s *Server) debugLaunchRust(ctx context.Context, req Request) (response Response) {
	if req.DebugMode != "" && req.DebugMode != "exec" {
		return fileError("rust_dap_unsupported_mode", "validation", errors.New("Rust supports only a prebuilt executable, no attach/Cargo/build command"))
	}
	if req.Workspace == "" || req.DebugProgram == "" || req.FileVersion == "" {
		return fileError("debug_identity_required", "validation", errors.New("exact workspace, Rust ELF and executable file_version required"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || workspace != filepath.Clean(req.Workspace) {
		return fileError("invalid_workspace", "validation", errors.New("exact resolved worker workspace required"))
	}
	relative, err := safeWorkspaceRelativeFile(req.DebugProgram)
	if err != nil {
		return fileError("rust_dap_invalid_program", "validation", errors.New("Rust ELF must be workspace-relative"))
	}
	file := filepath.Join(workspace, relative)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0111 == 0 {
		return fileError("rust_dap_invalid_program", "validation", errors.New("prebuilt Rust debug target must be a regular executable, never a symlink"))
	}
	st := req
	st.Action, st.Path, st.FileVersion, st.Offset, st.Limit = "workspace_stat", relative, "", 0, 0
	current := s.workerWorkspaceFile(ctx, st)
	if !current.OK {
		return current
	}
	if current.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("Rust target file_version changed"))
	}
	// Probe only through the existing version-pinned unprivileged worker
	// reader. Root must not open or parse bytes through an untrusted parent
	// directory or a symlink that might escape the selected workspace.
	headerReq := st
	headerReq.Action, headerReq.FileVersion, headerReq.Limit = "workspace_read", current.FileVersion, 20
	header := s.workerWorkspaceFile(ctx, headerReq)
	if !header.OK || header.FileVersion != current.FileVersion || header.BytesReturned != 20 {
		return fileError("rust_dap_invalid_elf", "validation", errors.New("ELF header unavailable through bounded worker source read"))
	}
	var headerBytes []byte
	switch header.OutputEncoding {
	case "utf-8":
		headerBytes = []byte(header.Output)
	case "base64":
		headerBytes, err = base64.StdEncoding.DecodeString(header.Output)
	default:
		err = errors.New("invalid worker ELF header encoding")
	}
	if err != nil || rustDebugELFHeader(headerBytes) != nil {
		return fileError("rust_dap_invalid_elf", "validation", errors.New("Rust binary header is not a supported architecture or executable"))
	}
	if os.Geteuid() != 0 {
		return fileError("dap_containment_unavailable", "dependency", errors.New("Rust debugger requires executor-owned, atomically spawned cgroup-v2 containment"))
	}
	adapter := s.rustLLDBDAPBinary // test-only fixed fixture
	if adapter == "" {
		adapter, err = systemexec.RustLLDBDAP()
		if err != nil {
			return fileError("rust_lldb_dap_unavailable", "dependency", err)
		}
	}
	auditObject := workspace + "\x00" + relative + "\x00rust/exec"
	if blocked := s.beginActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	defer func() {
		response = s.finishActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger", started, response)
	}()
	group, err := createDAPCgroup()
	if err != nil {
		return fileError("dap_containment_unavailable", "dependency", err)
	}
	assigned := false
	defer func() {
		if !assigned && group.path != "" {
			if cleanupErr := group.abortBeforeLaunch(); cleanupErr != nil {
				response.OK = false
				response.ErrorCode = "dap_containment_cleanup_uncertain"
				response.ErrorClass = "state"
				response.Error = fmt.Sprintf("Rust adapter cleanup was not verified: %v", cleanupErr)
			}
		}
	}()
	id, err := s.workerContainedDAPSession(req, workspace, adapter, group)
	if err != nil {
		failed := fileError("rust_dap_start_failed", "runtime", err)
		if id != "" {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				failed.ErrorCode, failed.Status, failed.SessionID = "debug_cleanup_uncertain", "cleanup_uncertain", id
				failed.Error = err.Error() + "; " + closeErr.Error()
			}
		}
		return failed
	}
	cleanup := true
	defer func() {
		if cleanup {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				response.OK, response.SessionID, response.Status = false, id, "cleanup_uncertain"
				response.ErrorCode, response.ErrorClass = "debug_cleanup_uncertain", "state"
				response.Error += "; " + closeErr.Error()
			}
		}
	}()
	e, err := s.sessions.get(req, id)
	if err != nil {
		return fileError("rust_dap_session_lost", "state", err)
	}
	e.stopMu.Lock()
	if !group.verified {
		e.stopMu.Unlock()
		return fileError("dap_containment_missing", "state", errors.New("Rust LLDB adapter not atomically spawned in private cgroup"))
	}
	e.mu.Lock()
	if e.closed || e.exited || e.pidPin == nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("rust_dap_adapter_exited_before_attach", "state", errors.New("LLDB adapter exited before pinning"))
	}
	copyFD, pinErr := unix.Dup(int(e.pidPin.Fd()))
	if pinErr != nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("rust_dap_pidfd_unavailable", "state", pinErr)
	}
	d := &dapState{
		conn: newStdioDAPConn(e.dapStdout, e.stdin), workspace: workspace, adapter: "rust/lldb-dap",
		adapterPID: e.pid, adapterPin: os.NewFile(uintptr(copyFD), "rust-dap-original-adapter-pidfd"),
		workerUID: s.workerUID, program: relative, sourceVersion: req.FileVersion,
		stage: "starting", containment: group,
	}
	e.dap = d
	e.mu.Unlock()
	e.stopMu.Unlock()
	assigned = true
	initCtx, cancel := context.WithTimeout(ctx, 13*time.Second)
	defer cancel()
	_, err = d.call(initCtx, "initialize", map[string]any{
		"adapterID": "lldb-dap", "clientID": "ai-server-agent", "linesStartAt1": true,
		"columnsStartAt1": true, "pathFormat": "path", "supportsVariableType": true,
		"supportsRunInTerminalRequest": false,
	})
	if err != nil {
		return fileError("rust_dap_initialize_failed", "runtime", err)
	}
	_, err = d.callDeferredLaunch(initCtx, map[string]any{
		"request": "launch", "type": "lldb-dap", "name": "Agent Rust target",
		"program": file, "cwd": workspace, "args": []string{},
		"stopOnEntry": req.DebugStopOnEntry,
	})
	if err != nil {
		return fileError("rust_dap_launch_failed", "runtime", err)
	}
	latest := s.workerWorkspaceFile(ctx, st)
	if !latest.OK || latest.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("Rust executable changed during LLDB launch"))
	}
	cleanup = false
	body, _ := json.Marshal(map[string]any{
		"adapter": "rust/lldb-dap", "mode": "exec", "configuration_required": true,
		"source_mapping": "workspace_paths_only", "additional_targets": false,
	})
	return Response{OK: true, Status: "starting", SessionID: id, FileVersion: req.FileVersion,
		OutputEncoding: "json", Output: string(body), BytesSeen: int64(len(body)), BytesReturned: int64(len(body))}
}

// Reject an arbitrary worker executable or script before the root executor
// starts any debugger. This checks only fixed-size ELF header identity, not
// untrusted DWARF or static analysis; LLDB owns the selected executable.
func rustDebugELFHeader(data []byte) error {
	if len(data) < 20 ||
		data[0] != 0x7f || data[1] != 'E' || data[2] != 'L' || data[3] != 'F' ||
		data[4] != 2 || data[5] != 1 { // supported 64-bit little-endian ELF
		return errors.New("Rust debug target is not a supported Linux ELF64")
	}
	machine := binary.LittleEndian.Uint16(data[18:20])
	wantMachine := uint16(0x3e) // EM_X86_64
	if runtime.GOARCH == "arm64" {
		wantMachine = 0xb7 // EM_AARCH64
	}
	kind := binary.LittleEndian.Uint16(data[16:18])
	if machine != wantMachine || (kind != 2 && kind != 3) { // ET_EXEC / ET_DYN
		return errors.New("Rust executable ELF architecture or type is incompatible")
	}
	return nil
}
