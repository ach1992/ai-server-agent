package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// A dedicated root-owned cgroup-v2 child contains Delve and *all* processes
// it subsequently launches, regardless of their process groups or whether
// the adapter reports a DAP process event. cgroup.kill is the only accepted
// proof of cleanup for pre-identity or multi-descendant DAP launches.
// The executor service itself is NEVER written into or killed by this code.
type dapCgroup struct {
	path     string
	verified bool
}

func createDAPCgroup() (*dapCgroup, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("dap containment requires root executor")
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	var relative string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::/") {
			relative = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if relative == "" || filepath.Base(relative) != "ai-server-agent-executor.service" || filepath.Clean(relative) != relative {
		return nil, errors.New("executor cgroup identity is not the dedicated Agent executor service")
	}
	parent := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(relative, "/"))
	if _, err := os.Stat(filepath.Join(parent, "cgroup.procs")); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(parent, "asa-dap-")
	if err != nil {
		return nil, fmt.Errorf("private DAP cgroup unavailable: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("kernel cgroup.kill unavailable: %w", err)
	}
	return &dapCgroup{path: dir}, nil
}

func (g *dapCgroup) joinOriginalAdapter(e *stdioSession) error {
	if g == nil || g.path == "" {
		return errors.New("DAP containment is not provisioned")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.exited || e.pidPin == nil || e.pid <= 0 {
		return errors.New("adapter was not live before containment")
	}
	if err := unix.PidfdSendSignal(int(e.pidPin.Fd()), 0, nil, 0); err != nil {
		return fmt.Errorf("adapter not live for cgroup join: %w", err)
	}
	pid := e.pid
	if err := os.WriteFile(filepath.Join(g.path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0600); err != nil {
		return fmt.Errorf("DAP cgroup join failed: %w", err)
	}
	// Check actual kernel membership, not only a successful write. The direct
	// child PID is still waitable (and cannot be recycled) under e.stopMu.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return fmt.Errorf("DAP cgroup membership not verifiable: %w", err)
	}
	rel := strings.TrimPrefix(g.path, "/sys/fs/cgroup")
	if !strings.Contains(string(data), "0::"+rel+"\n") && strings.TrimSpace(string(data)) != "0::"+rel {
		return errors.New("Delve was not attached to its intended cgroup")
	}
	if err := unix.PidfdSendSignal(int(e.pidPin.Fd()), 0, nil, 0); err != nil {
		return fmt.Errorf("adapter exited before cgroup verify: %w", err)
	}
	g.verified = true
	return nil
}

func (g *dapCgroup) populated() (bool, error) {
	if g == nil || g.path == "" {
		return false, errors.New("DAP containment path unavailable")
	}
	data, err := os.ReadFile(filepath.Join(g.path, "cgroup.events"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "populated 1" {
			return true, nil
		}
		if strings.TrimSpace(line) == "populated 0" {
			return false, nil
		}
	}
	return false, errors.New("DAP containment populated state unavailable")
}

// Only this freshly created, session-owned cgroup.kill may be written. Do
// not use kill(-numeric PGID), ancestor cgroup.kill or /proc enumeration.
func (g *dapCgroup) killVerifyAndRelease() error {
	if g == nil || !g.verified {
		return errors.New("DAP containment provenance not verified")
	}
	if err := os.WriteFile(filepath.Join(g.path, "cgroup.kill"), []byte("1"), 0600); err != nil {
		return fmt.Errorf("DAP cgroup kill not proven: %w", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		busy, err := g.populated()
		if err != nil {
			return fmt.Errorf("DAP cgroup empty proof unavailable: %w", err)
		}
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("DAP cgroup descendants survived kill timeout")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := os.Remove(g.path); err != nil {
		return fmt.Errorf("DAP cgroup removal unverified: %w", err)
	}
	g.verified = false
	g.path = ""
	return nil
}

// An empty unused cgroup may be removed before any DAP launch request; do
// not discard a populated/unverified cgroup, which may contain a process.
// A failed pre-launch cgroup attachment may have moved the adapter already.
// Kill only the newly created private cgroup; no traced target could exist
// because the launch DAP request has not yet been transmitted.
func (g *dapCgroup) abortBeforeLaunch() error {
	if g == nil || g.path == "" {
		return nil
	}
	if err := os.WriteFile(filepath.Join(g.path, "cgroup.kill"), []byte("1"), 0600); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		busy, err := g.populated()
		if err != nil {
			return err
		}
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("prelaunch DAP cgroup not empty after kill")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := os.Remove(g.path); err != nil {
		return err
	}
	g.path = ""
	g.verified = false
	return nil
}

func (g *dapCgroup) containsPID(pid int) bool {
	if g == nil || !g.verified || pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "0::"+strings.TrimPrefix(g.path, "/sys/fs/cgroup")
}

func (g *dapCgroup) removeUnused() error {
	if g == nil || g.path == "" {
		return nil
	}
	busy, err := g.populated()
	if err != nil {
		return err
	}
	if busy {
		return errors.New("unused DAP cgroup unexpectedly contains a process")
	}
	if err := os.Remove(g.path); err != nil {
		return err
	}
	g.path = ""
	return nil
}
