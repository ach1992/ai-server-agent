package executor

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This fixture is deliberately opt-in and host-bound. It only controls two
// disposable processes that this exact test spawned and its own transient
// cgroup. It is not run by generic CI or normal unprivileged development.
func TestApprovedRootDAPCgroupEarlyAdapterCrash(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ASA_ROOT_DAP_CGROUP_APPROVED_HOST") != "CLY581741" || host != "CLY581741" || os.Geteuid() != 0 {
		t.Skip("requires explicit approved development host and root invocation")
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	workerUID, err := strconv.Atoi(worker.Uid)
	if err != nil {
		t.Fatal(err)
	}
	workerGID, err := strconv.Atoi(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "asa-owned-cgroup-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err = os.Chown(dir, workerUID, workerGID); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "owned-child-pid")
	script := fmt.Sprintf("read action; if test \"$action\" = begin; then /usr/bin/setsid /bin/sleep 60 </dev/null >/dev/null 2>&1 & echo $! > %q; wait; fi", marker)
	cmd := exec.Command("/bin/bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uint32(workerUID), Gid: uint32(workerGID), Groups: []uint32{uint32(workerGID)}}}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	pin := os.NewFile(uintptr(fd), "root-approved-adapter-pin")
	defer pin.Close()
	e := &stdioSession{pid: pid, pidPin: pin}
	cg, err := createDAPCgroup()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cg.path != "" {
			_ = cg.abortBeforeLaunch()
		}
	}()
	if err = cg.joinOriginalAdapter(e); err != nil {
		t.Fatal(err)
	}
	if !cg.containsPID(pid) {
		t.Fatal("adapter cgroup membership not verified")
	}
	if _, err = input.Write([]byte("begin\n")); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	childPID := 0
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID <= 0 || childPID == pid {
		t.Fatalf("disposable child PID not observed: %d", childPID)
	}
	if !cg.containsPID(childPID) {
		t.Fatal("child escaped root-owned cgroup before adapter crash")
	}
	pgrp, err := syscall.Getpgid(childPID)
	if err != nil || pgrp != childPID {
		t.Fatalf("child not in its own independent process group: pgid=%d err=%v", pgrp, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	} // only the direct disposable test adapter
	_ = cmd.Wait()
	d := &dapState{containment: cg, launchMayHaveTarget: true} // NO process event, NO child PID pin
	if err := d.stopPinnedDebuggee(); err != nil {
		t.Fatalf("pre-event adapter crash left untracked target: %v", err)
	}
	if !d.containmentProven || cg.path != "" {
		t.Fatal("cgroup cleanup proof not committed")
	}
	t.Logf("confirmed cgroup.kill cleaned independently grouped child after adapter SIGKILL before any process event (adapter %d, child %d)", pid, childPID)
}
