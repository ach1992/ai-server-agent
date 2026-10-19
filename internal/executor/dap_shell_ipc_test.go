package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestShellDAPPtraceScopeFailClosed(t *testing.T) {
	for _, tc := range []struct {
		input    string
		accepted bool
	}{
		{"0", false}, {"1", true}, {"2", true}, {"3", true},
		{"-1", false}, {"4", false}, {"invalid", false}, {"", false},
	} {
		err := validateShellDAPPtraceScope([]byte(tc.input))
		if (err == nil) != tc.accepted {
			t.Fatalf("ptrace_scope %q accepted=%t, err=%v", tc.input, tc.accepted, err)
		}
	}
}

func TestShellDAPIPCKernelPrerequisiteFailClosed(t *testing.T) {
	// This Development host may deliberately run ptrace_scope=0. Never
	// silently accept that environment as a production Shell DAP target.
	if os.Geteuid() != 0 {
		if err := shellDAPIPCPrerequisite(); err == nil {
			t.Fatal("unprivileged process accepted as root IPC provider")
		}
		return
	}
	data, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil || strings.TrimSpace(string(data)) == "0" {
		if err := shellDAPIPCPrerequisite(); err == nil {
			t.Fatal("Yama disabled/unavailable but Shell DAP IPC admitted")
		}
	}
}

func TestShellDAPIPCHelperProcess(t *testing.T) {
	if os.Getenv("ASA_SHELL_DAP_IPC_HELPER") != "1" {
		t.Skip("test subprocess only")
	}
	p, err := inheritRootShellDAPIPC()
	if err != nil {
		fmt.Fprintln(os.Stderr, "inherited root pipe validation:", err)
		os.Exit(96)
	}
	if err := serveShellDAPProtected(os.Stdin, os.Stdout, p); err != nil {
		fmt.Fprintln(os.Stderr, "protected adapter:", err)
		os.Exit(97)
	}
	os.Exit(0)
}

func TestShellDAPIPCPeerProcess(t *testing.T) {
	if os.Getenv("ASA_SHELL_DAP_IPC_PEER") != "1" {
		t.Skip("test subprocess only")
	}
	pid, err := strconv.Atoi(os.Getenv("ASA_SHELL_DAP_VICTIM_PID"))
	if err != nil || pid <= 0 {
		os.Exit(94)
	}
	fds := []int{4, 6}
	if os.Getenv("ASA_SHELL_DAP_VICTIM_KIND") == "bash" {
		fds = []int{4, 5}
	}
	if err := unix.Kill(pid, 0); err != nil {
		fmt.Fprintln(os.Stderr, "original victim not alive:", err)
		os.Exit(95)
	}
	pin, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot address original PID:", err)
		os.Exit(96)
	}
	defer unix.Close(pin)
	for _, fd := range fds {
		path := fmt.Sprintf("/proc/%d/fd/%d", pid, fd)
		stolen, openErr := os.OpenFile(path, os.O_WRONLY, 0)
		if openErr == nil {
			stolen.Close()
			fmt.Fprintln(os.Stderr, "same-UID /proc reopen was permitted:", path)
			os.Exit(97)
		}
		if !errors.Is(openErr, syscall.EACCES) && !errors.Is(openErr, syscall.EPERM) {
			fmt.Fprintln(os.Stderr, "unexpected /proc reopen result:", openErr)
			os.Exit(98)
		}
		duplicated, copyErr := unix.PidfdGetfd(pin, fd, 0)
		if copyErr == nil {
			unix.Close(duplicated)
			fmt.Fprintln(os.Stderr, "same-UID pidfd_getfd escaped ptrace policy:", fd)
			os.Exit(99)
		}
		if !errors.Is(copyErr, syscall.EPERM) && !errors.Is(copyErr, syscall.EACCES) {
			fmt.Fprintln(os.Stderr, "unexpected pidfd_getfd result:", copyErr)
			os.Exit(100)
		}
	}
	os.Exit(0)
}

// This test must run in an isolated root-owned CI test process, not an
// occupied Agent service. It provisions ONLY its own four anonymous pipes,
// a worker-UID adapter/Bash fixture and two short-lived sibling probes.
// No global ptrace/sysctl changes, server connections, cgroup or host service
// mutation are performed. CI can require it with ASA_REQUIRE_ROOT_SHELL_IPC.
func TestShellDAPRootOwnedIPCRejectsSameUIDPeerAndPreservesActivation(t *testing.T) {
	require := os.Getenv("ASA_REQUIRE_ROOT_SHELL_IPC") == "1"
	if os.Geteuid() != 0 {
		if require {
			t.Fatal("required root IPC acceptance is not running as root")
		}
		t.Skip("root-owned ephemeral transport acceptance")
	}
	if err := shellDAPIPCPrerequisite(); err != nil {
		if require {
			t.Fatalf("disposable root environment cannot prove required IPC boundary: %v", err)
		}
		t.Skipf("unsafe kernel ptrace policy is correctly unavailable to Shell DAP: %v", err)
	}
	const workerUID, workerGID = 65534, 65534 // disposable test identity, not production config
	temp, err := os.MkdirTemp("/var/tmp", "asa-shell-ipc-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(temp)
	if err := os.Chmod(temp, 0755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspace, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0777); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(workspace, "example.sh")
	marker := filepath.Join(workspace, "result")
	source := "#!/usr/bin/env bash\nRESULT=7\nprintf '%s' \"$BASHPID\" > " + shellDAPQuotedTestArg(marker) + "\n"
	if err := os.WriteFile(script, []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(temp, "adapter-test")
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(current)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dest, err := os.OpenFile(binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dest, src); err != nil {
		dest.Close()
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	// tempfile parent may be worker-writable but the fixture executable
	// itself is root-owned, non-writable by the test worker UID.
	ipc, err := newRootShellDAPIPC()
	if err != nil {
		t.Fatal(err)
	}
	defer ipc.close()
	cmd := exec.Command(binary, "-test.run=^TestShellDAPIPCHelperProcess$")
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + workspace, "ASA_SHELL_DAP_IPC_HELPER=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: workerUID, Gid: workerGID, Groups: []uint32{workerGID}},
		Pdeathsig:  syscall.SIGKILL,
	}
	cmd.ExtraFiles = ipc.descriptors()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	// The root provider releases its copies after the child inherited
	// them; no unprivileged process can create an equivalent replacement.
	ipc.close()
	conn, bridge := net.Pipe()
	defer conn.Close()
	go func() { _, _ = io.Copy(stdin, bridge); _ = stdin.Close() }()
	go func() { _, _ = io.Copy(bridge, stdout); _ = bridge.Close() }()
	c := &shellDAPTestClient{t: t, conn: conn, parser: &dapState{conn: conn}}
	peer := func(pid int, kind string) {
		t.Helper()
		probe := exec.Command(binary, "-test.run=^TestShellDAPIPCPeerProcess$")
		probe.Env = []string{
			"PATH=/usr/bin:/bin",
			"ASA_SHELL_DAP_IPC_PEER=1",
			"ASA_SHELL_DAP_VICTIM_PID=" + strconv.Itoa(pid),
			"ASA_SHELL_DAP_VICTIM_KIND=" + kind,
		}
		probe.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: workerUID, Gid: workerGID, Groups: []uint32{workerGID},
		}}
		out, err := probe.CombinedOutput()
		if err != nil {
			t.Fatalf("unauthorized same-UID FD probe failed: %s: %v", out, err)
		}
	}
	peer(cmd.Process.Pid, "adapter")
	c.success("initialize", map[string]any{})
	c.event("initialized")
	c.success("launch", map[string]any{
		"program": script, "cwd": workspace, "fileVersion": shellDAPTestVersion(t, script), "stopOnEntry": true,
	})
	c.success("configurationDone", map[string]any{})
	process := c.event("process")
	var identity struct {
		SystemProcessID int `json:"systemProcessId"`
	}
	if err := json.Unmarshal(process.Body, &identity); err != nil || identity.SystemProcessID <= 0 {
		t.Fatalf("Bash original PID missing: %+v, %v", process, err)
	}
	peer(identity.SystemProcessID, "bash")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Bash executed before root-mediated START: %v", err)
	}
	c.success("asaActivate", map[string]any{})
	c.event("stopped")
	c.success("continue", map[string]any{"threadId": 1})
	c.event("terminated")
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != strconv.Itoa(identity.SystemProcessID) {
		t.Fatalf("original Bash PID/verified start not preserved: pid=%d marker=%q err=%v", identity.SystemProcessID, data, err)
	}
	c.success("disconnect", map[string]any{})
	_ = conn.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("protected adapter rejected original root transport: %v stderr=%s", err, stderr.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("protected adapter did not exit, stderr=%s", stderr.String())
	}
}

// A worker process can create the same four anonymous pipe FDs, but cannot
// manufacture the root-owned inode capability that the binary requires.
func TestShellDAPIPCRejectsWorkerCreatedPipeSubstitution(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root-owned positive path has its own ephemeral acceptance")
	}
	r1, w1, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		r1.Close()
		w1.Close()
		t.Fatal(err)
	}
	ipc := &shellDAPIPC{traceRead: r1, traceWrite: w1, commandRead: r2, commandWrite: w2}
	defer ipc.close()
	if err := ipc.validate(); err == nil {
		t.Fatal("worker-created IPC was accepted as root broker capability")
	}
}
