package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

const (
	maxExecutorRequestBytes   = 8 << 20
	maxExecutorResponseBytes  = 8 << 20
	executorConnectionTimeout = 31 * time.Minute
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
	if req.Action == "run" {
		if !s.auth(req.Token) {
			return Response{Error: "unauthorized"}
		}
		return s.runContext(ctx, req)
	}
	return s.dispatch(req)
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
	cmd := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", command)
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
		Retryable:  true,
		Status:     "busy",
	}
}

func (s *Server) runContext(parent context.Context, req Request) Response {
	ctx, cancel := context.WithTimeout(parent, requestRunTimeout(req))
	defer cancel()

	cmd, dec := s.commandContext(ctx, req)
	if !dec.Allowed {
		return Response{Error: dec.Reason}
	}
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", Approval: dec}
	}
	release, ok := s.runs.acquire(req.Root)
	if !ok {
		return runCapacityResponse(req.Root)
	}
	defer release()

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	lingeringGroup, cleanupErr := terminateProcessGroup(cmd.Process.Pid)
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
	switch ctx.Err() {
	case context.DeadlineExceeded:
		errorText = "command timed out"
		reasonCode = "timeout"
	case context.Canceled:
		errorText = "command canceled"
		reasonCode = "canceled"
	default:
		if errors.Is(err, exec.ErrWaitDelay) {
			errorText = "command left background work after the shell exited; completion cannot be proven; use start_job or a managed service"
			reasonCode = "unknown_completion"
		} else if lingeringGroup {
			errorText = "synchronous command left background work; remaining process-group members were stopped; use start_job or a managed service"
			reasonCode = "background_process"
			if err == nil {
				err = errors.New(errorText)
				code = 1
			}
		}
	}
	if cleanupErr != nil {
		errorText = "command process-group termination could not be proven: " + cleanupErr.Error()
		reasonCode = "unknown_completion"
		if err == nil {
			err = cleanupErr
			code = 1
		}
	}

	_ = s.audit.Write(audit.Entry{
		Action:  "run",
		Mode:    map[bool]string{true: "root", false: "worker"}[req.Root],
		Command: req.Command,
		Success: err == nil,
		Detail:  dec.Category,
	})
	return Response{
		OK:         err == nil,
		Error:      errorText,
		ReasonCode: reasonCode,
		Output:     limit(out.String(), 4<<20),
		ExitCode:   code,
	}
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
