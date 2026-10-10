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
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		// If systemd-run succeeded before its own transport failed, this
		// exact random unit still needs reconciliation. Do not claim a
		// clean failure unless stopping that unit is proven.
		if stopErr := stopScopedTerminalBackend(t.name); stopErr != nil {
			return fmt.Errorf("terminal scope launch uncertain (%s): %w; cleanup: %v", unit, runErr, stopErr)
		}
		return fmt.Errorf("terminal scope launch rejected: %w (%s)", runErr, strings.TrimSpace(string(output)))
	}
	if err := verifyScopedTerminalBackend(t.name); err != nil {
		if stopErr := stopScopedTerminalBackend(t.name); stopErr != nil {
			return fmt.Errorf("terminal scope startup uncertain: %w; cleanup: %v", err, stopErr)
		}
		return fmt.Errorf("terminal scope verification failed: %w", err)
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
