package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

func TestRunLimiterSeparatesWorkerAndRootWithoutQueue(t *testing.T) {
	limiter := newRunLimiterWith(1, 1)

	releaseWorker, ok := limiter.acquire(false)
	if !ok {
		t.Fatal("first worker slot was not available")
	}
	defer releaseWorker()
	if _, ok := limiter.acquire(false); ok {
		t.Fatal("worker limiter queued or admitted work beyond capacity")
	}

	releaseRoot, ok := limiter.acquire(true)
	if !ok {
		t.Fatal("root capacity must be independent from worker capacity")
	}
	defer releaseRoot()
	if _, ok := limiter.acquire(true); ok {
		t.Fatal("root limiter queued or admitted work beyond capacity")
	}
}

func TestRunCapacityResponseIsStructured(t *testing.T) {
	s := &Server{
		cfg:   config.Config{WorkspaceDir: t.TempDir()},
		guard: policy.New(nil),
		runs:  newRunLimiterWith(1, 1),
	}
	release, ok := s.runs.acquire(false)
	if !ok {
		t.Fatal("failed to reserve worker slot")
	}
	defer release()

	resp := s.runContext(context.Background(), Request{Command: "printf should-not-run"})
	if resp.OK || resp.Status != "busy" || resp.ReasonCode != "resource_limit" || !resp.Retryable {
		t.Fatalf("unexpected busy response: %+v", resp)
	}
}

func TestRunContextHandlesProcessStartFailure(t *testing.T) {
	s := &Server{
		cfg: config.Config{
			WorkspaceDir: filepath.Join(t.TempDir(), "missing"),
		},
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
		runs:      newRunLimiterWith(1, 1),
	}
	resp := s.runContext(context.Background(), Request{Command: "printf unreachable"})
	if resp.OK || resp.Error == "" {
		t.Fatalf("process start failure was not returned safely: %+v", resp)
	}
}

func TestRequestRunTimeoutIsBounded(t *testing.T) {
	if got := requestRunTimeout(Request{}); got != defaultRunTimeout {
		t.Fatalf("default timeout = %s, want %s", got, defaultRunTimeout)
	}
	if got := requestRunTimeout(Request{TimeoutMS: int64((defaultRunTimeout + time.Minute) / time.Millisecond)}); got != defaultRunTimeout {
		t.Fatalf("oversized timeout = %s, want hard maximum %s", got, defaultRunTimeout)
	}
	const requested = 1250 * time.Millisecond
	if got := requestRunTimeout(Request{TimeoutMS: int64(requested / time.Millisecond)}); got != requested {
		t.Fatalf("requested timeout = %s, want %s", got, requested)
	}
}

func TestShellContextCancellationTerminatesProcessGroup(t *testing.T) {
	dir := t.TempDir()
	termMarker := filepath.Join(dir, "term-seen")
	leakMarker := filepath.Join(dir, "escaped-child")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	command := "trap 'printf term > " + shellQuote(termMarker) + "' TERM; while :; do sleep 0.2; done; printf leaked > " + shellQuote(leakMarker)
	cmd := newShellCommandContext(ctx, command, dir, dir)
	if err := cmd.Run(); err == nil {
		t.Fatal("canceled command unexpectedly succeeded")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
	}
	if _, err := os.Stat(termMarker); err != nil {
		t.Fatalf("process group did not observe graceful TERM before fallback: %v", err)
	}
	if _, err := os.Stat(leakMarker); !os.IsNotExist(err) {
		t.Fatalf("background work escaped process-group cancellation; stat err=%v", err)
	}
}

func TestTerminateProcessGroupCleansBackgroundChildAfterShellExit(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "background-child")
	command := "(sleep 0.8; printf leaked > " + shellQuote(marker) + ") >/dev/null 2>&1 &"
	cmd := newShellCommandContext(context.Background(), command, dir, dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("shell command failed before cleanup: %v", err)
	}
	lingering, err := terminateProcessGroup(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("terminateProcessGroup: %v", err)
	}
	if !lingering {
		t.Fatal("expected same-process-group background child to remain after shell exit")
	}
	time.Sleep(850 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("same-process-group background child survived cleanup; stat err=%v", err)
	}
}

func TestShellWaitDelaySurfacesInheritedBackgroundOutput(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "background-child")
	command := "(sleep 4; printf leaked > " + shellQuote(marker) + ") &"
	cmd := newShellCommandContext(context.Background(), command, dir, dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	err := cmd.Run()
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("command error = %v, want exec.ErrWaitDelay", err)
	}
	if elapsed := time.Since(start); elapsed < processGroupTerminateGrace {
		t.Fatalf("wait delay returned too early after %s", elapsed)
	}
	lingering, cleanupErr := terminateProcessGroup(cmd.Process.Pid)
	if cleanupErr != nil {
		t.Fatalf("terminateProcessGroup: %v", cleanupErr)
	}
	if !lingering {
		t.Fatal("expected inherited-output background child to remain until explicit group cleanup")
	}
	time.Sleep(250 * time.Millisecond)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("background child survived wait-delay cleanup; stat err=%v", statErr)
	}
}

func TestConnectionContextCancelsOnPeerClose(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	ctx, cancel := connectionContext(server)
	defer cancel()

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("executor connection context did not cancel after peer close")
	}
}

func TestEncodeExecutorResponseBoundsFrame(t *testing.T) {
	resp := Response{OK: true, Output: strings.Repeat("\x00", maxExecutorResponseBytes/4)}
	payload := encodeExecutorResponse(resp)
	if len(payload) > maxExecutorResponseBytes {
		t.Fatalf("encoded response length = %d, exceeds %d-byte frame", len(payload), maxExecutorResponseBytes)
	}
	var decoded Response
	if err := json.Unmarshal(bytes.TrimSpace(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ReasonCode != "response_too_large" || decoded.OK {
		t.Fatalf("oversized response did not fail closed: %+v", decoded)
	}
}

func TestClientCallContextRejectsOversizedResponse(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "executor.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer c.Close()
		var req Request
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			serverDone <- err
			return
		}
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		remaining := maxExecutorResponseBytes + 1
		for remaining > 0 {
			n := len(chunk)
			if n > remaining {
				n = remaining
			}
			if _, err := c.Write(chunk[:n]); err != nil {
				serverDone <- err
				return
			}
			remaining -= n
		}
		serverDone <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = ClientCallContext(ctx, socket, "token", Request{Action: "job_status", JobID: "1"})
	if err == nil || !strings.Contains(err.Error(), "executor response exceeds") {
		t.Fatalf("ClientCallContext error = %v, want bounded-response rejection", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestClientCallContextClosesSocketOnCancellation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "executor.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer c.Close()
		var req Request
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			serverDone <- err
			return
		}
		var b [1]byte
		_, err = c.Read(b[:])
		if err == nil {
			serverDone <- errors.New("client connection remained open after cancellation")
			return
		}
		serverDone <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = ClientCallContext(ctx, socket, "token", Request{Action: "job_status", JobID: "1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ClientCallContext error = %v, want deadline exceeded", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestClientCallContextOverridesCallerPrincipalFromAuthenticatedContext(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "executor.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	reqCh := make(chan Request, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			errCh <- err
			return
		}
		reqCh <- req
		if err := json.NewEncoder(conn).Encode(Response{OK: true, Status: "ok", RequestID: req.RequestID, OperationID: req.OperationID}); err != nil {
			errCh <- err
		}
	}()

	ctx := credential.WithPrincipal(context.Background(), credential.Principal{
		ID: "mcp-gateway", Class: "gateway", Name: "mcp-gateway",
	})
	ctx, requestID, err := WithNewRequestCorrelation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ClientCallContext(ctx, socket, "executor-secret", Request{
		Action:         "run",
		Command:        "true",
		Approval:       true,
		PrincipalID:    "caller-controlled",
		PrincipalClass: "caller",
		PrincipalName:  "caller",
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		t.Fatal(err)
	case req := <-reqCh:
		if req.PrincipalID != "mcp-gateway" || req.PrincipalClass != "gateway" || req.PrincipalName != "mcp-gateway" {
			t.Fatalf("executor request principal was not server-derived: %+v", req)
		}
		if req.RequestID != requestID || req.ApprovalID == "" {
			t.Fatalf("MCP-entry correlation was not propagated to executor request: want=%q req=%+v", requestID, req)
		}
		if resp.RequestID != req.RequestID {
			t.Fatalf("request correlation was not preserved in response: resp=%+v req=%+v", resp, req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executor request")
	}
}

func TestClientCallContextClearsPrincipalWithoutAuthenticatedContext(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "executor.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	reqCh := make(chan Request, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			errCh <- err
			return
		}
		reqCh <- req
		if err := json.NewEncoder(conn).Encode(Response{OK: true, Status: "ok"}); err != nil {
			errCh <- err
		}
	}()

	_, err = ClientCallContext(context.Background(), socket, "executor-secret", Request{
		Action:         "run",
		Command:        "true",
		PrincipalID:    "caller-controlled",
		PrincipalClass: "caller",
		PrincipalName:  "caller",
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		t.Fatal(err)
	case req := <-reqCh:
		if req.PrincipalID != "" || req.PrincipalClass != "" || req.PrincipalName != "" {
			t.Fatalf("unauthenticated context preserved caller principal: %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executor request")
	}
}
