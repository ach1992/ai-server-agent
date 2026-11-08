package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/policy"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
	"golang.org/x/sys/unix"
)

const (
	maxExecutorRequestBytes    = 8 << 20
	maxExecutorResponseBytes   = 8 << 20
	executorConnectionTimeout  = 31 * time.Minute
	defaultRunTimeout          = 30 * time.Minute
	processGroupTerminateGrace = 2 * time.Second
	maxWorkerRunConcurrency    = 64
	rootRunConcurrency         = 1
)

type runLimiter struct {
	worker chan struct{}
	root   chan struct{}
}

func newRunLimiter() *runLimiter {
	return newRunLimiterForSlots(0)
}

// Zero selects bounded effective-CPU admission without default CPU oversubscription.
// A validated positive override deliberately permits greater I/O-bound
// concurrency, without changing separately protected root command capacity.
func newRunLimiterForSlots(configured int) *runLimiter {
	if configured > 0 {
		if configured > 64 {
			configured = 64
		}
		return newRunLimiterWith(configured, rootRunConcurrency)
	}
	return newRunLimiterWith(autoWorkerCommandSlots(runtime.GOMAXPROCS(0)), rootRunConcurrency)
}

// Each admitted command holds capacity only while it runs, not while idle.
// An auto limit tracks effective CPUs, without the historical cap of four.
func autoWorkerCommandSlots(effectiveCPUs int) int {
	if effectiveCPUs < 1 {
		return 1
	}
	if effectiveCPUs > maxWorkerRunConcurrency {
		return maxWorkerRunConcurrency
	}
	return effectiveCPUs
}

func newRunLimiterWith(workerLimit, rootLimit int) *runLimiter {
	if workerLimit < 1 {
		workerLimit = 1
	}
	if rootLimit < 1 {
		rootLimit = 1
	}
	return &runLimiter{
		worker: make(chan struct{}, workerLimit),
		root:   make(chan struct{}, rootLimit),
	}
}

func (l *runLimiter) acquire(root bool) (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	slots := l.worker
	if root {
		slots = l.root
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	default:
		return nil, false
	}
}

func connectionContext(c net.Conn) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		var b [1]byte
		for {
			if _, err := c.Read(b[:]); err != nil {
				cancel()
				return
			}
		}
	}()
	return ctx, cancel
}

func (s *Server) dispatchContext(ctx context.Context, req Request) Response {
	switch req.Action {
	case "service_access", "service_use", "managed_environment", "browser_admission_status", "browser_session_open", "browser_session_credential_login", "browser_session_flow", "browser_session_script", "browser_session_capture", "browser_session_trace", "browser_session_image", "browser_session_status", "browser_session_close", "run", "runtime_list", "code_inspect", "debug_adapter_status", "debug_launch", "debug_action", "debug_status", "debug_stop", "workspace_stat", "workspace_read", "workspace_write", "workspace_apply_edits", "workspace_transfer", "workspace_text_search", "workspace_search", "repository_environment", "repository_discover", "repository_inspect", "worktree_create", "worktree_remove":
		if !s.auth(req.Token) {
			return Response{Error: "unauthorized"}
		}
		switch req.Action {
		case "service_access":
			return s.serviceAccessAction(ctx, req)
		case "service_use":
			return s.serviceUseAction(ctx, req)
		case "managed_environment":
			return s.managedEnvironmentAction(ctx, req)
		case "browser_admission_status":
			return s.browserAdmissionResponse()
		case "browser_session_open", "browser_session_credential_login", "browser_session_flow", "browser_session_script", "browser_session_capture", "browser_session_trace", "browser_session_image", "browser_session_status", "browser_session_close":
			return s.browserSessionAction(ctx, req)
		case "run":
			return s.runContext(ctx, req)
		case "runtime_list":
			return s.runtimeListContext(ctx, req)
		case "code_inspect":
			return s.codeInspect(ctx, req)
		case "debug_adapter_status", "debug_launch", "debug_action", "debug_status", "debug_stop":
			return s.debugAction(ctx, req)
		case "workspace_stat", "workspace_read", "workspace_write", "workspace_apply_edits", "workspace_transfer", "workspace_text_search":
			return s.workerWorkspaceFile(ctx, req)
		case "workspace_search":
			return s.workspaceSearchContext(ctx, req)
		case "repository_environment":
			return s.repositoryEnvironmentContext(ctx, req)
		case "repository_discover":
			return s.repositoryDiscoverContext(ctx, req)
		case "repository_inspect":
			return s.repositoryInspectContext(ctx, req)
		case "worktree_create":
			return s.worktreeCreateContext(ctx, req)
		case "worktree_remove":
			return s.worktreeRemoveContext(ctx, req)
		}
	default:
		return s.dispatch(req)
	}
	return Response{Error: "unknown action"}
}

func processGroupExists(pgid int) (bool, error) {
	if pgid <= 0 {
		return false, nil
	}
	err := syscall.Kill(-pgid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return true, err
}

func terminateProcessGroup(pgid int) (bool, error) {
	exists, err := processGroupExists(pgid)
	if err != nil || !exists {
		return false, err
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return true, err
	}

	deadline := time.Now().Add(processGroupTerminateGrace)
	for time.Now().Before(deadline) {
		exists, err = processGroupExists(pgid)
		if err != nil {
			return true, err
		}
		if !exists {
			return true, nil
		}
		time.Sleep(25 * time.Millisecond)
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return true, err
	}
	killDeadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(killDeadline) {
		exists, err = processGroupExists(pgid)
		if err != nil {
			return true, err
		}
		if !exists {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	exists, err = processGroupExists(pgid)
	if err != nil {
		return true, err
	}
	if exists {
		return true, errors.New("process group still exists after SIGKILL")
	}
	return true, nil
}

func newShellCommandContext(ctx context.Context, command, home, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-s")
	cmd.Stdin = strings.NewReader(command)
	cmd.Dir = dir
	cmd.Env = sanitizedSessionEnv(home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		if err == nil {
			return nil
		}
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	return cmd
}

func (s *Server) commandContext(ctx context.Context, req Request) (*exec.Cmd, policy.Decision) {
	dec := s.guard.Evaluate(req.Command, req.Root)
	home := filepath.Join(s.cfg.StateDir, "worker-home")
	dir := s.cfg.WorkspaceDir
	if req.Workspace != "" {
		dir = req.Workspace
	}
	if req.Root {
		home = "/root"
		dir = "/root"
	}
	cmd := newShellCommandContext(ctx, req.Command, home, dir)
	if req.Root {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{0}}
	} else {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
	}
	return cmd, dec
}

func requestRunTimeout(req Request) time.Duration {
	maxMS := int64(defaultRunTimeout / time.Millisecond)
	if req.TimeoutMS <= 0 || req.TimeoutMS > maxMS {
		return defaultRunTimeout
	}
	return time.Duration(req.TimeoutMS) * time.Millisecond
}

func runCapacityResponse(root bool) Response {
	mode := "worker"
	if root {
		mode = "root"
	}
	return Response{
		Error:      mode + " command execution capacity is busy",
		ReasonCode: "resource_limit",
		ErrorCode:  "resource_limit",
		ErrorClass: "resource",
		Retryable:  true,
		Status:     "busy",
	}
}

// provisionPrivateHostFD creates an anonymous, sealed in-memory credential
// descriptor. Bash keeps its script on stdin and reads the credential from
// fd 3 only when the authorized script explicitly needs it. The descriptor
// is never a path or inherited environment plaintext.
func provisionPrivateHostFD(secret string) (*os.File, error) {
	fd, err := unix.MemfdCreate("asa-private-service-stdin", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "asa-private-service-stdin")
	if _, err = f.WriteString(secret + "\n"); err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil {
		_, err = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS,
			unix.F_SEAL_WRITE|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_SEAL)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Server) runContext(parent context.Context, req Request) Response {
	started := time.Now()
	if bad := commandInputError(req.Command); bad != nil {
		bad.DurationMS = time.Since(started).Milliseconds()
		return *bad
	}

	ctx, cancel := context.WithTimeout(parent, requestRunTimeout(req))
	defer cancel()

	if req.Root {
		if req.Workspace != "" {
			return fileError("invalid_workspace", "validation", errors.New("worker workspace is invalid for root execution"))
		}
	} else {
		cwd, err := s.workerCommandWorkspace(req.Workspace)
		if err != nil {
			return fileError("invalid_workspace", "validation", err)
		}
		if _, err := s.workerSessionHome(); err != nil {
			return fileError("worker_home_unavailable", "state", err)
		}
		req.Workspace = cwd
	}
	cmd, dec := s.commandContext(ctx, req)
	var secretValue string
	if req.CredentialRef != "" {
		if !req.Root || req.PrincipalID == "" || req.PrincipalClass == "" {
			return fileError("private_root_requires_authorized_principal", "authorization",
				errors.New("root credential use requires an authenticated principal and root command"))
		}
		if s.cfg.InstanceID == "" {
			return fileError("host_identity_unavailable", "state", errors.New("Agent instance identity is required"))
		}
		stored, err := serviceaccess.New(s.cfg.StateDir).Resolve(req.CredentialRef, s.cfg.InstanceID)
		if err != nil {
			return fileError("private_root_credential_unavailable", "authorization", err)
		}
		if stored.Project != "" || stored.Guest != "" || stored.GuestUUID != "" {
			return fileError("private_root_target_mismatch", "authorization",
				errors.New("host-root execution cannot consume a guest-bound credential"))
		}
		secretValue = stored.Secret
	}
	if !dec.Allowed {
		return Response{Error: dec.Reason, ReasonCode: "policy_denied", ErrorCode: "policy_denied", ErrorClass: "policy", DurationMS: time.Since(started).Milliseconds()}
	}
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", ReasonCode: "approval_required", ErrorCode: "approval_required", ErrorClass: "approval", Approval: dec, DurationMS: time.Since(started).Milliseconds()}
	}
	browserReservation := ""
	browserTerminationProven := true
	if req.AuditAction == "browser_run" || req.AuditAction == "browser_setup" || req.AuditAction == "browser_cleanup" {
		mode := "maintenance"
		if req.AuditAction == "browser_run" {
			var valid bool
			mode, valid = browserProfileMode(req.BrowserProfileMode)
			if !valid {
				return fileError("invalid_browser_profile_mode", "validation", errors.New("Browser profile mode must be shared or isolated"))
			}
		} else if req.BrowserProfileMode != "" {
			return fileError("invalid_browser_profile_mode", "validation", errors.New("Browser maintenance cannot request a profile mode"))
		}
		holder, err := browserReservationID()
		if err != nil {
			return fileError("browser_admission_unavailable", "state", err)
		}
		granted, err := s.browserAcquire(holder, mode)
		if err != nil {
			return fileError("browser_scratch_reconciliation_failed", "state", err)
		}
		if !granted {
			// Share the managed Browser admission contract: a capacity refusal
			// is a retryable busy outcome, not an execution failure.
			return browserSessionBusy()
		}
		browserReservation = holder
		defer func() {
			// If the group-stop outcome is unknown, do not release its
			// profile/process slot: a still-running Chromium may exist.
			// Executor restart / inspected host recovery remains the
			// explicitly bounded failure path for this rare one-shot case.
			if browserTerminationProven {
				s.browserAdmission.release(browserReservation)
			}
		}()
	} else if req.BrowserProfileMode != "" {
		return fileError("invalid_browser_profile_mode", "validation", errors.New("Browser profile mode belongs only to managed Browser runs"))
	}
	release, ok := s.runs.acquire(req.Root)
	if !ok {
		resp := runCapacityResponse(req.Root)
		resp.DurationMS = time.Since(started).Milliseconds()
		return resp
	}
	defer release()
	if blocked := s.beginActionAudit(req, "run", auditMode(req.Root), req.Command, dec.Category); blocked != nil {
		blocked.DurationMS = time.Since(started).Milliseconds()
		return *blocked
	}

	out := newSyncOutputCapture(s)
	var secretFilter *credentialOutputRedactor
	if secretValue != "" {
		secretFD, err := provisionPrivateHostFD(secretValue)
		if err != nil {
			return s.finishActionAudit(req, "run", "root", req.Command, dec.Category, started,
				fileError("private_root_descriptor_unavailable", "dependency", err))
		}
		defer secretFD.Close()
		// Root Bash receives its script via stdin, so a dedicated inherited fd
		// is necessary to avoid script corruption or secrets in process argv.
		cmd.ExtraFiles = append(cmd.ExtraFiles, secretFD)
		cmd.Env = append(cmd.Env, "ASA_SERVICE_SECRET_FD=3")
		secretFilter = &credentialOutputRedactor{to: out, secret: []byte(secretValue)}
		cmd.Stdout, cmd.Stderr = secretFilter, secretFilter
	} else {
		cmd.Stdout, cmd.Stderr = out, out
	}
	var browserGroup *dapCgroup
	browserGroupFD := -1
	browserScratch := ""
	if req.AuditAction == "browser_run" && s.browserNodeBinary == "" {
		browserScratch = "run." + strings.TrimPrefix(browserReservation, "browser_")
		cmd.Env = append(cmd.Env, "ASA_BROWSER_RUN_SCRATCH="+browserScratch)
	}
	// Only the pinned root Executor production path can launch Chromium.
	// Playwright places Chromium in its own process group, so a Node PGID
	// terminator cannot safely authorize the next profile admission.
	if req.AuditAction == "browser_run" && s.browserNodeBinary == "" {
		var containmentErr error
		browserGroup, containmentErr = createBrowserCgroup(s.cfg.EffectiveExecutorServiceUnit())
		if containmentErr != nil {
			return fileError("browser_containment_unavailable", "dependency", containmentErr)
		}
		browserGroup.beforeKillTestHook = s.browserKillTestHook
		fd, openErr := unix.Open(browserGroup.path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
		if openErr != nil {
			cleanupErr := browserGroup.abortBeforeLaunch()
			if cleanupErr != nil {
				browserTerminationProven = false
			}
			return fileError("browser_containment_unavailable", "state", errors.Join(openErr, cleanupErr))
		}
		// cmd.Process is not yet started, so atomic CLONE_INTO_CGROUP
		// covers Node itself and all future detached Chromium descendants.
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = fd
		browserGroupFD = fd
	}
	var err error
	if browserGroup != nil {
		err = cmd.Start()
		// This descriptor is only needed for atomic process startup;
		// close it before the exact cgroup is verified empty and removed.
		_ = unix.Close(browserGroupFD)
		browserGroupFD = -1
		if err == nil {
			if verifyErr := browserGroup.verifySpawnMembership(cmd.Process.Pid); verifyErr != nil {
				// The child may have started. Do not wait indefinitely for
				// an untrusted launcher before containing its descendants.
				abortErr := browserGroup.abortBeforeLaunch()
				if abortErr != nil {
					// A failed whole-group stop retains Browser admission.
					// Best-effort stop of the unreaped original process
					// prevents a hung synchronous worker, but is not proof
					// that detached Chromium descendants have exited.
					browserTerminationProven = false
					_, groupErr := terminateProcessGroup(cmd.Process.Pid)
					abortErr = errors.Join(abortErr, groupErr)
				}
				err = errors.Join(verifyErr, abortErr, cmd.Wait())
			} else {
				err = cmd.Wait()
			}
		}
	} else {
		err = cmd.Run()
	}
	if secretFilter != nil {
		if filterErr := secretFilter.Flush(); filterErr != nil {
			err = errors.Join(err, fmt.Errorf("private command output filter failed: %w", filterErr))
		}
		// The exact bound reference is marked used only when execution started;
		// a command's nonzero exit does not prove that it had no side effects.
		if cmd.Process != nil {
			_ = serviceaccess.New(s.cfg.StateDir).MarkUsed(req.CredentialRef)
		}
	}
	lingeringGroup := false
	var cleanupErr error
	if browserGroup != nil {
		// A cleanly completed synchronous Browser must have retired its
		// process children. A surviving detached Chromium is background
		// work, not a successful main-command completion. Still perform
		// the exact cgroup kill and empty proof before releasing capacity.
		if browserGroup.verified {
			if busy, inspectErr := browserGroup.populated(); inspectErr == nil {
				lingeringGroup = busy
			}
		}
		if browserGroup.path != "" {
			if browserGroup.verified {
				cleanupErr = browserGroup.killVerifyAndRelease()
			} else {
				cleanupErr = browserGroup.abortBeforeLaunch()
			}
		}
		if cleanupErr == nil && browserScratch != "" {
			cleanupErr = s.browserReclaimNamedScratch(browserScratch, false, 0)
		}
		if cleanupErr != nil {
			browserTerminationProven = false
		}
	} else if cmd.Process != nil {
		terminator := terminateProcessGroup
		if browserReservation != "" && s.browserRunTerminateTestHook != nil {
			terminator = s.browserRunTerminateTestHook // private test-only fault injection
		}
		lingeringGroup, cleanupErr = terminator(cmd.Process.Pid)
		if browserReservation != "" && cleanupErr != nil {
			browserTerminationProven = false
		}
	}
	code := 0
	if err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}

	errorText := errString(err)
	reasonCode := ""
	errorClass := ""
	timedOut := false
	switch ctx.Err() {
	case context.DeadlineExceeded:
		errorText = "command timed out"
		reasonCode = "timeout"
		errorClass = "timeout"
		timedOut = true
	case context.Canceled:
		errorText = "command canceled"
		reasonCode = "canceled"
		errorClass = "canceled"
	default:
		if errors.Is(err, exec.ErrWaitDelay) {
			errorText = "command left background work after the shell exited; completion cannot be proven; use start_job or a managed service"
			reasonCode = "unknown_completion"
			errorClass = "process"
		} else if lingeringGroup {
			errorText = "synchronous command left background work; remaining process-group members were stopped; use start_job or a managed service"
			reasonCode = "background_process"
			errorClass = "process"
			if err == nil {
				err = errors.New(errorText)
				code = 1
			}
		} else if err != nil {
			reasonCode = "command_failed"
			errorClass = "process"
		}
	}
	if cleanupErr != nil {
		errorText = "command or Browser-owned descendant termination could not be proven: " + cleanupErr.Error()
		reasonCode = "unknown_completion"
		errorClass = "process"
		if err == nil {
			err = cleanupErr
			code = 1
		}
	}

	resp := Response{
		OK:         err == nil,
		Error:      errorText,
		ReasonCode: reasonCode,
		ErrorCode:  reasonCode,
		ErrorClass: errorClass,
		ExitCode:   code,
		DurationMS: time.Since(started).Milliseconds(),
		TimedOut:   timedOut,
	}
	applyOutputResult(&resp, out.preview.Result())
	if captureErr := out.Finish(&resp, s.cfg.EffectiveRuntime().TextFallbackBytes); captureErr != nil {
		resp.OK = false
		resp.Status = "output_incomplete"
		resp.ErrorClass = "data"
		resp.ErrorCode = "output_capture_unavailable"
		if errors.Is(captureErr, errSyncOutputLimit) {
			resp.ErrorCode = "output_limit_exceeded"
		}
		resp.ReasonCode = resp.ErrorCode
		resp.Error = "command may have run, but its complete output could not be retained: " + captureErr.Error()
	}
	return s.finishActionAudit(req, "run", auditMode(req.Root), req.Command, dec.Category, started, resp)
}
func encodeExecutorResponse(resp Response) []byte {
	payload, err := json.Marshal(resp)
	if err == nil && len(payload)+1 <= maxExecutorResponseBytes {
		return append(payload, '\n')
	}
	fallback := Response{
		Error:      "executor response exceeded the local frame limit",
		ReasonCode: "response_too_large",
	}
	payload, _ = json.Marshal(fallback)
	return append(payload, '\n')
}

func ClientCallContext(ctx context.Context, socket, token string, req Request) (Response, error) {
	req.PrincipalID = ""
	req.PrincipalClass = ""
	req.PrincipalName = ""
	if principal, ok := credential.PrincipalFromContext(ctx); ok {
		req.PrincipalID = principal.ID
		req.PrincipalClass = principal.Class
		req.PrincipalName = principal.Name
	}
	if req.RequestID == "" {
		if requestID, ok := RequestCorrelationID(ctx); ok {
			req.RequestID = requestID
		}
	}
	if err := ensureRequestCorrelation(&req); err != nil {
		return Response{}, err
	}
	req.Token = token
	// The internal executor socket also has an 8 MiB request-frame limit.
	// Large escaped workspace edits must fail deterministically BEFORE
	// connecting; otherwise the decoder would reject a valid raw-size batch
	// with a transport error rather than a bounded resource result.
	if req.Action == "workspace_write" || req.Action == "workspace_apply_edits" ||
		req.Action == "workspace_read" || req.Action == "workspace_transfer" || req.Action == "workspace_text_search" || req.Action == "service_use" {
		wire, err := json.Marshal(req)
		if err != nil {
			return fileError("invalid_request", "validation", err), nil
		}
		if len(wire)+1 > maxExecutorRequestBytes {
			return fileError("input_too_large", "resource", fmt.Errorf("serialized executor request exceeds %d-byte frame", maxExecutorRequestBytes)), nil
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	c, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()

	stopCancel := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopCancel()

	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, err
	}
	payload, err := io.ReadAll(io.LimitReader(c, maxExecutorResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	if len(payload) > maxExecutorResponseBytes {
		return Response{}, fmt.Errorf("executor response exceeds %d bytes", maxExecutorResponseBytes)
	}

	var resp Response
	if err := json.Unmarshal(payload, &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}
