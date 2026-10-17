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

// HTTP mode is intentionally restricted to an existing worker Service Job
// running one exact PHP built-in local server, with the Xdebug client Unix
// endpoint and trigger predeclared in its original service startup command.
// DAP owns only the adapter and private DBGp proxy, NEVER the service.
func (s *Server) debugLaunchPHPHTTP(ctx context.Context, req Request) (response Response) {
	if req.Root || req.Approval || req.DebugStopOnEntry || req.Workspace == "" || req.DebugProgram == "" ||
		req.FileVersion == "" || req.JobID == "" ||
		!validPHPHTTPTrigger(req.DebugHTTPTrigger) ||
		req.DebugHTTPPort < 1024 || req.DebugHTTPPort > 65535 {
		return fileError("php_http_identity_required", "validation",
			errors.New("exact worker workspace, service_job_id, source file_version, unprivileged local port and 32-hex trigger required"))
	}
	if _, err := safeID(req.JobID); err != nil {
		return fileError("php_http_identity_required", "validation", errors.New("PHP HTTP requires an exact numeric Service Job ID"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || workspace != filepath.Clean(req.Workspace) {
		return fileError("invalid_workspace", "validation", errors.New("exact PHP HTTP workspace required"))
	}
	relative, err := safeWorkspaceRelativeFile(req.DebugProgram)
	if err != nil || filepath.Ext(relative) != ".php" {
		return fileError("php_http_invalid_source", "validation", errors.New("exact workspace-relative PHP HTTP source required"))
	}
	file := filepath.Join(workspace, relative)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fileError("php_http_invalid_source", "validation", errors.New("regular non-symlink PHP source required"))
	}
	statReq := req
	statReq.Action, statReq.Path, statReq.FileVersion = "workspace_stat", relative, ""
	statReq.Offset, statReq.Limit = 0, 0
	stat := s.workerWorkspaceFile(ctx, statReq)
	if !stat.OK {
		return stat
	}
	if stat.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("PHP HTTP source changed before debug launch"))
	}
	if os.Geteuid() != 0 {
		return fileError("dap_containment_unavailable", "dependency", errors.New("PHP HTTP DAP requires root-owned adapter cgroup and separate systemd Job service"))
	}
	node, entrypoint, php, module := s.phpDAPNodeBinary, s.phpDAPCLIBinary, s.phpDAPNativeBinary, s.phpDAPXdebugModule
	if node != "" || entrypoint != "" || php != "" || module != "" {
		if node == "" || entrypoint == "" || php == "" || module == "" {
			return fileError("php_dap_fixture_incomplete", "dependency", errors.New("test-only PHP adapter fixture must be complete"))
		}
	} else {
		node, entrypoint, php, module, err = systemexec.PHPDAP()
		if err != nil {
			return fileError("php_xdebug_dap_unavailable", "dependency", err)
		}
	}
	auditObject := workspace + "\x00" + relative + "\x00php/http\x00job/" + req.JobID
	if blocked := s.beginActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	defer func() {
		response = s.finishActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger", started, response)
	}()
	// Validate protected Job claim, live systemd identity, original runner,
	// one trusted PHP -S process, its argv, cgroup and loopback listen inode.
	target, err := s.pinPHPHTTPServiceTarget(ctx, req, workspace, php, module)
	if err != nil {
		return fileError("php_http_service_unverified", "authorization", err)
	}
	target.program = relative
	target.sourceInfo = info
	assigned := false
	defer func() {
		if !assigned {
			target.close()
		}
	}()
	// Important: private adapter group is NOT the Service Job cgroup.
	group, err := createDAPCgroup()
	if err != nil {
		return fileError("dap_containment_unavailable", "dependency", err)
	}
	defer func() {
		if !assigned && group.path != "" {
			if e := group.abortBeforeLaunch(); e != nil {
				response.OK = false
				response.ErrorCode, response.ErrorClass = "dap_containment_cleanup_uncertain", "state"
				response.Error = fmt.Sprintf("PHP HTTP prelaunch adapter cgroup cleanup uncertain: %v", e)
			}
		}
	}()
	proxy, err := createPHPHTTPDBGpProxy(s.workerUID, s.workerGID, target, file)
	if err != nil {
		return fileError("php_http_unix_socket_unavailable", "dependency", err)
	}
	target.proxy = proxy
	defer func() {
		if !assigned {
			if e := proxy.removeAfterStop(); e != nil {
				response.OK = false
				response.ErrorCode, response.ErrorClass = "php_http_proxy_cleanup_uncertain", "state"
				response.Error = e.Error()
			}
		}
	}()
	id, err := s.workerContainedDAPSession(req, workspace, node, group, entrypoint)
	if err != nil {
		bad := fileError("php_http_adapter_start_failed", "runtime", err)
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
				response.ErrorCode, response.ErrorClass = "debug_cleanup_uncertain", "state"
				response.Error += "; " + closeErr.Error()
			}
		}
	}()
	e, err := s.sessions.get(req, id)
	if err != nil {
		return fileError("php_http_adapter_session_missing", "state", err)
	}
	e.stopMu.Lock()
	if !group.verified {
		e.stopMu.Unlock()
		return fileError("dap_containment_missing", "state", errors.New("PHP HTTP adapter not atomically contained"))
	}
	e.mu.Lock()
	if e.closed || e.exited || e.pidPin == nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("php_http_adapter_exited_early", "state", errors.New("original PHP HTTP adapter exited before DAP handshake"))
	}
	fd, err := unix.Dup(int(e.pidPin.Fd()))
	if err != nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("php_http_adapter_pidfd_unavailable", "state", err)
	}
	d := &dapState{
		conn: newStdioDAPConn(e.dapStdout, e.stdin), workspace: workspace,
		adapter: "php/xdebug", adapterPID: e.pid,
		adapterPin: os.NewFile(uintptr(fd), "php-http-original-dap-adapter-pidfd"),
		workerUID:  s.workerUID, program: relative, targetInterpreter: php,
		sourceVersion: req.FileVersion, sourceVersions: map[string]string{relative: req.FileVersion},
		stage: "starting", containment: group, phpProxy: proxy, phpHTTP: target,
	}
	e.dap = d
	e.mu.Unlock()
	e.stopMu.Unlock()
	assigned = true
	proxy.start(e, group)
	callCtx, cancel := context.WithTimeout(ctx, 16*time.Second)
	defer cancel()
	if _, err := d.call(callCtx, "initialize", map[string]any{
		"adapterID": "php", "clientID": "ai-server-agent", "linesStartAt1": true,
		"columnsStartAt1": true, "pathFormat": "path", "supportsVariableType": true,
		"supportsRunInTerminalRequest": false,
	}); err != nil {
		return fileError("php_http_initialize_failed", "runtime", err)
	}
	// Adapter listens ONLY on its private verified Unix side. Crucially it
	// must not start a PHP script or terminate the separately owned service.
	if _, err := d.callDeferredLaunch(callCtx, map[string]any{
		"request": "launch", "type": "php", "name": "Agent PHP HTTP service request",
		"hostname": "unix://" + proxy.adapterPath, "cwd": workspace,
		"console": "internalConsole", "stopOnEntry": false, "maxConnections": 1,
	}); err != nil {
		return fileError("php_http_adapter_launch_failed", "runtime", err)
	}
	if latest := s.workerWorkspaceFile(ctx, statReq); !latest.OK || latest.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("PHP HTTP source changed during DAP launch"))
	}
	if err := target.verifyService(ctx, s.cfg.WorkerUser); err != nil {
		return fileError("php_http_service_drift", "state", err)
	}
	cleanup = false
	body, _ := json.Marshal(map[string]any{
		"adapter": "php/xdebug", "mode": "http",
		"service_job_id": req.JobID, "request_mode": "single_verified_local_request",
		"configuration_required": true, "http_request_triggered": false,
		"service_ownership": "independent_service_job",
	})
	return Response{OK: true, Status: "starting", SessionID: id,
		FileVersion: req.FileVersion, OutputEncoding: "json", Output: string(body),
		BytesSeen: int64(len(body)), BytesReturned: int64(len(body))}
}
