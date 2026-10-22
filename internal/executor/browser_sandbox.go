package executor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Chromium's own --no-sandbox flag is insufficient evidence: a requested
// sandbox is reported ready only when a renderer in the original root-owned
// Browser cgroup has seccomp-BPF, no_new_privs and distinct user/PID namespaces.
// This is process-start assurance, NOT per-principal Linux identity isolation
// (all isolated managed sessions still share aibrowser).
type browserSandboxProcess struct {
	effectiveUID uint32
	effectiveGID uint32
	seccomp      uint32
	noNewPrivs   uint32
	userNS       string
	pidNS        string
}

func sandboxStatusField(status []byte, key string, index int) (uint32, error) {
	for _, line := range bytes.Split(status, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte(key+":")) {
			continue
		}
		fields := strings.Fields(string(line[len(key)+1:]))
		if len(fields) <= index {
			break
		}
		value, err := strconv.ParseUint(fields[index], 10, 32)
		if err == nil {
			return uint32(value), nil
		}
		break
	}
	return 0, fmt.Errorf("missing or invalid Browser /proc status %s", key)
}

func readBrowserSandboxProcess(pid int) (browserSandboxProcess, error) {
	if pid <= 0 {
		return browserSandboxProcess{}, errors.New("invalid Chromium PID")
	}
	root := fmt.Sprintf("/proc/%d", pid)
	status, err := os.ReadFile(filepath.Join(root, "status"))
	if err != nil {
		return browserSandboxProcess{}, err
	}
	var p browserSandboxProcess
	for _, part := range []struct {
		key   string
		index int
		dst   *uint32
	}{
		{"Uid", 1, &p.effectiveUID}, {"Gid", 1, &p.effectiveGID},
		{"Seccomp", 0, &p.seccomp}, {"NoNewPrivs", 0, &p.noNewPrivs},
	} {
		*part.dst, err = sandboxStatusField(status, part.key, part.index)
		if err != nil {
			return browserSandboxProcess{}, err
		}
	}
	p.userNS, err = os.Readlink(filepath.Join(root, "ns", "user"))
	if err != nil {
		return browserSandboxProcess{}, err
	}
	p.pidNS, err = os.Readlink(filepath.Join(root, "ns", "pid"))
	if err != nil {
		return browserSandboxProcess{}, err
	}
	return p, nil
}

func (p browserSandboxProcess) confinedFrom(parent browserSandboxProcess, uid, gid uint32) bool {
	return p.effectiveUID == uid && p.effectiveGID == gid &&
		p.seccomp == 2 && p.noNewPrivs == 1 &&
		p.userNS != "" && parent.userNS != "" && p.userNS != parent.userNS &&
		p.pidNS != "" && parent.pidNS != "" && p.pidNS != parent.pidNS
}

// Only the original private cgroup is inspected. Never scan arbitrary host
// processes to infer sandbox readiness from another browser. The Node PID is
// the existing broker-pinned process, not a user-provided PID.
func (s *Server) verifyManagedBrowserSandbox(entry *stdioSession) error {
	entry.mu.Lock()
	group, nodePID, private := entry.browserGroup, entry.pid, entry.browserPrivate
	entry.mu.Unlock()
	if !private || group == nil || !group.verified || !group.containsPID(nodePID) {
		return errors.New("isolated Browser cgroup/identity proof unavailable")
	}
	node, err := readBrowserSandboxProcess(nodePID)
	if err != nil {
		return fmt.Errorf("isolated Browser Node identity or namespace unreadable: %w", err)
	}
	if node.effectiveUID != s.browserUID || node.effectiveGID != s.browserGID {
		return errors.New("isolated Browser Node did not retain its dedicated UID/GID")
	}
	// The generated Node runner has already created and evaluated one
	// about:blank page before emitting ready, allowing bounded startup proof.
	// Short retries tolerate /proc renderer process turnover.
	deadline := time.Now().Add(1200 * time.Millisecond)
	for {
		pids, err := os.ReadFile(filepath.Join(group.path, "cgroup.procs"))
		if err != nil || len(pids) > 16384 {
			return errors.New("isolated Browser cgroup process inventory unavailable")
		}
		renderers, allEnforced := 0, true
		for _, line := range strings.Fields(string(pids)) {
			raw, parseErr := strconv.ParseUint(line, 10, 31)
			if parseErr != nil || raw == 0 {
				return errors.New("isolated Browser cgroup process inventory malformed")
			}
			pid := int(raw)
			if !group.containsPID(pid) {
				continue // raced with normal Chromium shutdown
			}
			cmdline, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
			if readErr != nil {
				continue
			}
			args := bytes.Split(cmdline, []byte{0})
			renderer := false
			for _, arg := range args {
				if bytes.Equal(arg, []byte("--no-sandbox")) ||
					bytes.HasPrefix(arg, []byte("--no-sandbox=")) {
					return errors.New("Chromium disabled its sandbox despite the explicit request")
				}
				if bytes.Equal(arg, []byte("--type=renderer")) {
					renderer = true
				}
			}
			if !renderer {
				continue
			}
			renderers++
			proc, readErr := readBrowserSandboxProcess(pid)
			if readErr != nil || !proc.confinedFrom(node, s.browserUID, s.browserGID) {
				allEnforced = false
			}
		}
		if renderers > 0 && allEnforced {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("not all Chromium renderers have seccomp-BPF, no_new_privs and separate user/PID namespaces")
		}
		time.Sleep(35 * time.Millisecond)
	}
}
