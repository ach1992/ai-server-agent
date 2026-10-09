package executor

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

type Server struct {
	cfg                       config.Config
	token                     string
	guard                     *policy.Guard
	audit                     *audit.Logger
	auditWriteHook            func(string) error // test-only pre-action failure injection
	auditBeforeCompletionHook func()             // test-only completion-path failure/interleaving injection
	workerUID                 uint32
	workerGID                 uint32
	runs                      *runLimiter
	sessions                  *stdioSessionBroker
	jobsMu                    sync.Mutex
	lifecycleLockPath         string
	fileWriteHooks            *fileWriteTestHooks
	workspaceHooks            *workspaceTestHooks
	structuralSearchBinary    string // test-only override; production resolves trusted ast-grep paths
	workspaceHelperBinary     string // test-only compiled Agent binary; production self-executes
	terminalBinary            string // test-only isolated tmux fixture; production uses /usr/bin/tmux
	goplsBinary               string // test-only Go language server fixture
}

func NewServer(cfg config.Config, token string) (*Server, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("executor token is empty")
	}
	u, err := user.Lookup(cfg.WorkerUser)
	if err != nil {
		return nil, fmt.Errorf("lookup worker user: %w", err)
	}
	uid64, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid64, _ := strconv.ParseUint(u.Gid, 10, 32)
	protected := []string{"ai-server-agent", "/usr/local/bin/ai-server-agent", "/etc/ai-server-agent", cfg.StateDir, cfg.LogDir, cfg.ExecutorSocket, cfg.ListenAddress}
	return &Server{
		cfg:               cfg,
		token:             token,
		guard:             policy.New(protected),
		audit:             audit.NewWithKeyAndReserve(filepath.Join(cfg.LogDir, "audit.jsonl"), filepath.Join(filepath.Dir(cfg.ExecutorToken), "audit-fingerprint.key"), jobStateSafetyReserveBytes),
		workerUID:         uint32(uid64),
		workerGID:         uint32(gid64),
		runs:              newRunLimiter(),
		sessions:          newStdioSessionBroker(),
		lifecycleLockPath: lifecycleManagementLockPath,
	}, nil
}

func (s *Server) Serve() error {
	_ = os.Remove(s.cfg.ExecutorSocket)
	if err := os.MkdirAll(filepath.Dir(s.cfg.ExecutorSocket), 0750); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.cfg.ExecutorSocket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(s.cfg.ExecutorSocket, 0660); err != nil {
		return err
	}
	if g, err := user.LookupGroup(s.cfg.AgentUser); err == nil {
		if gid, er := strconv.Atoi(g.Gid); er == nil {
			_ = os.Chown(s.cfg.ExecutorSocket, 0, gid)
		}
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(executorConnectionTimeout))
	var req Request
	if err := json.NewDecoder(io.LimitReader(c, maxExecutorRequestBytes)).Decode(&req); err != nil {
		_, _ = c.Write(encodeExecutorResponse(Response{Error: "invalid request: " + err.Error(), GeneratedAt: time.Now().UTC()}))
		return
	}
	ctx, cancel := connectionContext(c)
	defer cancel()
	if req.Action != "start_job" {
		req.OperationID = ""
	}
	if err := ensureRequestCorrelation(&req); err != nil {
		_, _ = c.Write(encodeExecutorResponse(Response{Error: "invalid correlation metadata: " + err.Error(), ReasonCode: "invalid_correlation", ErrorCode: "invalid_correlation", ErrorClass: "validation", GeneratedAt: time.Now().UTC()}))
		return
	}
	resp := s.dispatchContext(ctx, req)
	if resp.RequestID == "" {
		resp.RequestID = req.RequestID
	}
	if resp.OperationID == "" {
		resp.OperationID = req.OperationID
	}
	resp.GeneratedAt = time.Now().UTC()
	_, _ = c.Write(encodeExecutorResponse(resp))
}

func (s *Server) auth(tok string) bool {
	a := []byte(strings.TrimSpace(tok))
	b := []byte(s.token)
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

func (s *Server) dispatch(req Request) Response {
	if !s.auth(req.Token) {
		return Response{Error: "unauthorized"}
	}
	switch req.Action {
	case "run":
		return s.run(req)
	case "start_job":
		return s.startJob(req)
	case "job_status":
		return s.jobStatus(req)
	case "job_output":
		return s.jobOutput(req)
	case "job_stop":
		return s.jobStop(req)
	case "read_file":
		return s.readFile(req)
	case "write_file":
		return s.writeFile(req)
	case "workspace_stat", "workspace_read", "workspace_write", "workspace_apply_edits", "workspace_text_search":
		return s.workerWorkspaceFile(context.Background(), req)
	case "workspace_search":
		return s.workspaceSearchContext(context.Background(), req)
	case "repository_environment":
		return s.repositoryEnvironmentContext(context.Background(), req)
	case "repository_discover":
		return s.repositoryDiscoverContext(context.Background(), req)
	case "repository_inspect":
		return s.repositoryInspectContext(context.Background(), req)
	case "worktree_create":
		return s.worktreeCreateContext(context.Background(), req)
	case "worktree_remove":
		return s.worktreeRemoveContext(context.Background(), req)
	case "code_inspect":
		return s.codeInspect(context.Background(), req)
	case "terminal_open", "terminal_read", "terminal_write", "terminal_interrupt", "terminal_resize", "terminal_close", "terminal_reconnect":
		return s.terminalAction(req)
	default:
		return Response{Error: "unknown action"}
	}
}

const safeCommandPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func sanitizedCommandEnv(home string) []string {
	return []string{
		"HOME=" + home,
		"PATH=" + safeCommandPath,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"AI_SERVER_AGENT=1",
	}
}

func newShellCommand(command, home, dir string) *exec.Cmd {
	return newShellCommandContext(context.Background(), command, home, dir)
}

func (s *Server) command(req Request) (*exec.Cmd, policy.Decision) {
	return s.commandContext(context.Background(), req)
}

func (s *Server) run(req Request) Response {
	return s.runContext(context.Background(), req)
}

func trustedDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("trusted path is not a real directory: %s", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("trusted directory is not owned by executor uid: %s", path)
	}
	if fi.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("trusted directory is group/other writable: %s", path)
	}
	return nil
}

func createJobFile(path string, uid, gid uint32) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0640)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chown(int(uid), int(gid)); err != nil {
		return err
	}
	return f.Chmod(0640)
}

func (s *Server) openJobFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("job path is not a regular file: %s", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || (st.Uid != 0 && st.Uid != s.workerUID) {
		f.Close()
		return nil, fmt.Errorf("job file has an unexpected owner: %s", path)
	}
	if fi.Mode().Perm()&0002 != 0 {
		f.Close()
		return nil, fmt.Errorf("job file is world-writable: %s", path)
	}
	return f, nil
}

func (s *Server) startJob(req Request) Response {
	return s.startJobBounded(req)
}
func (s *Server) jobStatus(req Request) Response {
	id, err := safeID(req.JobID)
	if err != nil {
		return Response{Error: err.Error(), ReasonCode: "invalid_job_id", ErrorCode: "invalid_job_id", ErrorClass: "validation"}
	}
	jobsDir := filepath.Join(s.cfg.StateDir, "jobs")
	paths := jobPathsFor(jobsDir, id)
	if terminal, found := s.jobTerminalReplayResponse(paths.status, id); found {
		return terminal
	}

	started, err := s.jobStarted(paths.started)
	if err != nil {
		return jobStateError("job_status_unavailable", err)
	}
	accepted := started
	if !accepted {
		accepted, err = jobHasStartedClaim(filepath.Join(jobsDir, "claims"), id)
		if err != nil {
			return jobStateError("job_status_unavailable", err)
		}
	}
	if accepted {
		active, err := jobUnitActive(id)
		if err != nil {
			return jobStateError("job_status_unavailable", err)
		}
		if !active {
			if err := s.markJobStatusUnknownIfEmpty(paths.status); err != nil {
				return jobStateError("job_status_unavailable", err)
			}
			if err := removeTerminalCommandHandoff(paths.command); err != nil {
				return jobStateError("job_status_unavailable", err)
			}
			terminal, _ := s.jobTerminalReplayResponse(paths.status, id)
			return terminal
		}
	}

	unit := "ai-job-" + id
	out := newBoundedOutputCollector(systemdOutputLimit)
	cmd := exec.Command("systemctl", "show", unit, "--property=ActiveState,SubState,ExecMainStatus,MainPID", "--no-pager")
	cmd.Stdout = out
	cmd.Stderr = out
	er := cmd.Run()
	result := out.Result()
	if er != nil {
		resp := Response{Error: er.Error(), ReasonCode: "job_status_unavailable", ErrorCode: "job_status_unavailable", ErrorClass: "state"}
		applyOutputResult(&resp, result)
		return resp
	}
	resp := Response{OK: true, Status: strings.TrimSpace(result.Output)}
	applyOutputResult(&resp, result)
	return resp
}
func (s *Server) jobStop(req Request) (resp Response) {
	id, err := safeID(req.JobID)
	if err != nil {
		return Response{Error: err.Error(), ReasonCode: "invalid_job_id", ErrorCode: "invalid_job_id", ErrorClass: "validation"}
	}
	req.JobID = id
	auditStarted := time.Now()
	if blocked := s.beginActionAudit(req, "job_stop", "root", "", ""); blocked != nil {
		return *blocked
	}
	defer func() {
		resp = s.finishActionAudit(req, "job_stop", "root", "", "", auditStarted, resp)
	}()

	out := newBoundedOutputCollector(systemdOutputLimit)
	cmd := exec.Command("systemctl", "stop", "ai-job-"+id)
	cmd.Stdout = out
	cmd.Stderr = out
	er := cmd.Run()
	result := out.Result()
	if er != nil {
		resp = Response{Error: er.Error(), ReasonCode: "job_stop_failed", ErrorCode: "job_stop_failed", ErrorClass: "process", JobID: id}
		applyOutputResult(&resp, result)
		return resp
	}
	resp = Response{OK: true, JobID: id, Status: "stop_requested"}
	applyOutputResult(&resp, result)
	return resp
}
func (s *Server) jobOutput(req Request) Response {
	return s.jobOutputBounded(req)
}
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[output truncated]"
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func safeID(s string) (string, error) {
	if s == "" {
		return "", errors.New("job_id required")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", errors.New("invalid job_id")
		}
	}
	return s, nil
}

func ClientCall(socket, token string, req Request) (Response, error) {
	return ClientCallContext(context.Background(), socket, token, req)
}
