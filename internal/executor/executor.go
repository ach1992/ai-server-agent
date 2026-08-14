package executor

import (
	"bufio"
	"bytes"
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
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

type Server struct {
	cfg       config.Config
	token     string
	guard     *policy.Guard
	audit     *audit.Logger
	workerUID uint32
	workerGID uint32
}

func NewServer(cfg config.Config, token string) (*Server, error) {
	u, err := user.Lookup(cfg.WorkerUser)
	if err != nil {
		return nil, fmt.Errorf("lookup worker user: %w", err)
	}
	uid64, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid64, _ := strconv.ParseUint(u.Gid, 10, 32)
	protected := []string{"ai-server-agent", "/usr/local/bin/ai-server-agent", "/etc/ai-server-agent", cfg.StateDir, cfg.LogDir, cfg.ExecutorSocket, cfg.ListenAddress}
	return &Server{cfg: cfg, token: strings.TrimSpace(token), guard: policy.New(protected), audit: audit.New(filepath.Join(cfg.LogDir, "audit.jsonl")), workerUID: uint32(uid64), workerGID: uint32(gid64)}, nil
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
	_ = c.SetDeadline(time.Now().Add(10 * time.Minute))
	var req Request
	if err := json.NewDecoder(io.LimitReader(c, 8<<20)).Decode(&req); err != nil {
		_ = json.NewEncoder(c).Encode(Response{Error: "invalid request: " + err.Error(), GeneratedAt: time.Now().UTC()})
		return
	}
	resp := s.dispatch(req)
	resp.GeneratedAt = time.Now().UTC()
	_ = json.NewEncoder(c).Encode(resp)
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
	default:
		return Response{Error: "unknown action"}
	}
}

func (s *Server) command(req Request) (*exec.Cmd, policy.Decision) {
	dec := s.guard.Evaluate(req.Command, req.Root)
	cmd := exec.Command("/bin/bash", "-lc", req.Command)
	if req.Root {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{0}}}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}}
	}
	cmd.Dir = s.cfg.WorkspaceDir
	cmd.Env = append(os.Environ(), "HOME="+s.cfg.WorkspaceDir, "AI_SERVER_AGENT=1")
	return cmd, dec
}

func (s *Server) run(req Request) Response {
	cmd, dec := s.command(req)
	if !dec.Allowed {
		return Response{Error: dec.Reason}
	}
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", Approval: dec}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	_ = s.audit.Write(audit.Entry{Action: "run", Mode: map[bool]string{true: "root", false: "worker"}[req.Root], Command: req.Command, Success: err == nil, Detail: dec.Category})
	return Response{OK: err == nil, Error: errString(err), Output: limit(out.String(), 4<<20), ExitCode: code}
}

func (s *Server) startJob(req Request) Response {
	_, dec := s.command(req)
	if !dec.Allowed {
		return Response{Error: dec.Reason}
	}
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", Approval: dec}
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	logPath := filepath.Join(s.cfg.StateDir, "jobs", id+".log")
	statusPath := filepath.Join(s.cfg.StateDir, "jobs", id+".status")
	if err := os.MkdirAll(filepath.Dir(logPath), 0750); err != nil {
		return Response{Error: err.Error()}
	}
	unit := "ai-job-" + id
	mode := ""
	if !req.Root {
		mode = "--uid=" + s.cfg.WorkerUser
	}
	shell := fmt.Sprintf("/bin/bash -lc %s >>%s 2>&1; rc=$?; printf '%%s\n' \"$rc\" >%s; exit \"$rc\"", shellQuote(req.Command), shellQuote(logPath), shellQuote(statusPath))
	args := []string{"--unit", unit, "--collect", "--property=WorkingDirectory=" + s.cfg.WorkspaceDir, "--setenv=HOME=" + s.cfg.WorkspaceDir, "--setenv=AI_SERVER_AGENT=1"}
	if mode != "" {
		args = append(args, mode)
	}
	args = append(args, "/bin/bash", "-lc", shell)
	out, err := exec.Command("systemd-run", args...).CombinedOutput()
	_ = s.audit.Write(audit.Entry{Action: "start_job", Mode: map[bool]string{true: "root", false: "worker"}[req.Root], Command: req.Command, Success: err == nil, Detail: string(out)})
	if err != nil {
		return Response{Error: err.Error() + ": " + string(out)}
	}
	return Response{OK: true, JobID: id, Output: string(out)}
}

func (s *Server) jobStatus(req Request) Response {
	id, err := safeID(req.JobID)
	if err != nil {
		return Response{Error: err.Error()}
	}
	statusPath := filepath.Join(s.cfg.StateDir, "jobs", id+".status")
	if b, er := os.ReadFile(statusPath); er == nil {
		return Response{OK: true, Status: "completed", Output: strings.TrimSpace(string(b))}
	}
	unit := "ai-job-" + id
	out, er := exec.Command("systemctl", "show", unit, "--property=ActiveState,SubState,ExecMainStatus,MainPID", "--no-pager").CombinedOutput()
	if er != nil {
		return Response{Error: er.Error() + ": " + string(out)}
	}
	return Response{OK: true, Status: string(out)}
}
func (s *Server) jobStop(req Request) Response {
	id, err := safeID(req.JobID)
	if err != nil {
		return Response{Error: err.Error()}
	}
	out, er := exec.Command("systemctl", "stop", "ai-job-"+id).CombinedOutput()
	_ = s.audit.Write(audit.Entry{Action: "job_stop", Success: er == nil, Detail: id})
	if er != nil {
		return Response{Error: er.Error() + ": " + string(out)}
	}
	return Response{OK: true, Output: string(out)}
}
func (s *Server) jobOutput(req Request) Response {
	id, err := safeID(req.JobID)
	if err != nil {
		return Response{Error: err.Error()}
	}
	path := filepath.Join(s.cfg.StateDir, "jobs", id+".log")
	f, er := os.Open(path)
	if er != nil {
		return Response{Error: er.Error()}
	}
	defer f.Close()
	if req.Offset < 0 {
		req.Offset = 0
	}
	if _, er = f.Seek(req.Offset, 0); er != nil {
		return Response{Error: er.Error()}
	}
	lim := req.Limit
	if lim <= 0 || lim > 1<<20 {
		lim = 1 << 20
	}
	b, er := io.ReadAll(io.LimitReader(f, int64(lim)))
	if er != nil {
		return Response{Error: er.Error()}
	}
	return Response{OK: true, Output: string(b), NextOffset: req.Offset + int64(len(b))}
}

func (s *Server) readFile(req Request) Response {
	dec := s.guard.Evaluate("read "+req.Path, true)
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", Approval: dec}
	}
	b, err := os.ReadFile(req.Path)
	if err != nil {
		return Response{Error: err.Error()}
	}
	if len(b) > 4<<20 {
		b = b[:4<<20]
	}
	_ = s.audit.Write(audit.Entry{Action: "read_file", Mode: "root", Command: req.Path, Success: true})
	return Response{OK: true, Output: string(b)}
}
func (s *Server) writeFile(req Request) Response {
	dec := s.guard.Evaluate("write "+req.Path, req.Root)
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", Approval: dec}
	}
	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0644
	}
	if err := os.MkdirAll(filepath.Dir(req.Path), 0755); err != nil {
		return Response{Error: err.Error()}
	}
	if err := os.WriteFile(req.Path, []byte(req.Content), mode); err != nil {
		return Response{Error: err.Error()}
	}
	_ = s.audit.Write(audit.Entry{Action: "write_file", Mode: map[bool]string{true: "root", false: "worker"}[req.Root], Command: req.Path, Success: true})
	return Response{OK: true}
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
	req.Token = token
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	err = json.NewDecoder(bufio.NewReader(c)).Decode(&resp)
	return resp, err
}
