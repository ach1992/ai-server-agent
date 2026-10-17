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

// PHP CLI launch owns exactly one explicit PHP script and its transitive
// descendants. The existing DAP broker launches a pinned Node adapter in an
// atomically created root-executor private cgroup; an authenticated in-process
// Unix DBGp broker validates the exact PHP peer before forwarding protocol.
// HTTP Job/Service DAP is a separate source extension that retains service ownership; it is not a shipped Stable contract.
func (s *Server) debugLaunchPHP(ctx context.Context, req Request) (response Response) {
	if req.DebugMode == "http" {
		return s.debugLaunchPHPHTTP(ctx, req)
	}
	if req.DebugMode != "" && req.DebugMode != "cli" {
		return fileError("php_dap_unsupported_mode", "validation", errors.New("PHP supports contained cli and verified local service-owned http modes; arbitrary remote/process attach is unsupported"))
	}
	if req.Workspace == "" || req.DebugProgram == "" || req.FileVersion == "" {
		return fileError("debug_identity_required", "validation", errors.New("exact PHP source, workspace and file_version required"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || workspace != filepath.Clean(req.Workspace) {
		return fileError("invalid_workspace", "validation", errors.New("exact PHP workspace required"))
	}
	relative, err := safeWorkspaceRelativeFile(req.DebugProgram)
	if err != nil || filepath.Ext(relative) != ".php" {
		return fileError("php_dap_invalid_source", "validation", errors.New("PHP CLI requires one workspace-relative .php file"))
	}
	file := filepath.Join(workspace, relative)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fileError("php_dap_invalid_source", "validation", errors.New("PHP source must be regular and not a symlink"))
	}
	statReq := req
	statReq.Action, statReq.Path, statReq.FileVersion = "workspace_stat", relative, ""
	statReq.Offset, statReq.Limit = 0, 0
	stat := s.workerWorkspaceFile(ctx, statReq)
	if !stat.OK {
		return stat
	}
	if stat.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("PHP source changed before launch"))
	}
	if os.Geteuid() != 0 {
		return fileError("dap_containment_unavailable", "dependency", errors.New("PHP CLI needs root-executor-owned private cgroup-v2 containment"))
	}
	node, entrypoint, php, module := s.phpDAPNodeBinary, s.phpDAPCLIBinary, s.phpDAPNativeBinary, s.phpDAPXdebugModule // test-only fixtures
	if node != "" || entrypoint != "" || php != "" || module != "" {
		if node == "" || entrypoint == "" || php == "" || module == "" {
			return fileError("php_dap_fixture_incomplete", "dependency", errors.New("PHP fixture must provide the complete trusted runtime"))
		}
	} else {
		node, entrypoint, php, module, err = systemexec.PHPDAP()
		if err != nil {
			return fileError("php_xdebug_dap_unavailable", "dependency", err)
		}
	}
	auditObject := workspace + "\x00" + relative + "\x00php/cli"
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
				response.Error = fmt.Sprintf("unverified PHP cgroup prelaunch cleanup: %v", cleanupErr)
			}
		}
	}()
	proxy, err := createPHPDBGpProxy(s.workerUID, s.workerGID, php, file)
	if err != nil {
		return fileError("php_dap_unix_socket_unavailable", "dependency", err)
	}
	defer func() {
		if !assigned {
			if err := proxy.removeAfterStop(); err != nil {
				response.OK = false
				response.ErrorCode = "php_dap_proxy_cleanup_uncertain"
				response.ErrorClass = "state"
				response.Error = "unverified PHP private socket prelaunch cleanup: " + err.Error()
			}
		}
	}()
	id, err := s.workerContainedDAPSession(req, workspace, node, group, entrypoint)
	if err != nil {
		bad := fileError("php_dap_start_failed", "runtime", err)
		if id != "" {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				bad.ErrorCode, bad.Status, bad.SessionID = "debug_cleanup_uncertain", "cleanup_uncertain", id
				bad.Error = err.Error() + "; " + closeErr.Error()
			}
		}
		return bad
	}
	cleanup := true
	defer func() {
		if cleanup {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				response.OK = false
				response.SessionID = id
				response.Status = "cleanup_uncertain"
				response.ErrorCode = "debug_cleanup_uncertain"
				response.ErrorClass = "state"
				response.Error += "; " + closeErr.Error()
			}
		}
	}()
	e, err := s.sessions.get(req, id)
	if err != nil {
		return fileError("php_dap_session_lost", "state", err)
	}
	e.stopMu.Lock()
	if !group.verified {
		e.stopMu.Unlock()
		return fileError("dap_containment_missing", "state", errors.New("Node PHP adapter not atomically spawned in private cgroup"))
	}
	e.mu.Lock()
	if e.closed || e.exited || e.pidPin == nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("php_dap_adapter_exited_before_attach", "state", errors.New("PHP debugger adapter already exited"))
	}
	fd, pinErr := unix.Dup(int(e.pidPin.Fd()))
	if pinErr != nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("php_dap_pidfd_unavailable", "state", pinErr)
	}
	d := &dapState{
		conn: newStdioDAPConn(e.dapStdout, e.stdin), workspace: workspace,
		adapter: "php/xdebug", adapterPID: e.pid, adapterPin: os.NewFile(uintptr(fd), "php-dap-original-adapter-pidfd"),
		workerUID: s.workerUID, program: relative, targetInterpreter: php, sourceVersion: req.FileVersion,
		sourceVersions: map[string]string{relative: req.FileVersion}, stage: "starting", containment: group,
		socketDir: proxy.dir, phpProxy: proxy,
	}
	e.dap = d
	e.mu.Unlock()
	e.stopMu.Unlock()
	assigned = true
	proxy.start(e, group)
	callCtx, cancel := context.WithTimeout(ctx, 16*time.Second)
	defer cancel()
	_, err = d.call(callCtx, "initialize", map[string]any{
		"adapterID": "php", "clientID": "ai-server-agent",
		"linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path",
		"supportsVariableType": true, "supportsRunInTerminalRequest": false,
	})
	if err != nil {
		return fileError("php_dap_initialize_failed", "runtime", err)
	}
	_, err = d.callDeferredLaunch(callCtx, map[string]any{
		"request": "launch", "type": "php", "name": "Agent PHP CLI",
		"hostname": "unix://" + proxy.adapterPath, "program": file, "cwd": workspace,
		"runtimeExecutable": php, "runtimeArgs": []string{
			"-n", "-d", "zend_extension=" + module,
			"-d", "xdebug.mode=debug", "-d", "xdebug.start_with_request=yes",
			"-d", "xdebug.discover_client_host=0", "-d", "xdebug.client_host=unix://" + proxy.clientPath,
		},
		"console": "internalConsole", "stopOnEntry": req.DebugStopOnEntry, "maxConnections": 1,
	})
	if err != nil {
		return fileError("php_dap_launch_failed", "runtime", err)
	}
	latest := s.workerWorkspaceFile(ctx, statReq)
	if !latest.OK || latest.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("PHP source changed during launch"))
	}
	cleanup = false
	payload, _ := json.Marshal(map[string]any{"adapter": "php/xdebug", "mode": "cli",
		"configuration_required": true, "http_request_mode": "unsupported",
		"verified_socket_peer_required": true})
	return Response{OK: true, Status: "starting", SessionID: id, FileVersion: req.FileVersion,
		OutputEncoding: "json", Output: string(payload), BytesSeen: int64(len(payload)), BytesReturned: int64(len(payload))}
}
