package executor

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// A standalone PID1 fixture, not the installed Agent: loss of every Go timer
// still leaves a real bounded scope. Short deadlines exercise the same native
// systemd-run/systemctl commands as the production v2 one-hour timer.
func TestApprovedPID1RenewedIdleTimerStopsAbandonedScope(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || !approvedPTYDisposableFixtureHost(host) {
		t.Skip("explicitly authorized disposable PID1 host required")
	}
	nonce, err := terminalEpoch()
	if err != nil {
		t.Fatal(err)
	}
	name := "asa_" + nonce
	unit, err := terminalScopeUnit(name)
	if err != nil {
		t.Fatal(err)
	}
	runner := systemexec.First("/usr/bin/systemd-run", "/bin/systemd-run")
	if runner == "" {
		t.Fatal("trusted systemd-run unavailable")
	}
	t.Cleanup(func() {
		if err := stopScopedTerminalBackend(name); err != nil {
			t.Errorf("disposable scope stop failed: %v", err)
		}
	})
	cmd := exec.Command(runner, "--scope", "--collect", "--quiet", "--unit="+strings.TrimSuffix(unit, ".scope"),
		"--property=RuntimeMaxSec=86400s", "--property=KillMode=control-group", "/bin/bash", "-c", "nohup /bin/sleep 45 >/dev/null 2>&1 </dev/null &")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("test-owned scope launch failed: %v %s", err, out)
	}
	if err := verifyScopedTerminalBackendForVersion(name, 2); err != nil {
		t.Fatalf("independent finite scope policy: %v", err)
	}
	if err := startTerminalLeaseTimer(name, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := verifyTerminalLeaseTimer(name); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if err := renewTerminalLeaseTimer(name); err != nil {
		t.Fatalf("native timer re-arm failed: %v", err)
	}
	time.Sleep(4 * time.Second)
	if err := verifyScopedTerminalBackendForVersion(name, 2); err != nil {
		t.Fatalf("PID1 prematurely expired active scope despite verified renewal: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if verifyScopedTerminalBackendForVersion(name, 2) != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := verifyScopedTerminalBackendForVersion(name, 2); err == nil {
		t.Fatal("unattended scope survived independent PID1 idle expiry")
	}
	t.Log("separate PID1 timer: renewed beyond initial deadline; unattended cgroup reaped without Executor")
}
