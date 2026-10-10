package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Delve starts a debuggee as a *separate* process-group leader, not as a
// member of the adapter's process group. Killing the adapter alone can leave
// executable code running indefinitely. Pin that exact target identity while
// the adapter is alive; the broker must stop BOTH groups after an adapter
// crash, terminal disconnect, explicit stop, or expiry.
//
// Never signal a PID merely because an adapter supplied it in a JSON event.
// We require a pidfd AND Linux-confirmed worker UID, direct parent Delve PID,
// executable inode identity and dedicated process-group leadership. A later
// signal always uses the still-open pidfd to prevent numeric PGID reuse.
func (d *dapState) pinDebuggeeProcess(body json.RawMessage) error {
	var reported struct {
		IsLocalProcess  bool `json:"isLocalProcess"`
		SystemProcessID int  `json:"systemProcessId"`
	}
	if err := json.Unmarshal(body, &reported); err != nil || !reported.IsLocalProcess || reported.SystemProcessID <= 0 {
		return errors.New("dap_debuggee_invalid_identity")
	}
	pid := reported.SystemProcessID
	if d.debuggeePin != nil {
		if d.debuggeePID != pid {
			return errors.New("dap_multiple_debuggees_unsupported")
		}
		return nil
	}
	if d.adapterPID <= 0 || d.workerUID == 0 {
		return errors.New("dap_adapter_identity_unavailable")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("dap_debuggee_pidfd_unavailable: %w", err)
	}
	pin := os.NewFile(uintptr(fd), "dap-debuggee-pidfd")
	valid := false
	defer func() {
		if !valid {
			_ = pin.Close()
		}
	}()
	if err := unix.PidfdSendSignal(int(pin.Fd()), 0, nil, 0); err != nil {
		return fmt.Errorf("dap_debuggee_pidfd_dead: %w", err)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return fmt.Errorf("dap_debuggee_status_unavailable: %w", err)
	}
	var parent int
	var uid uint64
	foundParent, foundUID := false, false
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			_, err = fmt.Sscanf(line, "PPid: %d", &parent)
			foundParent = err == nil
		}
		if strings.HasPrefix(line, "Uid:") {
			parts := strings.Fields(line)
			if len(parts) > 1 {
				uid, err = strconv.ParseUint(parts[1], 10, 32)
				foundUID = err == nil
			}
		}
	}
	if !foundParent || !foundUID || parent != d.adapterPID || uint32(uid) != d.workerUID {
		return errors.New("dap_debuggee_parent_or_uid_mismatch")
	}
	group, err := syscall.Getpgid(pid)
	if err != nil || group != pid {
		return errors.New("dap_debuggee_group_identity_mismatch")
	}
	workspaceExecutable := filepath.Join(d.workspace, d.program)
	targetInfo, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return fmt.Errorf("dap_debuggee_executable_unavailable: %w", err)
	}
	acceptedInfo, err := os.Stat(workspaceExecutable)
	if err != nil || !os.SameFile(targetInfo, acceptedInfo) {
		return errors.New("dap_debuggee_executable_mismatch")
	}
	// The adapter itself must still be the ORIGINAL live parent, not a
	// recycled numeric PPid. This check runs while d.mu excludes broker
	// cleanup/reaping of the adapter. A failed check poisons the session.
	if d.adapterPin == nil || unix.PidfdSendSignal(int(d.adapterPin.Fd()), 0, nil, 0) != nil {
		return errors.New("dap_original_adapter_no_longer_live")
	}
	if d.containment != nil && !d.containment.containsPID(pid) {
		return errors.New("dap_debuggee_is_not_in_executor_owned_containment")
	}
	// Validate the exact original debuggee pidfd after procfs provenance.
	// The production target is killed via its verified cgroup, never by a
	// bare numeric PID/PGID. Fixture-only tests still use safe group pidfd.
	signalFlags := 0
	if d.containment == nil {
		signalFlags = pidfdSignalProcessGroup
	}
	if err := unix.PidfdSendSignal(int(pin.Fd()), 0, nil, signalFlags); err != nil {
		return fmt.Errorf("dap_debuggee_original_identity_not_live: %w", err)
	}
	d.debuggeePID = pid
	d.debuggeePin = pin
	valid = true
	return nil
}

// Requires d.mu held. It can be called by both the ordinary close path and
// broker wait() after an unexpected Delve SIGKILL. Never silently discard a
// failed pin: that would turn an orphan into a successful session cleanup.
func (d *dapState) stopPinnedDebuggee() error {
	if d.containment != nil {
		// cgroup.kill atomically covers adapter, debuggee and all inherited
		// descendants, even without *any* validated DAP process event.
		if err := d.containment.killVerifyAndRelease(); err != nil {
			return fmt.Errorf("dap_containment_cleanup_unknown: %w", err)
		}
		d.containment = nil
		d.containmentProven = true
		if d.debuggeePin != nil {
			_ = d.debuggeePin.Close()
			d.debuggeePin = nil
		}
		d.debuggeePID = 0
		return nil
	}
	// The following alternative is exclusively for unprivileged fixture
	// tests with an explicit s.dlvBinary override; production never enters
	// it. Unobserved launch completion must remain unproven, not success.
	if d.debuggeePin == nil {
		if d.pinError != "" {
			return errors.New(d.pinError)
		}
		if d.launchMayHaveTarget {
			return errors.New("debuggee_identity_unobserved_after_launch_without_containment")
		}
		return nil
	}
	if d.debuggeePID <= 0 {
		return errors.New("dap_debuggee_pin_identity_invalid")
	}
	_, err := terminatePinnedGroup(d.debuggeePID, d.debuggeePin, false)
	if err != nil {
		return fmt.Errorf("dap_debuggee_cleanup_unknown: %w", err)
	}
	_ = d.debuggeePin.Close()
	d.debuggeePin = nil
	d.debuggeePID = 0
	if d.pinError != "" {
		return fmt.Errorf("dap_untracked_debuggee_identity: %s", d.pinError)
	}
	return nil
}

func (d *dapState) pinnedDebuggeeError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pinError != "" {
		return errors.New(d.pinError)
	}
	if d.debuggeePin == nil {
		return errors.New("dap_debuggee_identity_not_observed")
	}
	return nil
}
