package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
	"golang.org/x/sys/unix"
)

// Python is an explicit, source-version-bound worker DAP mode. The #66
// broker owns its pinned adapter; its private stdio pipe is never exposed as
// a network endpoint. A root-executor-owned cgroup MUST contain every launched
// debuggee and descendant; no uncontained fixture fallback can run targets.
func (s *Server) debugLaunchPython(ctx context.Context, req Request) (response Response) {
	mode := req.DebugMode
	if mode == "" {
		mode = "script"
	}
	if mode != "script" && mode != "module" && mode != "test" {
		return fileError("python_dap_unsupported_mode", "validation", errors.New("supported Python modes: script, module, test"))
	}
	if req.Workspace == "" || req.DebugProgram == "" || req.FileVersion == "" {
		return fileError("debug_identity_required", "validation", errors.New("workspace, Python source, exact file_version required"))
	}
	workspace, err := s.workspacePath(req.Workspace, true)
	if err != nil || workspace != filepath.Clean(req.Workspace) {
		return fileError("invalid_workspace", "validation", errors.New("resolved workspace identity mismatch"))
	}
	relative, err := safeWorkspaceRelativeFile(req.DebugProgram)
	if err != nil || filepath.Ext(relative) != ".py" {
		return fileError("python_dap_invalid_source", "validation", errors.New("Python source must be a regular workspace-relative .py file"))
	}
	if (mode == "module" || mode == "test") && filepath.Base(relative) == "__init__.py" {
		return fileError("python_dap_invalid_module", "validation", errors.New("explicit module/test target cannot be a package initializer"))
	}
	file := filepath.Join(workspace, relative)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fileError("python_dap_invalid_source", "validation", errors.New("Python source must be regular; no symlinks"))
	}
	st := req
	st.Action = "workspace_stat"
	st.Path = relative
	st.FileVersion = ""
	st.Offset, st.Limit = 0, 0
	stat := s.workerWorkspaceFile(ctx, st)
	if !stat.OK {
		return stat
	}
	if stat.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("Python source changed before launch"))
	}
	// There is no Go-style pidfd process-group fallback for Python's
	// intermediate debugpy launcher. A real, verified cgroup is mandatory.
	if os.Geteuid() != 0 {
		return fileError("dap_containment_unavailable", "dependency", errors.New("Python launch requires the root-owned Agent executor cgroup-v2 containment"))
	}
	python, modules := s.pythonDebugpyBinary, s.pythonDebugpyModules // isolated test fixture only
	if python != "" && modules == "" {
		return fileError("python_dap_fixture_incomplete", "dependency", errors.New("Python fixture needs exact adapter modules"))
	}
	if python == "" {
		python, modules, err = systemexec.PythonDebugpy()
		if err != nil {
			return fileError("python_debugpy_unavailable", "dependency", errors.New("trusted pinned Python debugpy 1.8.22 optional runtime unavailable"))
		}
	}
	if blocked := s.beginActionAudit(req, "debug_launch", "worker", workspace+"\x00"+relative+"\x00python/"+mode, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	auditObject := workspace + "\x00" + relative + "\x00python/" + mode
	defer func() {
		response = s.finishActionAudit(req, "debug_launch", "worker", auditObject, "developer_debugger", started, response)
	}()
	containment, err := createDAPCgroup()
	if err != nil {
		return fileError("dap_containment_unavailable", "dependency", err)
	}
	attached := false
	defer func() {
		if !attached && containment != nil && containment.path != "" {
			if cleanErr := containment.abortBeforeLaunch(); cleanErr != nil {
				response.OK = false
				response.ErrorCode = "dap_containment_cleanup_uncertain"
				response.ErrorClass = "state"
				response.Error = fmt.Sprintf("unverified prelaunch containment cleanup: %v", cleanErr)
			}
		}
	}()
	// -I ignores user/project environment, Python startup hooks and site
	// injections. The sole explicit sys.path entry is the trusted operator-
	// owned and content-pinned complete debugpy wheel extraction.
	bootstrap := "import runpy,sys;sys.path.insert(0," + stringMustJSON(modules) + ");runpy.run_module('debugpy.adapter',run_name='__main__')"
	id, err := s.workerContainedDAPSession(req, workspace, python, containment, "-I", "-Xfrozen_modules=off", "-c", bootstrap)
	if err != nil {
		bad := fileError("python_dap_start_failed", "runtime", err)
		if id != "" {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				bad.ErrorCode = "debug_cleanup_uncertain"
				bad.Status = "cleanup_uncertain"
				bad.SessionID = id
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
				response.Error = response.Error + "; " + closeErr.Error()
			}
		}
	}()
	e, err := s.sessions.get(req, id)
	if err != nil {
		return fileError("python_dap_session_lost", "state", err)
	}
	e.stopMu.Lock()
	if !containment.verified {
		e.stopMu.Unlock()
		return fileError("dap_containment_missing", "state", errors.New("Python adapter was not atomically spawned in its verified cgroup"))
	}
	e.mu.Lock()
	if e.closed || e.exited || e.pidPin == nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("python_dap_adapter_exited_before_attach", "state", errors.New("adapter exited before pinning"))
	}
	duplicate, dupErr := unix.Dup(int(e.pidPin.Fd()))
	if dupErr != nil {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return fileError("python_dap_pidfd_unavailable", "state", dupErr)
	}
	d := &dapState{
		conn: newStdioDAPConn(e.dapStdout, e.stdin), workspace: workspace, adapter: "python/debugpy",
		adapterPID: e.pid, adapterPin: os.NewFile(uintptr(duplicate), "python-dap-original-pidfd"),
		workerUID: s.workerUID, program: relative, targetInterpreter: python,
		sourceVersion: req.FileVersion, sourceVersions: map[string]string{relative: req.FileVersion},
		stage: "starting", containment: containment,
	}
	e.dap = d
	e.mu.Unlock()
	e.stopMu.Unlock()
	attached = true // broker owns cgroup cleanup on EVERY subsequent failure
	ctxInit, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	_, err = d.call(ctxInit, "initialize", map[string]any{
		"adapterID": "debugpy", "clientID": "ai-server-agent",
		"linesStartAt1": true, "columnsStartAt1": true,
		"pathFormat": "path", "supportsVariableType": true,
		"supportsRunInTerminalRequest": false,
	})
	if err != nil {
		return fileError("python_dap_initialize_failed", "runtime", err)
	}
	args := map[string]any{
		"type": "debugpy", "request": "launch", "name": "Agent Python",
		"python": python, "console": "internalConsole", "cwd": workspace,
		"env":         map[string]string{"PYTHONPATH": modules, "PYTHONDONTWRITEBYTECODE": "1"},
		"stopOnEntry": req.DebugStopOnEntry, "subProcess": false,
		"justMyCode": false,
	}
	if mode == "script" {
		args["program"] = file
	}
	if mode == "module" || mode == "test" {
		name := strings.TrimSuffix(filepath.ToSlash(relative), ".py")
		name = strings.ReplaceAll(name, "/", ".")
		if mode == "module" {
			args["module"] = name
		} else {
			args["module"] = "unittest"
			args["args"] = []string{name}
		}
	}
	// debugpy legitimately defers its launch reply until breakpoint setup
	// and configurationDone. The common DAP wire already has this path.
	_, err = d.callDeferredLaunch(ctxInit, args)
	if err != nil {
		return fileError("python_dap_launch_failed", "runtime", err)
	}
	after := s.workerWorkspaceFile(ctx, st)
	if !after.OK || after.FileVersion != req.FileVersion {
		return fileError("debug_program_changed", "conflict", errors.New("Python source changed during debug launch"))
	}
	cleanup = false
	result := map[string]any{"adapter": "python/debugpy", "mode": mode, "configuration_required": true}
	body, _ := json.Marshal(result)
	return Response{OK: true, SessionID: id, Status: "starting", FileVersion: req.FileVersion,
		OutputEncoding: "json", Output: string(body), BytesSeen: int64(len(body)), BytesReturned: int64(len(body))}
}

func stringMustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
