package executor

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"
)

func terminalWaitFixture(t *testing.T) (*Server, Request, *stdioSession) {
	t.Helper()
	s, owner, workspace := testStdioBroker(t)
	const id = "stdio_9a8b7c6d5e4f30129a8b7c6d5e4f3012"
	owner.Action = "terminal_read"
	owner.SessionID = id
	owner.SessionEpoch = "wait-epoch-1"
	owner.Limit = 8192
	e := &stdioSession{
		id: id, kind: "terminal", ownerID: owner.PrincipalID,
		ownerClass: owner.PrincipalClass, workspace: workspace,
		terminal: &tmuxTerminalState{epoch: owner.SessionEpoch},
	}
	if err := s.sessions.reserve(e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.sessions.remove(e) })
	return s, owner, e
}

func waitForTerminalWaiter(t *testing.T, e *stdioSession) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		waiting := e.terminal.readWaiter
		e.mu.Unlock()
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("bounded terminal reader did not enter wait without holding the session mutex")
}

func TestTerminalReadWaitInputAndDefaultBehavior(t *testing.T) {
	s, req, e := terminalWaitFixture(t)
	for _, n := range []int{-1, 2001, 1 << 20} {
		req.WaitMS = n
		got := s.terminalRead(req)
		if got.OK || got.ErrorCode != "invalid_wait" || got.Output != "" {
			t.Fatalf("invalid wait_ms=%d accepted: %+v", n, got)
		}
	}
	// No waiting on the default wire/behavior; the output cursor and
	// zero-byte response remain identical to existing clients.
	req.WaitMS = 0
	begin := time.Now()
	got := s.terminalRead(req)
	if !got.OK || got.NextCursor != 0 || got.Latest != 0 ||
		got.Output != "" || got.ReadWaitSupported || !*got.Running || time.Since(begin) > time.Second {
		t.Fatalf("legacy immediate read changed: %+v", got)
	}
	// Existing buffered output must be delivered immediately even if a
	// caller offered a long wait. Do not postpone legitimate history.
	e.mu.Lock()
	e.terminal.accept([]byte("%output %1 READY_FROM_BUFFER\n"))
	e.mu.Unlock()
	req.WaitMS = 2000
	begin = time.Now()
	got = s.terminalRead(req)
	if !got.OK || got.NextCursor != 1 || got.Latest != 1 || !got.ReadWaitSupported ||
		time.Since(begin) > time.Second {
		t.Fatalf("available terminal output was unnecessarily delayed: %+v", got)
	}
	out, err := base64.StdEncoding.DecodeString(got.Output)
	if err != nil || !bytes.Equal(out, []byte("READY_FROM_BUFFER")) {
		t.Fatalf("prebuffered output changed: %q err=%v", out, err)
	}
	// A future cursor cannot consume a waiter slot or renew a host lease.
	req.Cursor = 2
	if invalid := s.terminalRead(req); invalid.OK || invalid.ErrorCode != "invalid_cursor" {
		t.Fatalf("future cursor waited or succeeded: %+v", invalid)
	}
}

func TestTerminalReadWaitWakesWithoutBlockingWrites(t *testing.T) {
	s, req, e := terminalWaitFixture(t)
	req.WaitMS = 1250
	result := make(chan Response, 1)
	begin := time.Now()
	go func() { result <- s.terminalRead(req) }()
	waitForTerminalWaiter(t, e)

	foreign := req
	foreign.PrincipalID = "not-the-owner"
	if got := s.terminalRead(foreign); got.OK || got.ErrorCode != "session_not_found" {
		t.Fatalf("foreign principal accessed waiting PTY: %+v", got)
	}

	other := req
	other.WaitMS = 1200
	busyBegin := time.Now()
	busy := s.terminalRead(other)
	if busy.OK || busy.ErrorCode != "terminal_read_busy" || busy.Status != "busy" ||
		!busy.Retryable || time.Since(busyBegin) > time.Second {
		t.Fatalf("two sleeping readers admitted or wait was unbounded: %+v", busy)
	}

	immediate := req
	immediate.WaitMS = 0
	now := s.terminalRead(immediate)
	if !now.OK || now.NextCursor != 0 || now.BytesReturned != 0 {
		t.Fatalf("ordinary read blocked by a waiting reader: %+v", now)
	}
	// A real tmux stdout producer holds only e.mu to parse the data.
	// The sleeping read must NOT hold it (or t.actionMu) across waits.
	e.mu.Lock()
	e.terminal.accept([]byte("%output %2 ASYNC_NEW_OUTPUT\n"))
	e.mu.Unlock()

	select {
	case got := <-result:
		if !got.OK || got.NextCursor != 1 || got.Latest != 1 || !got.ReadWaitSupported ||
			got.BytesReturned != int64(len("ASYNC_NEW_OUTPUT")) ||
			time.Since(begin) > 1*time.Second {
			t.Fatalf("bounded reader did not promptly wake on output: %+v", got)
		}
		if len(got.TerminalEvents) != 1 || got.TerminalEvents[0].PaneID != "%2" {
			t.Fatalf("real pane identity was lost: %+v", got.TerminalEvents)
		}
		data, err := base64.StdEncoding.DecodeString(got.Output)
		if err != nil || !bytes.Equal(data, []byte("ASYNC_NEW_OUTPUT")) {
			t.Fatalf("new output corrupted: %q err=%v", data, err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("terminal writer could not wake the bounded reader")
	}
	e.mu.Lock()
	stillWaiting := e.terminal.readWaiter
	e.mu.Unlock()
	if stillWaiting {
		t.Fatal("terminal waiter admission was not released after new output")
	}

	// A timed-out read is a successful, complete zero-byte observation,
	// never a cancelled command or ambiguous/retained output replay.
	req.Cursor = 1
	req.WaitMS = 110
	timeoutBegin := time.Now()
	timed := s.terminalRead(req)
	elapsed := time.Since(timeoutBegin)
	if !timed.OK || timed.Status != "running" || timed.NextCursor != 1 ||
		timed.Latest != 1 || timed.BytesReturned != 0 || !timed.ReadWaitSupported || timed.Truncated ||
		len(timed.TerminalEvents) != 0 || elapsed < 65*time.Millisecond ||
		elapsed > time.Second {
		t.Fatalf("bounded no-output wait did not return normal empty cursor after deadline: elapsed=%s %+v", elapsed, timed)
	}
}

func TestTerminalReadWaitObservesLifecycleAndProtocolFaults(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*stdioSession)
		wantError string
		wantState string
	}{
		{
			name:      "disconnected",
			mutate:    func(e *stdioSession) { e.closed = true },
			wantState: "disconnected",
		},
		{
			name:      "epoch_replaced",
			mutate:    func(e *stdioSession) { e.terminal.epoch = "wait-epoch-2" },
			wantError: "terminal_epoch_changed",
		},
		{
			name:      "protocol_error",
			mutate:    func(e *stdioSession) { e.terminal.protocolError = "terminal_invalid_output_encoding" },
			wantError: "terminal_invalid_output_encoding",
		},
		{
			name:      "scope_uncertain",
			mutate:    func(e *stdioSession) { e.terminal.scopeUncertain = true },
			wantError: "terminal_start_uncertain",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, req, e := terminalWaitFixture(t)
			req.WaitMS = 1500
			done := make(chan Response, 1)
			go func() { done <- s.terminalRead(req) }()
			waitForTerminalWaiter(t, e)
			e.mu.Lock()
			tc.mutate(e)
			e.mu.Unlock()
			select {
			case got := <-done:
				if tc.wantError != "" {
					if got.OK || got.ErrorCode != tc.wantError || got.Output != "" {
						t.Fatalf("mid-wait failure was reported as successful PTY output: %+v", got)
					}
				} else if !got.OK || got.Status != tc.wantState || got.Output != "" || got.Running == nil || *got.Running {
					t.Fatalf("mid-wait disconnect incorrectly reported as running: %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("read ignored lifecycle change while waiting")
			}
			e.mu.Lock()
			wasWaiting := e.terminal.readWaiter
			e.mu.Unlock()
			if wasWaiting {
				t.Fatal("lifecycle failure leaked the session's one waiting-reader slot")
			}
		})
	}

	// Ring history loss must remain explicit and never be mistaken for
	// absence of output (which would cause a wasteful wait).
	t.Run("retention_gap", func(t *testing.T) {
		s, req, e := terminalWaitFixture(t)
		e.mu.Lock()
		for i := 0; i < 70; i++ {
			e.terminal.accept([]byte("%output %0 " + strings.Repeat("x", 1024) + "\n"))
		}
		e.mu.Unlock()
		req.WaitMS = 2000
		start := time.Now()
		got := s.terminalRead(req)
		if !got.OK || !got.Truncated || !got.RetentionTruncated ||
			got.Earliest < 2 || got.NextCursor <= 0 ||
			time.Since(start) > time.Second {
			t.Fatalf("overwritten terminal ring was masked by waiting: %+v", got)
		}
	})
}

// Source-level fixture, not a claim of installed Executor/root PTY admission.
// Opens real isolated worker tmux and proves the optional read waits for a
// new event without blocking a simultaneous terminal_write.
func TestWorkerTmuxReadWaitReceivesLaterInput(t *testing.T) {
	bin := os.Getenv("ASA_TEST_TMUX_BIN")
	if bin == "" {
		t.Skip("optional isolated tmux fixture not supplied")
	}
	s, owner, _ := testStdioBroker(t)
	s.terminalBinary = bin
	owner.Action, owner.Columns, owner.Rows = "terminal_open", 80, 24
	opened := s.terminalAction(owner)
	if !opened.OK {
		t.Fatalf("open disposable tmux: %+v", opened)
	}
	owner.SessionID, owner.SessionEpoch = opened.SessionID, opened.SessionEpoch
	t.Cleanup(func() {
		closeRequest := owner
		closeRequest.Action = "terminal_close"
		_ = s.terminalAction(closeRequest)
	})
	reader := owner
	reader.Action, reader.Limit = "terminal_read", 8192
	var cursor uint64
	for i := 0; i < 15; i++ {
		reader.Cursor, reader.WaitMS = cursor, 0
		r := s.terminalAction(reader)
		if !r.OK {
			t.Fatalf("drain initial tmux prompt: %+v", r)
		}
		cursor = r.NextCursor
		if cursor == r.Latest {
			break
		}
	}
	reader.Cursor, reader.WaitMS = cursor, 1600
	start := time.Now()
	result := make(chan Response, 1)
	go func() { result <- s.terminalAction(reader) }()
	time.Sleep(160 * time.Millisecond)
	writer := owner
	writer.Action, writer.Content = "terminal_write", "printf 'ASYNC_TERMINAL_WAIT_PROOF\\n'\n"
	wrote := s.terminalAction(writer)
	if !wrote.OK {
		t.Fatalf("tmux write blocked by waiting reader: %+v", wrote)
	}
	select {
	case got := <-result:
		if !got.OK || got.NextCursor <= cursor || got.BytesReturned == 0 ||
			!got.ReadWaitSupported || time.Since(start) > 2300*time.Millisecond {
			t.Fatalf("real tmux asynchronous output was not returned: %+v", got)
		}
		data, err := base64.StdEncoding.DecodeString(got.Output)
		if err != nil {
			t.Fatal(err)
		}
		cursor = got.NextCursor
		deadline := time.Now().Add(2 * time.Second)
		for !bytes.Contains(data, []byte("ASYNC_TERMINAL_WAIT_PROOF")) && time.Now().Before(deadline) {
			reader.Cursor, reader.WaitMS = cursor, 400
			follow := s.terminalAction(reader)
			if !follow.OK {
				t.Fatalf("follow-on tmux cursor: %+v", follow)
			}
			part, er := base64.StdEncoding.DecodeString(follow.Output)
			if er != nil {
				t.Fatal(er)
			}
			cursor = follow.NextCursor
			data = append(data, part...)
		}
		if !bytes.Contains(data, []byte("ASYNC_TERMINAL_WAIT_PROOF")) {
			t.Fatalf("real tmux command proof absent from waiting cursor output: %q", data)
		}
	case <-time.After(2400 * time.Millisecond):
		t.Fatal("tmux terminal reader did not receive delayed command output")
	}
}
