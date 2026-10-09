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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

const (
	maxExecutorRequestBytes    = 8 << 20
	maxExecutorResponseBytes   = 8 << 20
	executorConnectionTimeout  = 31 * time.Minute
	defaultRunTimeout          = 30 * time.Minute
	processGroupTerminateGrace = 2 * time.Second
	maxWorkerRunConcurrency    = 4
	rootRunConcurrency         = 1
)

type runLimiter struct {
	worker chan struct{}
	root   chan struct{}
}

func newRunLimiter() *runLimiter {
	workerLimit := runtime.GOMAXPROCS(0)
	if workerLimit < 1 {
		workerLimit = 1
	}
	if workerLimit > maxWorkerRunConcurrency {
		workerLimit = maxWorkerRunConcurrency
	}
	return newRunLimiterWith(workerLimit, rootRunConcurrency)
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
	case "run", "workspace_read", "workspace_write", "workspace_apply_edits", "workspace_text_search", "workspace_search", "repository_environment", "repository_discover", "repository_inspect", "worktree_create", "worktree_remove":
		if !s.auth(req.Token) {
			return Response{Error: "unauthorized"}
		}
		switch req.Action {
		case "run":
			return s.runContext(ctx, req)
		case "workspace_read", "workspace_write", "workspace_apply_edits", "workspace_text_search":
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
	cmd.Env = sanitizedCommandEnv(home)
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
	home := s.cfg.WorkspaceDir
	dir := s.cfg.WorkspaceDir
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

func (s *Server) runContext(parent context.Context, req Request) Response {
	started := time.Now()
	if bad := commandInputError(req.Command); bad != nil {
		bad.DurationMS = time.Since(started).Milliseconds()
		return *bad
	}

	ctx, cancel := context.WithTimeout(parent, requestRunTimeout(req))
	defer cancel()

	cmd, dec := s.commandContext(ctx, req)
	if !dec.Allowed {
		return Response{Error: dec.Reason, ReasonCode: "policy_denied", ErrorCode: "policy_denied", ErrorClass: "policy", DurationMS: time.Since(started).Milliseconds()}
	}
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", ReasonCode: "approval_required", ErrorCode: "approval_required", ErrorClass: "approval", Approval: dec, DurationMS: time.Since(started).Milliseconds()}
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

	out := newBoundedOutputCollector(maxSyncOutputBytes)
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	lingeringGroup := false
	var cleanupErr error
	if cmd.Process != nil {
		lingeringGroup, cleanupErr = terminateProcessGroup(cmd.Process.Pid)
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
		errorText = "command process-group termination could not be proven: " + cleanupErr.Error()
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
	applyOutputResult(&resp, out.Result())
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
