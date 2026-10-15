package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
	"golang.org/x/sys/unix"
)

// A single selected Node target uses TWO DAP protocol channels to ONE trusted
// js-debug adapter process. Both sockets have the original broker's pidfd,
// UID, principal, exact workspace and one cgroup; there is no second broker,
// user-chosen attach endpoint or adapter-supplied process execution command.
//
// Browser local-file mode has a distinct pinned Chromium and isolated profile.
// Extension-host launch/remote browser attach remain explicitly unsupported.
// Precompiled TypeScript uses the same Node process with a pinned adjacent source map;
// this profile never compiles or executes a project build script.
func (s *Server) debugLaunchNode(ctx context.Context, req Request) (response Response) {
	mode := req.DebugMode
	if mode == "" {
		mode = "node"
	}
	browserMode := mode == "browser"
	if mode != "node" && !browserMode {
		return fileError("javascript_dap_unsupported_mode", "validation", errors.New("only explicit local Node or file-only Browser launch modes are supported; no extension/attach"))
	}
	if browserMode && req.Language != "javascript" {
		return fileError("typescript_browser_source_map_unverified", "validation", errors.New("TypeScript browser mappings require separate provenance verification; supported Browser profile is JavaScript local HTML only"))
	}
	if req.Workspace == "" || req.DebugProgram == "" || req.FileVersion == "" {
		return fileError("debug_identity_required", "validation", errors.New("workspace, Node source and exact source file_version required"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || workspace != filepath.Clean(req.Workspace) {
		return fileError("invalid_workspace", "validation", errors.New("exact resolved workspace required"))
	}
	relative, err := safeWorkspaceRelativeFile(req.DebugProgram)
	validSource := validNodeDebugSource(relative)
	if browserMode {
		validSource = filepath.Ext(relative) == ".html"
	}
	if err != nil || !validSource {
		return fileError("javascript_dap_invalid_program", "validation", errors.New("Node requires .js/.mjs/.cjs or local Browser requires .html in the exact workspace"))
	}
	program := filepath.Join(workspace, relative)
	info, err := os.Lstat(program)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fileError("javascript_dap_invalid_program", "validation", errors.New("Node source must be a regular file without symlink"))
	}
	snapshot := req
	snapshot.Action, snapshot.Path, snapshot.FileVersion = "workspace_stat", relative, ""
	snapshot.Offset, snapshot.Limit = 0, 0
	stat := s.workerWorkspaceFile(ctx, snapshot)
	if !stat.OK {
		return stat
	}
	if stat.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("Node source was changed before launch"))
	}
	versions := map[string]string{relative: req.FileVersion}
	// Pin the actual source-map graph, never a merely adjacent file that
	// could refer to foreign source paths, stale JS or remote sources.
	if req.Language == "typescript" && !browserMode {
		versions, err = s.inspectPrecompiledTypeScriptMap(ctx, req, relative, req.FileVersion)
		if err != nil {
			return fileError("typescript_dap_source_map_untrusted", "validation", err)
		}
	}
	if os.Geteuid() != 0 {
		return fileError("dap_containment_unavailable", "dependency", errors.New("Node debugger requires executor-owned private cgroup v2; uncontained launch forbidden"))
	}
	node, entry := s.javascriptDAPNode, s.javascriptDAPCLI // fixture-only overrides
	if node != "" && entry == "" {
		return fileError("javascript_dap_fixture_incomplete", "dependency", errors.New("Node fixture requires exact JS debugger entrypoint"))
	}
	if node == "" {
		node, entry, err = systemexec.JavaScriptDAP()
		if err != nil {
			return fileError("javascript_dap_unavailable", "dependency", errors.New("trusted pinned Microsoft js-debug 1.140.0 and Node 24.18.1 are unavailable"))
		}
	}
	browserBinary := ""
	if browserMode {
		browserBinary = s.javascriptDAPBrowserBinary // test-only fixture
		if browserBinary == "" {
			browserBinary, err = systemexec.JavaScriptBrowser()
			if err != nil {
				return fileError("javascript_browser_unavailable", "dependency", err)
			}
		}
	}
	auditObject := workspace + "\x00" + relative + "\x00" + req.Language + "/" + mode
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
	// Abort before any user code can be launched, unless the authenticated
	// session owns this containment and its broker is registered to clean it.
	assigned := false
	defer func() {
		if !assigned && group != nil && group.path != "" {
			if stopErr := group.abortBeforeLaunch(); stopErr != nil {
				response.OK = false
				response.ErrorCode = "dap_containment_cleanup_uncertain"
				response.ErrorClass = "state"
				response.Error = fmt.Sprintf("Node private containment prelaunch cleanup unknown: %v", stopErr)
			}
		}
	}()
	browserProfileParent, browserProfile := "", ""
	if browserMode {
		browserProfileParent, browserProfile, err = createBrowserDAPProfile(s.workerUID, s.workerGID)
		if err != nil {
			return fileError("javascript_browser_profile_unavailable", "resource", err)
		}
		defer func() {
			if !assigned {
				if e := removeBrowserDAPProfile(browserProfileParent); e != nil {
					response.OK = false
					response.ErrorCode, response.ErrorClass = "javascript_browser_profile_cleanup_uncertain", "state"
					response.Error = e.Error()
				}
			}
		}()
	}
	// Node itself must create the socket; this randomized dedicated directory
	// is worker-owned but has no public/remote binding. Peer credentials are
	// always checked against the pidfd-pinned original adapter before use.
	dir, err := os.MkdirTemp("/tmp", ".asa-js-dap-")
	if err != nil {
		return fileError("javascript_dap_socket_unavailable", "resource", err)
	}
	removeDir := true
	defer func() {
		if removeDir {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := os.Chown(dir, int(s.workerUID), int(s.workerGID)); err != nil {
		return fileError("javascript_dap_socket_unavailable", "security", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fileError("javascript_dap_socket_unavailable", "security", err)
	}
	sock := filepath.Join(dir, "dap.sock")
	if len(sock) > terminalMaxUnixSocketPath {
		return fileError("javascript_dap_socket_unavailable", "validation", errors.New("private DAP socket exceeds Unix path limit"))
	}
	id, err := s.workerContainedSocketDAPSession(req, workspace, node, group, entry, sock)
	if err != nil {
		failed := fileError("javascript_dap_start_failed", "runtime", err)
		if id != "" {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				failed.ErrorCode = "debug_cleanup_uncertain"
				failed.Status = "cleanup_uncertain"
				failed.SessionID = id
				failed.Error = err.Error() + "; " + closeErr.Error()
			}
		}
		return failed
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
				response.Error = response.Error + "; " + closeErr.Error()
			}
		}
	}()
	e, err := s.sessions.get(req, id)
	if err != nil {
		return fileError("javascript_dap_session_lost", "state", err)
	}
	socketCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	rootConn, err := waitPinnedNodeDAPPeer(socketCtx, e, s.workerUID, sock)
	if err != nil {
		return fileError("javascript_dap_socket_provenance_unavailable", "state", err)
	}
	rootOwned := true
	defer func() {
		if rootOwned {
			_ = rootConn.Close()
		}
	}()
	e.stopMu.Lock()
	e.mu.Lock()
	if e.closed || e.exited || e.pidPin == nil || !group.verified {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("javascript_dap_adapter_identity_unproven", "state", errors.New("adapter exited or private cgroup unverified before attach"))
	}
	dup, dupErr := unix.Dup(int(e.pidPin.Fd()))
	if dupErr != nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("javascript_dap_pidfd_unavailable", "state", dupErr)
	}
	targetExecutable := node
	if browserMode {
		targetExecutable = browserBinary
	}
	d := &dapState{
		conn: rootConn, workspace: workspace, adapter: "javascript/js-debug", sourceLanguage: req.Language,
		adapterPID: e.pid, adapterPin: os.NewFile(uintptr(dup), "js-dap-original-adapter-pidfd"),
		workerUID: s.workerUID, program: relative, targetInterpreter: targetExecutable,
		sourceVersion: req.FileVersion, sourceVersions: versions, stage: "starting", containment: group,
		socketDir: dir, browserMode: browserMode, browserProfileDir: browserProfileParent,
	}
	e.dap = d
	e.mu.Unlock()
	e.stopMu.Unlock()
	assigned = true
	rootOwned = false
	removeDir = false // the original broker owns private socket removal
	protocolCtx, cancelProtocol := context.WithTimeout(ctx, 17*time.Second)
	defer cancelProtocol()
	adapterType := "pwa-node"
	if browserMode {
		adapterType = "pwa-chrome"
	}
	_, err = d.call(protocolCtx, "initialize", map[string]any{
		"adapterID": adapterType, "clientID": "ai-server-agent", "linesStartAt1": true,
		"columnsStartAt1": true, "pathFormat": "path", "supportsVariableType": true,
		"supportsRunInTerminalRequest": false,
	})
	if err != nil {
		return fileError("javascript_dap_initialize_failed", "runtime", err)
	}
	launch := map[string]any{
		"request": "launch", "type": "pwa-node", "name": "Agent Node", "program": program,
		"cwd": workspace, "runtimeExecutable": node, "console": "internalConsole",
		"autoAttachChildProcesses": false, "sourceMaps": true, "stopOnEntry": req.DebugStopOnEntry,
	}
	if browserMode {
		launch = map[string]any{
			"request": "launch", "type": "pwa-chrome", "name": "Agent isolated Browser",
			"file": program, "cwd": workspace, "runtimeExecutable": browserBinary,
			"runtimeArgs": []string{"--headless=new", "--disable-dev-shm-usage"},
			"port":        0, "webRoot": workspace, "userDataDir": browserProfile,
			"cleanUp": "wholeBrowser", "sourceMaps": true,
		}
	}
	_, err = d.callDeferredLaunch(protocolCtx, launch)
	if err != nil {
		return fileError("javascript_dap_parent_launch_failed", "runtime", err)
	}
	// js-debug's parent protocol must be configured before it discovers a
	// single Node target. Child breakpoints are configured later through
	// the SAME public debug_breakpoints/debug_configure contract.
	if _, err = d.call(protocolCtx, "configurationDone", map[string]any{}); err != nil {
		return fileError("javascript_dap_parent_configure_failed", "runtime", err)
	}
	targetID, err := d.receiveTypedNodeChild(protocolCtx)
	if err != nil {
		return fileError("javascript_dap_child_reverse_rejected", "security", err)
	}
	child, err := waitPinnedNodeDAPPeer(protocolCtx, e, s.workerUID, sock)
	if err != nil {
		return fileError("javascript_dap_child_provenance_unavailable", "state", err)
	}
	d.mu.Lock()
	d.parentConn = d.conn
	d.conn = child
	d.pending = nil
	d.seq = 0
	d.initialized = false
	d.pendingLaunchSeq = 0
	d.events = nil
	d.eventBytes = 0
	d.stage = "starting"
	d.mu.Unlock()
	_, err = d.call(protocolCtx, "initialize", map[string]any{
		"adapterID": adapterType, "clientID": "ai-server-agent", "linesStartAt1": true,
		"columnsStartAt1": true, "pathFormat": "path", "supportsVariableType": true,
		"supportsRunInTerminalRequest": false,
	})
	if err != nil {
		return fileError("javascript_dap_child_initialize_failed", "runtime", err)
	}
	_, err = d.callDeferredLaunch(protocolCtx, map[string]any{
		"request": "launch", "type": adapterType, "name": "Agent local JavaScript target", "__pendingTargetId": targetID,
	})
	if err != nil {
		return fileError("javascript_dap_child_launch_failed", "runtime", err)
	}
	stable := s.workerWorkspaceFile(ctx, snapshot)
	if !stable.OK || stable.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("Node source changed during debug launch"))
	}
	for path, version := range versions {
		if path == relative {
			continue
		}
		verify := snapshot
		verify.Path = path
		latest := s.workerWorkspaceFile(ctx, verify)
		if !latest.OK || latest.FileVersion != version {
			return fileError("typescript_dap_source_map_changed", "conflict", errors.New("mapped source or compiled source map changed during Node launch"))
		}
	}
	cleanup = false
	body := `{"adapter":"javascript/js-debug","mode":"node","configuration_required":true,"additional_targets":false,"language":"javascript"}`
	if browserMode {
		body = `{"adapter":"javascript/js-debug","mode":"browser","local_html_only":true,"remote_attach":false,"chromium_pipe_only":true,"configuration_required":true,"additional_targets":false,"language":"javascript"}`
	}
	if req.Language == "typescript" {
		body = `{"adapter":"javascript/js-debug","mode":"node","configuration_required":true,"additional_targets":false,"language":"typescript","precompiled_source_map":true}`
	}
	return Response{OK: true, Status: "starting", SessionID: id, FileVersion: req.FileVersion,
		OutputEncoding: "json", Output: body, BytesSeen: int64(len(body)), BytesReturned: int64(len(body))}
}

// The Node adapter's process event is optional. The accepted proof is
// original adapter pin plus atomic inherited cgroup membership. Never infer
// a safe Node target merely from a DAP stopped/threads event.
func (d *dapState) nodeContainmentReady() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.adapter == "javascript/js-debug" &&
		d.parentConn != nil && d.conn != nil &&
		d.containment != nil && d.containment.verified &&
		d.adapterPin != nil &&
		unix.PidfdSendSignal(int(d.adapterPin.Fd()), 0, nil, 0) == nil
}

func validNodeDebugSource(path string) bool {
	switch filepath.Ext(path) {
	case ".js", ".mjs", ".cjs":
		return true
	}
	return false
}

// A pinned same-UID/adapter-PID socket is the ONLY acceptable DAP transport.
// A malicious worker-controlled pathname alone has zero authorization: the
// kernel peer identity must match the original live broker process.
func waitPinnedNodeDAPPeer(ctx context.Context, e *stdioSession, uid uint32, socket string) (*net.UnixConn, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := net.DialTimeout("unix", socket, 120*time.Millisecond)
		if err == nil {
			u, ok := conn.(*net.UnixConn)
			if !ok {
				_ = conn.Close()
				return nil, errors.New("Node adapter endpoint is not AF_UNIX")
			}
			proof, pinErr := pinnedDAPPeer(e, u, uid)
			if pinErr != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("node_dap_original_adapter_peer_mismatch: %w", pinErr)
			}
			_ = proof.Close() // d.adapterPin retains the exact original pidfd separately
			return u, nil
		}
		e.mu.Lock()
		exited := e.exited || e.closed
		e.mu.Unlock()
		if exited {
			return nil, errors.New("Node adapter exited before authenticated Unix connection")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(35 * time.Millisecond):
		}
	}
}

// js-debug sends one narrow reverse startDebugging request containing an
// opaque internal target ID. This request may ONLY add a protocol connection
// to the same pinned adapter. It never supplies an OS command, attach address,
// path, environment, shell, port or a second session broker.
func (d *dapState) receiveTypedNodeChild(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.adapter != "javascript/js-debug" || d.parentConn != nil {
		return "", errors.New("node_dap_reverse_wrong_adapter_or_repeated_child")
	}
	if d.pendingLaunchSeq != 0 {
		// The pinned upstream adapter normally acknowledges parent launch
		// before issuing the typed child request. Never discard an in-flight
		// launch reply when promoting the single child DAP connection.
		return "", errors.New("node_dap_parent_launch_result_unproven")
	}
	deadline := time.Now().Add(8 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := d.conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	defer d.conn.SetDeadline(time.Time{})
	for n := 0; n < 32; n++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		raw, err := d.readFrame()
		if err != nil {
			return "", fmt.Errorf("node_dap_reverse_unverified: %w", err)
		}
		var packet dapPacket
		if err := json.Unmarshal(raw, &packet); err != nil {
			return "", err
		}
		switch packet.Type {
		case "event":
			d.acceptEvent(packet)
			if d.pinError != "" {
				return "", errors.New("node_dap_unproven_target_event")
			}
		case "response":
			if matched, err := d.acceptDeferredLaunchReply(packet); matched {
				if err != nil {
					return "", err
				}
			} else {
				return "", errors.New("node_dap_unexpected_parent_reply")
			}
		case "request":
			if packet.Seq < 1 || packet.Command != "startDebugging" {
				return "", errors.New("node_dap_untrusted_reverse_operation")
			}
			// No adapter-origin command, port, environment, attach
			// address or executable selector may be smuggled through a
			// permitted startDebugging request. The pinned upstream Node
			// adapter supplies only type/name/opaque pending target ID.
			var request struct {
				Seq       int    `json:"seq"`
				Type      string `json:"type"`
				Command   string `json:"command"`
				Arguments struct {
					Request       string `json:"request"`
					Configuration struct {
						Type      string `json:"type"`
						Name      string `json:"name"`
						PendingID string `json:"__pendingTargetId"`
					} `json:"configuration"`
				} `json:"arguments"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil ||
				request.Seq != packet.Seq || request.Command != packet.Command || request.Type != "request" {
				return "", errors.New("node_dap_malformed_or_extra_reverse_payload")
			}
			token := request.Arguments.Configuration.PendingID
			wanted := "pwa-node"
			if d.browserMode {
				wanted = "pwa-chrome"
			}
			if request.Arguments.Request != "launch" || request.Arguments.Configuration.Type != wanted ||
				len(token) < 16 || len(token) > 128 {
				return "", errors.New("node_dap_reverse_outside_single_local_target")
			}
			for _, ch := range token {
				isHex := ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f'
				if d.browserMode {
					isHex = isHex || ch >= 'A' && ch <= 'F'
				}
				if !isHex {
					return "", errors.New("javascript_dap_nonopaque_target_token")
				}
			}
			d.seq++
			if err := dapFrame(d.conn, map[string]any{
				"seq": d.seq, "type": "response", "request_seq": packet.Seq,
				"command": "startDebugging", "success": true, "body": map[string]any{},
			}); err != nil {
				return "", fmt.Errorf("node_dap_reverse_response_unknown: %w", err)
			}
			return token, nil
		default:
			return "", errors.New("node_dap_unexpected_parent_packet")
		}
	}
	return "", errors.New("node_dap_reverse_event_limit")
}
