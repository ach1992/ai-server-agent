package browser

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

// Public browser_status must not call the short-lived Manager mutex the
// authoritative shared-profile lease. The executor owns that reservation.
func TestBrowserStatusTracksExecutorAdmissionAndRecovery(t *testing.T) {
	m := testBrowserManager(t)
	writeReadyBrowserFixture(t, m)
	m.token = "test-executor-token"
	sock := filepath.Join(t.TempDir(), "executor.sock")
	m.cfg.ExecutorSocket = sock
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var reserved atomic.Bool
	reserved.Store(true) // live managed Browser session owns the profile
	requests := make(chan executor.Request, 3)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req executor.Request
			if json.NewDecoder(conn).Decode(&req) == nil {
				requests <- req
				state := "available"
				if reserved.Load() {
					state = "reserved"
				}
				_ = json.NewEncoder(conn).Encode(executor.Response{OK: true, Status: state})
			}
			_ = conn.Close()
		}
	}()
	live := m.Status(context.Background())
	if !live.InspectionComplete || !live.Ready || !live.Busy || !strings.Contains(live.Reason, "reserved") {
		t.Fatalf("live Browser session advertised available: %+v", live)
	}
	reserved.Store(false) // verified broker close/release
	available := m.Status(context.Background())
	if !available.InspectionComplete || !available.Ready || available.Busy || available.Reason != "" {
		t.Fatalf("verified Browser close still reports reserved: %+v", available)
	}
	for i := 0; i < 2; i++ {
		req := <-requests
		if req.Action != "browser_admission_status" || req.Token != "test-executor-token" || req.SessionID != "" || req.PrincipalID != "" {
			t.Fatalf("admission probe exposed identity or lacked executor auth: %+v", req)
		}
	}
	_ = ln.Close()
	unknown := m.Status(context.Background())
	if unknown.InspectionComplete || !unknown.Busy || !strings.Contains(unknown.Reason, "could not be verified") {
		t.Fatalf("executor outage incorrectly reported shared Browser available: %+v", unknown)
	}
}
