package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// A production tmux backend must survive the Executor's systemd cgroup restart,
// but must NOT become an unbounded privileged orphan. A separate transient
// systemd scope owns each private tmux server and all its panes. The Executor
// still owns the authenticated, pidfd-pinned Control Mode client and session
// record; root-owned private sockets are never delegated to aiworker.
const terminalScopeCommandTimeout = 15 * time.Second

// This error is reserved for cases where PID1 may own a privileged backend
// and the exact-scope stop CANNOT be proven. The caller must retain the
// principal-bound in-memory session identity and return its opaque ID.
type terminalScopeCleanupUncertain struct {
	phase   string
	cause   error
	cleanup error
}

func (e *terminalScopeCleanupUncertain) Error() string {
	return fmt.Sprintf("terminal scope %s outcome uncertain: %v; cleanup: %v", e.phase, e.cause, e.cleanup)
}

func (e *terminalScopeCleanupUncertain) Unwrap() error { return e.cause }

// Isolating the lifecycle decision permits deterministic fault injection for
// both run-unknown and policy-verification-unknown paths without needing to
// run systemd or mutate any host. A proven exact-scope stop is the only
// condition under which a startup failure is a clean failure.
func startTerminalScopeLifecycle(name string, run func() error, verify func() error, stop func(string) error) error {
	if err := run(); err != nil {
		if stopErr := stop(name); stopErr != nil {
			return &terminalScopeCleanupUncertain{phase: "launch", cause: err, cleanup: stopErr}
		}
		return fmt.Errorf("terminal scope launch rejected: %w", err)
	}
	if err := verify(); err != nil {
		if stopErr := stop(name); stopErr != nil {
			return &terminalScopeCleanupUncertain{phase: "verification", cause: err, cleanup: stopErr}
		}
		return fmt.Errorf("terminal scope verification failed: %w", err)
	}
	return nil
}

func terminalScopeUnit(name string) (string, error) {
	if !validTmuxSessionName(name) {
		return "", errors.New("untrusted terminal generation")
	}
	return "asa-pty-" + strings.TrimPrefix(name, "asa_") + ".scope", nil
}

func (s *Server) startScopedTerminalBackend(req Request, binary string, t *tmuxTerminalState, initialArgs []string) error {
	if os.Geteuid() != 0 || s.terminalBinary != "" || t == nil || t.recovered ||
		req.Root != t.root || req.Approval != req.Root || req.PrincipalID == "" || req.PrincipalClass == "" {
		return errors.New("terminal scope requires authenticated production executor authority")
	}
	if len(initialArgs) < 8 || initialArgs[0] != "-f" || initialArgs[1] != "/dev/null" ||
		initialArgs[2] != "-S" || initialArgs[3] != t.socket ||
		initialArgs[4] != "-C" || initialArgs[5] != "new-session" ||
		initialArgs[6] != "-s" || initialArgs[7] != t.name {
		return errors.New("unexpected terminal launch arguments")
	}
	unit, err := terminalScopeUnit(t.name)
	if err != nil {
		return err
	}
	runner := systemexec.First("/usr/bin/systemd-run", "/bin/systemd-run")
	if runner == "" {
		return errors.New("trusted systemd-run is required for recoverable tmux isolation")
	}
	// systemd-run --scope launches a detached tmux backend in a different
	// cgroup; the original -C new-session subprocess instead lived in the
	// Executor cgroup and systemd killed its server during service restart.
	// A scope lifetime of at most the broker's existing one-hour session TTL
	// guarantees cleanup even if the Executor dies before its Go timer fires.
	args := []string{
		"--scope", "--collect", "--quiet", "--unit=" + strings.TrimSuffix(unit, ".scope"),
		"--property=RuntimeMaxSec=" + strconv.FormatInt(int64(maxStdioSessionAge/time.Second), 10) + "s",
		"--property=KillMode=control-group",
		binary, "-f", "/dev/null", "-S", t.socket, "new-session", "-d",
	}
	args = append(args, initialArgs[6:]...)
	ctx, cancel := context.WithTimeout(context.Background(), terminalScopeCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runner, args...)
	cmd.Dir = req.Workspace
	cmd.Env = append(sanitizedSessionEnv("/root"), "TERM=xterm-256color")
	var output []byte
	err = startTerminalScopeLifecycle(t.name, func() error {
		var runErr error
		output, runErr = cmd.CombinedOutput()
		if runErr != nil {
			return fmt.Errorf("systemd-run: %w (%s)", runErr, strings.TrimSpace(string(output)))
		}
		return nil
	}, func() error {
		return verifyScopedTerminalBackend(t.name)
	}, stopScopedTerminalBackend)
	if err != nil {
		return err
	}
	t.scoped = true
	return nil
}

func verifyScopedTerminalBackend(name string) error {
	unit, err := terminalScopeUnit(name)
	if err != nil {
		return err
	}
	systemctl := systemexec.First("/usr/bin/systemctl", "/bin/systemctl")
	if systemctl == "" {
		return errors.New("trusted systemctl is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalScopeCommandTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, systemctl, "show", "--no-pager",
		"-p", "LoadState", "-p", "ActiveState", "-p", "ControlGroup",
		"-p", "KillMode", "-p", "RuntimeMaxUSec", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot verify terminal scope: %w", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[key] = value
		}
	}
	if values["LoadState"] != "loaded" || values["ActiveState"] != "active" ||
		values["KillMode"] != "control-group" ||
		values["ControlGroup"] != "/system.slice/"+unit ||
		values["RuntimeMaxUSec"] != "1h" {
		return fmt.Errorf("terminal scope boundary or lifetime not verified")
	}
	return nil
}

func stopScopedTerminalBackend(name string) error {
	unit, err := terminalScopeUnit(name)
	if err != nil {
		return err
	}
	systemctl := systemexec.First("/usr/bin/systemctl", "/bin/systemctl")
	if systemctl == "" {
		return errors.New("trusted systemctl is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalScopeCommandTimeout)
	defer cancel()
	stopOutput, stopErr := exec.CommandContext(ctx, systemctl, "stop", unit).CombinedOutput()
	inspect := exec.CommandContext(ctx, systemctl, "show", "--no-pager",
		"-p", "LoadState", "-p", "ActiveState", unit)
	inspectOutput, inspectErr := inspect.CombinedOutput()
	if inspectErr != nil {
		return fmt.Errorf("terminal scope shutdown unverified: %w", inspectErr)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(inspectOutput), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[key] = value
		}
	}
	// A --collect transient scope can disappear immediately after stop.
	// "not-found" + "inactive" is a verified cleaned unit, not an error.
	if values["ActiveState"] == "inactive" && (values["LoadState"] == "not-found" || values["LoadState"] == "loaded") {
		return nil
	}
	if stopErr != nil {
		return fmt.Errorf("terminal scope stop unverified: %w (%s)", stopErr, strings.TrimSpace(string(stopOutput)))
	}
	return errors.New("terminal scope still active or in an unverified state after stop")
}
