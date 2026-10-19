package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ach1992/ai-server-agent/internal/systemexec"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Delve/DAP is worker-only, launch-only for the first reference adapter.
// DAP protocol operations reuse the same #66 stdio process broker for process
// identity, principal/workspace binding, bounded capacity, audit, and cleanup.
// A private Unix socket is transport only: no second session registry/listener.
func (s *Server) debugAction(ctx context.Context, req Request) Response {
	if req.Root || req.Approval {
		return fileError("debug_worker_only", "authorization", errors.New("debugger does not accept root or privilege approval"))
	}
	if req.PrincipalID == "" || req.PrincipalClass == "" {
		return fileError("debug_unauthorized", "authorization", errors.New("authenticated principal required"))
	}
	switch req.Action {
	case "debug_adapter_status":
		if req.Language == "shell" {
			_, _, err := shellDAPTrustedRuntime()
			available := err == nil
			payload, _ := json.Marshal(map[string]any{
				"adapter": "shell/bash-trap", "installed": available,
				"modes": []string{"bash"}, "attach_supported": false, "privileged": false,
				"evaluation_supported": false, "subshell_stepping": false,
				"provisioning": "bundled_verified_agent_and_system_bash",
				"containment":  "executor_owned_private_cgroup_v2_required",
			})
			state := "unavailable"
			if available {
				state = "installed_containment_unverified"
			}
			return Response{OK: true, Status: state, Output: string(payload), OutputEncoding: "json", BytesSeen: int64(len(payload)), BytesReturned: int64(len(payload))}
		}
		if req.Language == "javascript" || req.Language == "typescript" {
			_, _, trustErr := systemexec.JavaScriptDAP()
			available := trustErr == nil
			if s.javascriptDAPNode != "" && s.javascriptDAPCLI != "" {
				available = true
			}
			modes := []string{"node"}
			if req.Language == "javascript" {
				_, browserErr := systemexec.JavaScriptBrowser()
				if browserErr == nil || s.javascriptDAPBrowserBinary != "" {
					modes = append(modes, "browser")
				}
			}
			body, _ := json.Marshal(map[string]any{
				"adapter": "javascript/js-debug", "installed": available,
				"modes": modes, "attach_supported": false,
				"precompiled_typescript_source_maps": req.Language == "typescript",
				"privileged":                         false, "provisioning": "optional_root_owned_pinned_complete_js_debug",
				"containment": "executor_owned_private_cgroup_v2_required",
			})
			state := "unavailable"
			if available {
				state = "installed_containment_unverified"
			}
			return Response{OK: true, Status: state, Output: string(body), OutputEncoding: "json", BytesSeen: int64(len(body)), BytesReturned: int64(len(body))}
		}
		if req.Language == "python" {
			_, _, err := systemexec.PythonDebugpy()
			available := err == nil
			if s.pythonDebugpyBinary != "" && s.pythonDebugpyModules != "" {
				available = true
			}
			payload := map[string]any{"adapter": "python/debugpy", "installed": available,
				"modes": []string{"script", "module", "test"}, "attach_supported": false,
				"privileged": false, "provisioning": "optional_root_owned_pinned_wheel_and_interpreter",
				"containment": "root_owned_cgroup_v2_required_before_launch"}
			b, _ := json.Marshal(payload)
			status := "unavailable"
			if available {
				status = "installed_containment_unverified"
			}
			return Response{OK: true, Status: status, Output: string(b), OutputEncoding: "json", BytesReturned: int64(len(b)), BytesSeen: int64(len(b))}
		}
		if req.Language == "php" {
			_, _, _, _, trustErr := systemexec.PHPDAP()
			available := trustErr == nil || (s.phpDAPNodeBinary != "" && s.phpDAPCLIBinary != "" && s.phpDAPNativeBinary != "" && s.phpDAPXdebugModule != "")
			payload, _ := json.Marshal(map[string]any{
				"adapter": "php/xdebug", "installed": available,
				"modes": []string{"cli"}, "attach_supported": false,
				"http_request_mode": "not_yet_verified", "privileged": false,
				"transport":   "single_verified_unix_dbgp_peer",
				"containment": "executor_owned_private_cgroup_v2_required",
			})
			status := "unavailable"
			if available {
				status = "installed_containment_unverified"
			}
			return Response{OK: true, Status: status, Output: string(payload), OutputEncoding: "json", BytesSeen: int64(len(payload)), BytesReturned: int64(len(payload))}
		}
		if req.Language == "rust" {
			_, trustErr := systemexec.RustLLDBDAP()
			available := trustErr == nil || s.rustLLDBDAPBinary != ""
			body, _ := json.Marshal(map[string]any{
				"adapter": "rust/lldb-dap", "installed": available,
				"modes": []string{"exec"}, "attach_supported": false,
				"privileged": false, "provisioning": "optional_verified_LLDB19_native_distribution",
				"containment": "executor_owned_private_cgroup_v2_required",
			})
			status := "unavailable"
			if available {
				status = "installed_containment_unverified"
			}
			return Response{OK: true, Status: status, Output: string(body), OutputEncoding: "json", BytesSeen: int64(len(body)), BytesReturned: int64(len(body))}
		}
		if req.Language != "" && req.Language != "go" {
			return fileError("unsupported_debug_language", "validation", errors.New("debugger readiness supports go, rust, php, python, shell, javascript and typescript only"))
		}
		_, err := s.debugBinary()
		available := err == nil
		detail := map[string]any{"adapter": "go/delve", "installed": available, "modes": []string{"exec"}, "attach_supported": false, "privileged": false, "provisioning": "optional_admin_owned_binary", "containment": "root_owned_cgroup_v2_required_before_launch"}
		b, _ := json.Marshal(detail)
		return Response{OK: true, Status: func() string {
			if available {
				return "installed_containment_unverified"
			}
			return "unavailable"
		}(), Output: string(b), OutputEncoding: "json", BytesReturned: int64(len(b)), BytesSeen: int64(len(b))}
	case "debug_launch":
		return s.debugLaunch(ctx, req)
	case "debug_status":
		return s.debugStatus(ctx, req)
	case "debug_action":
		return s.debugControl(ctx, req)
	case "debug_stop":
		return s.debugStop(ctx, req)
	default:
		return fileError("unsupported_debug_action", "validation", errors.New("unsupported debugger action"))
	}
}

func (s *Server) debugEntry(req Request) (*stdioSession, *dapState, error) {
	e, err := s.sessions.get(req, req.SessionID)
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	d := e.dap
	kind := e.kind
	e.mu.Unlock()
	if kind != "dap" || d == nil {
		return nil, nil, errSessionNotFound
	}
	// A broker exit is not yet a final DAP protocol classification:
	// complete terminal frames may still be buffered in the Unix socket.
	// Only debugStatus reconciles them under the per-session action lock.
	return e, d, nil
}

func (s *Server) debugBinary() (string, error) {
	if s.dlvBinary != "" {
		return s.dlvBinary, nil
	} // test-only fixture
	if binary := systemexec.First("/usr/local/bin/dlv", "/usr/bin/dlv"); binary != "" {
		return binary, nil
	}
	return "", errors.New("optional trusted root-owned /usr/local/bin/dlv or /usr/bin/dlv unavailable")
}

// createDAPListener is private executor-owned runtime state. A worker has
// traverse permission and may dial the socket, but cannot swap or unlink it.
// SO_PEERCRED must match the pidfd-pinned Delve child before any DAP bytes are
// trusted. Ordinary worker code must not be able to impersonate another owner.
func (s *Server) createDAPListener() (net.Listener, string, string, error) {
	dir, err := os.MkdirTemp("/tmp", ".asa-dap-")
	if err != nil {
		return nil, "", "", err
	}
	cleanup := func(e error) (net.Listener, string, string, error) { _ = os.RemoveAll(dir); return nil, "", "", e }
	if err = os.Chown(dir, os.Geteuid(), int(s.workerGID)); err != nil {
		return cleanup(err)
	}
	if err = os.Chmod(dir, 0710); err != nil {
		return cleanup(err)
	}
	socket := filepath.Join(dir, "dap.sock")
	if len(socket) > terminalMaxUnixSocketPath {
		return cleanup(errors.New("dap_socket_path_too_long"))
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return cleanup(err)
	}
	if err = os.Chown(socket, os.Geteuid(), int(s.workerGID)); err == nil {
		err = os.Chmod(socket, 0660)
	}
	if err != nil {
		_ = listener.Close()
		return cleanup(err)
	}
	return listener, dir, socket, nil
}

// The concrete adapter process must remain live after accepting AF_UNIX.
// DAP cleanup relies on root-owned cgroup v2 containment, not on a numeric
// process group or kernel-version-specific group-scoped pidfd signalling.
func adapterPidfdLiveAndGroupSafe(e *stdioSession) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pidPin == nil {
		return false
	}
	fd := int(e.pidPin.Fd())
	return unix.PidfdSendSignal(fd, 0, nil, 0) == nil
}

// Check the live pinned adapter and peer credentials while holding e.mu so
// Cmd.Wait/reaping cannot occur between those checks. Duplicate its pidfd for
// final validation of debuggee parent identity later in the DAP lifecycle.
func pinnedDAPPeer(e *stdioSession, conn *net.UnixConn, uid uint32) (*os.File, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pidPin == nil {
		return nil, errors.New("original adapter pidfd missing")
	}
	fd := int(e.pidPin.Fd())
	if unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		return nil, errors.New("adapter identity is no longer live")
	}
	if !peerIsWorkerDelve(conn, uid, e.pid) {
		return nil, errors.New("accepted peer is not pinned adapter")
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(dup), "dap-original-adapter-pidfd"), nil
}

func peerIsWorkerDelve(conn *net.UnixConn, uid uint32, pid int) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var readErr error
	err = raw.Control(func(fd uintptr) { cred, readErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	return err == nil && readErr == nil && cred != nil && cred.Uid == uid && int(cred.Pid) == pid
}

func (s *Server) debugLaunch(ctx context.Context, req Request) (response Response) {
	if req.Language == "shell" {
		return s.debugLaunchShell(ctx, req)
	}
	if req.Language == "javascript" || req.Language == "typescript" {
		return s.debugLaunchNode(ctx, req)
	}
	if req.Language == "python" {
		return s.debugLaunchPython(ctx, req)
	}
	if req.Language == "rust" {
		return s.debugLaunchRust(ctx, req)
	}
	if req.Language == "php" {
		return s.debugLaunchPHP(ctx, req)
	}
	if (req.Language != "" && req.Language != "go") || (req.DebugMode != "" && req.DebugMode != "exec") {
		return fileError("unsupported_debug_language_or_mode", "validation", errors.New("supported Go debug mode is exec; Python requires explicit trusted script/module/test mode"))
	}
	if req.Workspace == "" || req.DebugProgram == "" || req.FileVersion == "" {
		return fileError("debug_identity_required", "validation", errors.New("workspace, executable and exact executable file_version are required"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || workspace != filepath.Clean(req.Workspace) {
		return fileError("invalid_workspace", "validation", errors.New("resolved workspace identity mismatch"))
	}
	relative, err := safeWorkspaceRelativeFile(req.DebugProgram)
	if err != nil {
		return fileError("invalid_debug_program", "validation", errors.New("executable path must be workspace-relative and regular"))
	}
	filename := filepath.Join(workspace, relative)
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fileError("invalid_debug_program", "validation", errors.New("program must be a regular executable (no symlink) within the workspace"))
	}
	statReq := req
	statReq.Action = "workspace_stat"
	statReq.Path = relative
	statReq.FileVersion = ""
	statReq.Limit = 0
	statReq.Offset = 0
	stat := s.workerWorkspaceFile(ctx, statReq)
	if !stat.OK {
		return stat
	}
	if stat.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("executable changed before debug launch"))
	}
	if os.Geteuid() != 0 && s.dlvBinary == "" {
		return fileError("dap_containment_unavailable", "dependency", errors.New("production DAP requires root executor-owned cgroup v2 containment"))
	}
	binary, err := s.debugBinary()
	if err != nil {
		return fileError("delve_unavailable", "dependency", err)
	}
	if blocked := s.beginActionAudit(req, "debug_launch", "worker", workspace+"\x00"+relative, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	auditObject := workspace + "\x00" + relative
	defer func() {
		response = s.finishActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger", started, response)
	}()
	listener, dir, sock, err := s.createDAPListener()
	if err != nil {
		return fileError("debug_socket_unavailable", "resource", err)
	}
	cleanup := true
	defer func() {
		_ = listener.Close()
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	if unixLn, ok := listener.(*net.UnixListener); ok {
		_ = unixLn.SetDeadline(time.Now().Add(7 * time.Second))
	}
	// --client-addr only; Delve never exposes an unauthenticated TCP listener.
	id, err := s.workerStdioSession(req, "dap", workspace, binary, "dap", "--client-addr=unix:"+sock)
	if err != nil {
		failed := fileError("debug_start_failed", "runtime", err)
		if id != "" {
			if cleanupErr := s.workerStdioClose(req, id); cleanupErr != nil {
				failed.SessionID = id
				failed.Status = "cleanup_uncertain"
				failed.ErrorCode = "debug_cleanup_uncertain"
				failed.Error = err.Error() + "; " + cleanupErr.Error()
			}
		}
		return failed
	}
	defer func() {
		if cleanup {
			if cleanupErr := s.workerStdioClose(req, id); cleanupErr != nil {
				response.OK = false
				response.SessionID = id
				response.Status = "cleanup_uncertain"
				response.ErrorCode = "debug_cleanup_uncertain"
				response.ErrorClass = "state"
				response.Error = response.Error + "; " + cleanupErr.Error()
			}
		}
	}()
	e, err := s.sessions.get(req, id)
	if err != nil {
		return fileError("debug_session_lost", "runtime", err)
	}
	e.mu.Lock()
	pid := e.pid
	e.mu.Unlock()
	var stream *net.UnixConn
	var adapterPin *os.File
	defer func() {
		if adapterPin != nil {
			_ = adapterPin.Close()
		}
	}()
	for {
		if ctx.Err() != nil {
			return fileError("debug_start_cancelled", "runtime", ctx.Err())
		}
		conn, acceptErr := listener.(*net.UnixListener).AcceptUnix()
		if acceptErr != nil {
			return fileError("debug_adapter_not_connected", "runtime", acceptErr)
		}
		witness, peerErr := pinnedDAPPeer(e, conn, s.workerUID)
		if peerErr == nil {
			adapterPin = witness
			stream = conn
			break
		}
		_ = conn.Close()
		return fileError("debug_adapter_identity_invalid", "runtime", peerErr)
	}
	_ = listener.Close()
	d := &dapState{conn: stream, workspace: workspace, adapter: "go/delve", adapterPID: pid, workerUID: s.workerUID, program: relative, sourceVersion: req.FileVersion, stage: "starting", socketDir: dir}
	var containment *dapCgroup
	if os.Geteuid() == 0 {
		containment, err = createDAPCgroup()
		if err != nil {
			_ = stream.Close()
			return fileError("dap_containment_unavailable", "dependency", err)
		}
	}
	// Lock in the same order as broker wait/stop: the DAP consumer must be
	// installed before wait() snapshots consumers, or be rejected outright.
	e.stopMu.Lock()
	e.mu.Lock()
	if e.closed || e.exited {
		e.mu.Unlock()
		e.stopMu.Unlock()
		_ = stream.Close()
		if containment != nil {
			_ = containment.removeUnused()
		}
		return fileError("debug_adapter_exited_before_attach", "state", errors.New("adapter exited before DAP containment setup"))
	}
	e.mu.Unlock()
	if containment != nil {
		if joinErr := containment.joinOriginalAdapter(e); joinErr != nil {
			e.stopMu.Unlock()
			abortErr := containment.abortBeforeLaunch()
			_ = stream.Close()
			if abortErr != nil {
				return fileError("dap_containment_cleanup_uncertain", "state", fmt.Errorf("%v; %w", joinErr, abortErr))
			}
			return fileError("dap_containment_join_failed", "state", joinErr)
		}
	}
	e.mu.Lock()
	if e.closed || e.exited {
		e.mu.Unlock()
		e.stopMu.Unlock()
		if containment != nil {
			_ = containment.abortBeforeLaunch()
		}
		_ = stream.Close()
		return fileError("debug_adapter_exited_before_attach", "state", errors.New("adapter exited during containment attachment"))
	}
	d.containment = containment
	d.adapterPin = adapterPin
	e.dap = d
	adapterPin = nil
	e.mu.Unlock()
	e.stopMu.Unlock()
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = d.call(initCtx, "initialize", map[string]any{"adapterID": "go", "clientID": "ai-server-agent", "linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path", "supportsVariableType": true, "supportsRunInTerminalRequest": false})
	if err != nil {
		return fileError("debug_initialize_failed", "runtime", err)
	}
	_, err = d.call(initCtx, "launch", map[string]any{"mode": "exec", "program": filename, "stopOnEntry": req.DebugStopOnEntry, "outputMode": "remote"})
	if err != nil {
		return fileError("debug_launch_failed", "runtime", err)
	}
	// Delve executes the debuggee in a different process group. Do not give
	// callers a working debugger session until the child's own process
	// identity has been pinned. Otherwise a Delve crash could orphan it.
	pinCtx, pinCancel := context.WithTimeout(ctx, 2*time.Second)
	defer pinCancel()
	for {
		if pinErr := d.pinnedDebuggeeError(); pinErr == nil {
			break
		} else if pinErr.Error() != "dap_debuggee_identity_not_observed" {
			return fileError("debug_debuggee_identity_invalid", "state", pinErr)
		}
		if err := pinCtx.Err(); err != nil {
			return fileError("debug_debuggee_identity_unavailable", "state", err)
		}
		d.drain(pinCtx)
	}
	latest := s.workerWorkspaceFile(ctx, statReq)
	if !latest.OK || latest.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("executable changed during debug launch; disconnecting untrusted session"))
	}
	cleanup = false
	out := Response{OK: true, SessionID: id, Status: "starting", FileVersion: req.FileVersion, OutputEncoding: "json", Output: `{"adapter":"go/delve","mode":"exec","configuration_required":true}`}
	out.BytesReturned = int64(len(out.Output))
	out.BytesSeen = out.BytesReturned
	return out
}

// The adapter's own protocol events do not prove OS-level cleanup.
// Only broker-owned status queries may publish that separate evidence.
type dapRuntimeEvidence struct {
	adapterExited        bool
	cleanupVerified      bool
	adapterExitCode      int
	finalDrainIncomplete bool // final buffered frames not yet fully reconciled
}

func (s *Server) debugResult(req Request, d *dapState, result json.RawMessage, started time.Time, evidence ...*dapRuntimeEvidence) Response {
	out, err := normalizeDebugResult(d.workspace, result)
	if err != nil {
		return fileError("debug_result_invalid", "runtime", err)
	}
	stage, events, dropped := d.peek()
	if len(evidence) > 0 && evidence[0] != nil && evidence[0].finalDrainIncomplete && stage != "failed" {
		// Do not advertise a final protocol outcome while the bounded
		// post-reap scan has more inbound bytes or an unresolved partial
		// frame. The broker's OS cleanup evidence remains independent.
		stage = "uncertain"
	}
	originDropped := dropped
	converted := make([]json.RawMessage, 0, len(events))
	for _, event := range events {
		fixed, e := normalizeDebugResult(d.workspace, event)
		if e != nil {
			dropped++
			continue
		}
		converted = append(converted, fixed)
	}
	var body []byte
	for {
		payload := map[string]any{"result": json.RawMessage(out), "events": converted, "events_dropped": dropped, "adapter": d.adapter, "state": stage}
		if d.phpHTTP != nil {
			payload["http_request"] = d.phpHTTP.requestSnapshot()
			payload["service_owned"] = true
		}
		if len(evidence) > 0 && evidence[0] != nil {
			payload["adapter_exited"] = evidence[0].adapterExited
			if evidence[0].adapterExited {
				payload["cleanup_verified"] = evidence[0].cleanupVerified
				payload["adapter_exit_code"] = evidence[0].adapterExitCode
			}
		}
		body, err = json.Marshal(payload)
		if err != nil {
			return fileError("debug_response_invalid", "runtime", err)
		}
		if len(body) <= dapMaxReply {
			break
		}
		if len(converted) == 0 {
			return fileError("debug_result_too_large", "resource", errors.New("debugger result exceeds bounded output limit"))
		}
		converted[0] = nil
		converted = converted[1:]
		dropped++
	}
	d.acknowledge(events, originDropped)
	response := Response{OK: true, SessionID: req.SessionID, Status: stage, Output: string(body), OutputEncoding: "json", BytesSeen: int64(len(body)), BytesReturned: int64(len(body)), DurationMS: time.Since(started).Milliseconds(), FileVersion: d.sourceVersion}
	return response
}

func (s *Server) debugControl(ctx context.Context, req Request) (response Response) {
	e, d, err := s.debugEntry(req)
	if err != nil {
		return fileError("debug_session_not_found", "authorization", err)
	}
	d.actionMu.Lock()
	defer d.actionMu.Unlock()
	// A competing stop can retire the broker entry while this caller
	// waits for the per-DAP operation lock. Never act on a stale pointer.
	if _, err := s.sessions.get(req, req.SessionID); err != nil {
		return fileError("debug_session_not_found", "authorization", err)
	}
	e.mu.Lock()
	exited, cleanupErr := e.exited, e.cleanupErr
	e.mu.Unlock()
	if exited {
		if cleanupErr != nil {
			return fileError("debug_cleanup_uncertain", "state", errors.New("debugger adapter exited but contained process cleanup is unverified; inspect debug_status and reconcile with debug_stop"))
		}
		return fileError("debug_session_ended", "state", errors.New("debugger adapter exited; inspect debug_status then use debug_stop to retire its session before launching a new one"))
	}
	d.mu.Lock()
	unusable := d.pinError != "" || d.uncertain || d.passiveClosed
	d.mu.Unlock()
	if unusable {
		return fileError("debug_session_uncertain", "state", errors.New("DAP state or target identity unverified; only debug_status and debug_stop are permitted"))
	}
	method := req.DebugMethod
	// The Bash adapter uses an instrumentation trap, not a shell command
	// evaluator or a process-signal attachment. Unsupported operations are
	// refused before any wire send; they cannot poison a healthy session.
	if d.adapter == "shell/bash-trap" {
		switch method {
		case "pause", "evaluate", "exceptionInfo":
			return fileError("shell_dap_method_unsupported", "validation", errors.New("Bash v1 does not support asynchronous pause, arbitrary evaluation or exception introspection"))
		}
	}
	// The executable is compiled at an exact file_version; any source
	// changed after breakpoint registration must be surfaced, not silently
	// paired with stale symbol/line assumptions.
	if method != "breakpoints" {
		if err := s.checkDAPBreakpointSources(ctx, req, d); err != nil {
			return fileError("debug_source_changed", "conflict", err)
		}
	}
	if method == "http_request" {
		return s.debugPHPHTTPRequest(ctx, req, d)
	}
	if d.phpHTTP != nil {
		if err := d.phpHTTP.verifyService(ctx, s.cfg.WorkerUser); err != nil {
			return fileError("php_http_service_drift", "state", err)
		}
	}
	command := method
	var breakpointStat *Request
	var args any = map[string]any{}
	switch method {
	case "breakpoints":
		if d.configured {
			return fileError("debug_already_configured", "validation", errors.New("setBreakpoints belongs before configurationDone in this v1 adapter"))
		}
		relative, err := safeWorkspaceRelativeFile(req.Path)
		extension := ".go"
		if d.adapter == "python/debugpy" {
			extension = ".py"
		}
		if d.adapter == "rust/lldb-dap" {
			extension = ".rs"
		}
		if d.adapter == "php/xdebug" {
			extension = ".php"
		}
		sourceExt := filepath.Ext(relative)
		if d.adapter == "shell/bash-trap" {
			if relative != d.program || sourceExt != ".sh" && sourceExt != ".bash" {
				return fileError("shell_dap_source_mismatch", "validation", errors.New("Bash v1 only accepts breakpoints in its original script"))
			}
			extension = sourceExt
		}
		validJS := d.adapter == "javascript/js-debug" && (sourceExt == ".js" || sourceExt == ".mjs" || sourceExt == ".cjs" || (d.sourceLanguage == "typescript" && (sourceExt == ".ts" || sourceExt == ".tsx")))
		if err != nil || (d.adapter == "javascript/js-debug" && !validJS) ||
			(d.adapter != "javascript/js-debug" && sourceExt != extension) || len(req.DebugLines) > 32 || req.FileVersion == "" {
			return fileError("debug_invalid_breakpoints", "validation", errors.New("language-matching source file_version and <=32 breakpoint lines required"))
		}
		if d.adapter == "javascript/js-debug" && d.sourceLanguage == "typescript" &&
			(sourceExt == ".ts" || sourceExt == ".tsx") && d.sourceVersions[relative] == "" {
			return fileError("typescript_dap_unmapped_source", "validation", errors.New("TypeScript breakpoint source must be an original file in the pinned source map"))
		}
		if d.sourceVersions[relative] == "" && len(d.sourceVersions) >= 16 {
			return fileError("debug_source_limit", "resource", errors.New("at most 16 source files may have breakpoints in one debug session"))
		}
		for _, line := range req.DebugLines {
			if line < 1 || line > 1000000 {
				return fileError("debug_invalid_breakpoints", "validation", errors.New("DAP source lines start at 1"))
			}
		}
		statReq := req
		statReq.Action = "workspace_stat"
		statReq.Path = relative
		statReq.Offset = 0
		statReq.Limit = 0
		breakpointStat = &statReq
		stat := s.workerWorkspaceFile(ctx, statReq)
		if !stat.OK || stat.FileVersion != req.FileVersion {
			return fileError("debug_source_changed", "conflict", errors.New("source file identity changed before setting breakpoints"))
		}
		points := make([]map[string]int, 0, len(req.DebugLines))
		for _, line := range req.DebugLines {
			points = append(points, map[string]int{"line": line})
		}
		command = "setBreakpoints"
		args = map[string]any{"source": map[string]string{"path": filepath.Join(d.workspace, relative)}, "breakpoints": points, "sourceModified": false}
	case "configure":
		command = "configurationDone"
		if d.configured {
			return fileError("debug_already_configured", "validation", errors.New("debug configuration already completed"))
		}
		// js-debug's Node child does not reliably emit a DAP "process"
		// notification. Its safety proof is the original adapter pidfd,
		// authenticated two AF_UNIX connections and the kernel-verified
		// private cgroup. Require that proof before configurationDone
		// resumes any user code; never mistake missing process-event
		// metadata for permission to launch without containment.
		if d.adapter == "javascript/js-debug" && !d.nodeContainmentReady() {
			return fileError("javascript_dap_containment_unverified", "security",
				errors.New("Node debugger has no live pinned adapter and private cgroup; configuration denied"))
		}
	case "continue", "pause", "next", "stepIn", "stepOut", "stackTrace", "exceptionInfo":
		if !d.configured {
			return fileError("debug_not_configured", "state", errors.New("complete breakpoints/configure first"))
		}
		if req.DebugThreadID < 0 || (d.adapter != "javascript/js-debug" && req.DebugThreadID == 0) {
			return fileError("debug_thread_required", "validation", errors.New("valid adapter-specific DAP thread_id is required"))
		}
		args = map[string]any{"threadId": req.DebugThreadID}
		if method == "stackTrace" {
			args = map[string]any{"threadId": req.DebugThreadID, "startFrame": 0, "levels": 20}
		}
	case "threads":
		if !d.configured {
			return fileError("debug_not_configured", "state", errors.New("complete configuration first"))
		}
	case "scopes", "evaluate":
		if !d.configured {
			return fileError("debug_not_configured", "state", errors.New("complete configuration first"))
		}
		if req.DebugFrameID < 0 {
			return fileError("debug_frame_required", "validation", errors.New("valid frame id is required"))
		}
		args = map[string]any{"frameId": req.DebugFrameID}
		if method == "evaluate" {
			if len(req.DebugExpression) == 0 || len(req.DebugExpression) > 512 {
				return fileError("debug_invalid_expression", "validation", errors.New("expression must be 1..512 bytes and is potentially side-effecting"))
			}
			args = map[string]any{"expression": req.DebugExpression, "frameId": req.DebugFrameID, "context": "watch"}
		}
	case "variables":
		if !d.configured {
			return fileError("debug_not_configured", "state", errors.New("complete configuration first"))
		}
		if req.DebugReference < 1 {
			return fileError("debug_reference_required", "validation", errors.New("positive variablesReference is required"))
		}
		args = map[string]any{"variablesReference": req.DebugReference, "start": 0, "count": 40}
	default:
		return fileError("debug_method_unsupported", "validation", fmt.Errorf("unsupported debugger operation %q", method))
	}
	if blocked := s.beginActionAudit(req, "debug_action", "worker", req.SessionID+"\x00"+method, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	defer func() {
		response = s.finishActionAudit(req, "debug_action", "worker", req.SessionID+"\x00"+method, "developer_debugger", started, response)
	}()
	callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	out, err := d.call(callCtx, command, args)
	if err != nil {
		if d.adapter == "shell/bash-trap" && method == "configure" {
			return shellDAPUncertain(d, "shell_dap_configuration_uncertain", err)
		}
		return fileError("debug_action_uncertain", "runtime", err)
	}
	if breakpointStat != nil {
		stable := s.workerWorkspaceFile(ctx, *breakpointStat)
		if !stable.OK || stable.FileVersion != req.FileVersion {
			return fileError("debug_source_changed", "conflict", errors.New("source changed during DAP breakpoint registration; the debugger may already have applied earlier breakpoints; inspect or stop the session"))
		}
		if d.sourceVersions == nil {
			d.sourceVersions = make(map[string]string)
		}
		d.sourceVersions[breakpointStat.Path] = req.FileVersion
	}
	if method == "configure" {
		if d.adapter == "shell/bash-trap" {
			// The immutable Bash wrapper waits for root-mediated START
			// before reading ANY script bytes. configurationDone emitted
			// and pinned the original Bash child in its private cgroup.
			if err := d.pinnedDebuggeeError(); err != nil {
				return shellDAPUncertain(d, "shell_dap_debuggee_identity_unverified", err)
			}
			// The original Bash PID is now kernel-pinned but blocked
			// *before its first script command*. Check the exact current
			// source once more before granting the private activation.
			if err := s.checkDAPBreakpointSources(ctx, req, d); err != nil {
				return shellDAPUncertain(d, "debug_source_changed", err)
			}
			if _, err := d.call(callCtx, "asaActivate", map[string]any{}); err != nil {
				return shellDAPUncertain(d, "shell_dap_activation_uncertain", err)
			}
		}
		if d.phpHTTP != nil {
			if err := d.phpProxy.terminalFailure(); err != nil {
				return fileError("php_http_dbgp_provenance_failed", "state", err)
			}
		}
		// debugpy sends its real process event after configurationDone's
		// response. Never expose control over a Python target until its
		// pidfd, worker UID, trusted interpreter and dedicated cgroup
		// membership have been verified by the common DAP event consumer.
		if d.adapter == "php/xdebug" && d.phpHTTP == nil {
			if d.phpProxy == nil {
				return fileError("php_dap_missing_verified_proxy", "state", errors.New("PHP DBGp verifier absent; stop session"))
			}
			verifyCtx, verifyCancel := context.WithTimeout(ctx, 5*time.Second)
			proofErr := d.phpProxy.await(verifyCtx)
			verifyCancel()
			if proofErr != nil {
				d.mu.Lock()
				d.uncertain, d.stage = true, "uncertain"
				d.mu.Unlock()
				return fileError("php_dap_unverified_debuggee_identity", "state", proofErr)
			}
		}
		if d.adapter == "python/debugpy" {
			deadline := time.Now().Add(3 * time.Second)
			for {
				pinErr := d.pinnedDebuggeeError()
				if pinErr == nil {
					break
				}
				if ctx.Err() != nil || time.Now().After(deadline) || pinErr.Error() != "dap_debuggee_identity_not_observed" {
					d.mu.Lock()
					d.uncertain = true
					d.stage = "uncertain"
					d.mu.Unlock()
					return fileError("python_dap_debuggee_identity_unverified", "state", pinErr)
				}
				d.drain(ctx)
			}
		}
		d.mu.Lock()
		d.configured = true
		if d.stage == "starting" {
			d.stage = "running"
		}
		d.mu.Unlock()
	}
	return s.debugResult(req, d, out, started)
}

func (s *Server) checkDAPBreakpointSources(ctx context.Context, req Request, d *dapState) error {
	for path, version := range d.sourceVersions {
		statReq := Request{Action: "workspace_stat", Workspace: req.Workspace, Path: path, PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass, RequestID: req.RequestID}
		stat := s.workerWorkspaceFile(ctx, statReq)
		if !stat.OK || stat.FileVersion != version {
			return fmt.Errorf("source %q changed since breakpoint registration; recompile/relaunch against current source or stop debugger", path)
		}
	}
	return nil
}

func (s *Server) debugStatus(ctx context.Context, req Request) (response Response) {
	e, d, err := s.debugEntry(req)
	if err != nil {
		return fileError("debug_session_not_found", "authorization", err)
	}
	d.actionMu.Lock()
	defer d.actionMu.Unlock()
	// Recheck ownership/presence after waiting for a concurrent stop.
	// Otherwise a stale in-memory pointer could report a retired session
	// as live, or attempt a second broker removal with false uncertainty.
	if _, err := s.sessions.get(req, req.SessionID); err != nil {
		return fileError("debug_session_not_found", "authorization", err)
	}
	if blocked := s.beginActionAudit(req, "debug_status", "worker", req.SessionID, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	defer func() {
		response = s.finishActionAudit(req, "debug_status", "worker", req.SessionID, "developer_debugger", started, response)
	}()
	// A reaped adapter cannot execute another request, but its authenticated
	// Unix socket may still hold valid terminal events or a truncated final
	// frame. Consume only inbound frames under a bounded, nontransmitting
	// exit drain before publishing the final protocol classification.
	e.mu.Lock()
	exited, exitCode, cleanupErr := e.exited, e.exitCode, e.cleanupErr
	e.mu.Unlock()
	if !exited {
		d.drain(ctx)
		e.mu.Lock()
		exited, exitCode, cleanupErr = e.exited, e.exitCode, e.cleanupErr
		e.mu.Unlock()
	}
	finalDrainComplete := false
	if exited {
		finalDrainComplete = d.drainAfterExit(ctx)
		d.mu.Lock()
		if cleanupErr != nil {
			d.uncertain = true
			d.stage = "uncertain"
		} else if finalDrainComplete {
			// Passive EOF is NOT an unknown sent command. The final stream
			// must be reconciled before classifying successful OS cleanup.
			d.passiveClosed = false
			if d.stage != "terminated" && !d.uncertain && d.pinError == "" {
				d.stage = "failed"
			}
		}
		d.mu.Unlock()
	}
	if d.phpHTTP != nil && d.phpProxy != nil {
		if err := d.phpProxy.terminalFailure(); err != nil {
			d.mu.Lock()
			d.stage = "failed"
			d.mu.Unlock()
		}
	}
	result := s.debugResult(req, d, json.RawMessage(`{}`), started, &dapRuntimeEvidence{
		adapterExited: exited, cleanupVerified: exited && cleanupErr == nil, adapterExitCode: exitCode,
		finalDrainIncomplete: exited && !finalDrainComplete,
	})
	result.Running = boolPtr(!exited)
	result.ExitCode = exitCode
	return result
}

func (s *Server) debugStop(ctx context.Context, req Request) (response Response) {
	e, d, err := s.debugEntry(req)
	if err != nil {
		return fileError("debug_session_not_found", "authorization", err)
	}
	d.actionMu.Lock()
	defer d.actionMu.Unlock()
	// Recheck ownership/presence after waiting for a concurrent stop.
	// Otherwise a stale in-memory pointer could report a retired session
	// as live, or attempt a second broker removal with false uncertainty.
	if _, err := s.sessions.get(req, req.SessionID); err != nil {
		return fileError("debug_session_not_found", "authorization", err)
	}
	if blocked := s.beginActionAudit(req, "debug_stop", "worker", req.SessionID, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	defer func() {
		response = s.finishActionAudit(req, "debug_stop", "worker", req.SessionID, "developer_debugger", started, response)
	}()
	e.mu.Lock()
	alreadyExited := e.exited
	e.mu.Unlock()
	// An already-reaped adapter cannot acknowledge disconnect. Only the
	// broker's actual process/containment cleanup can prove a safe stop.
	var dapErr error
	d.mu.Lock()
	passiveClosed := d.passiveClosed
	d.mu.Unlock()
	// Service-owned HTTP must only detach the debugger; never terminate the
	// independent PHP Service Job. Preserve its exact pre-stop identity.
	isHTTP := d.phpHTTP != nil
	var priorService phpHTTPServiceUnit
	var priorPHP, priorPort int
	var priorExe string
	phpFD := -1
	var phpPinErr error
	if isHTTP {
		priorService, priorPHP, priorPort, priorExe, phpFD, phpPinErr = d.phpHTTP.pinServiceForDetach()
	}
	if phpFD >= 0 {
		defer unix.Close(phpFD)
	}
	// No wire request is valid after passive EOF; the broker remains the
	// sole process-containment cleanup authority in this case.
	if !alreadyExited && !passiveClosed {
		stopCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		_, dapErr = d.call(stopCtx, "disconnect", map[string]any{"terminateDebuggee": !isHTTP})
	}
	if isHTTP {
		d.phpHTTP.finishHTTPRequestAfterDetach() // HTTP client only; never signal Service Job
	}
	err = s.workerStdioClose(req, e.id)
	if err != nil {
		return fileError("debug_stop_uncertain", "state", err)
	}
	// The pidfd-pinned broker removes this session's private socket only
	// after confirmed process-group cleanup; do not race a second removal.
	response = Response{OK: dapErr == nil, SessionID: req.SessionID, Status: "terminated", DurationMS: time.Since(started).Milliseconds()}
	if isHTTP {
		serviceErr := phpPinErr
		if serviceErr == nil {
			serviceErr = verifyPHPHTTPServiceAfterDetach(ctx, priorService, s.cfg.WorkerUser, priorPHP, phpFD, priorExe, priorPort)
		}
		if serviceErr != nil {
			response.OK = false
			response.Status = "cleanup_confirmed_service_liveness_unverified"
			response.ErrorCode, response.ErrorClass = "php_http_service_drift", "state"
			response.Error = "DAP resources cleaned; independently owned PHP HTTP service identity or liveness unverified: " + serviceErr.Error()
			return response
		}
	}
	if alreadyExited {
		response.Status = "cleanup_confirmed_adapter_already_exited"
		return response
	}
	if passiveClosed {
		response.Status = "cleanup_confirmed_adapter_stream_closed"
		return response
	}
	if dapErr != nil {
		response.OK = false
		response.Status = "cleanup_confirmed_dap_disconnect_uncertain"
		response.ErrorCode = "debug_disconnect_uncertain"
		response.ErrorClass = "state"
		response.Error = dapErr.Error()
	}
	return response
}
