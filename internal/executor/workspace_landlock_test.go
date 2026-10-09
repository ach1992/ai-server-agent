package executor

import (
	"bufio"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireWorkerLandlockV2(t *testing.T) {
	t.Helper()
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	if errno != 0 || abi < 2 {
		t.Skip("Landlock ABI v2 unavailable; helper fails closed on this kernel")
	}
}

func TestWorkerLandlockFailsClosedOutside(t *testing.T) {
	if os.Getenv("ASA_LANDLOCK_PROBE") == "1" {
		root, repo, outside := os.Getenv("ASA_LANDLOCK_ROOT"), os.Getenv("ASA_LANDLOCK_REPO"), os.Getenv("ASA_LANDLOCK_OUTSIDE")
		unlock, err := workerLandlockRestrict(root, repo, false)
		if err != nil {
			fmt.Println("LANDLOCK_UNAVAILABLE")
			return
		}
		defer unlock()
		if _, err := os.ReadFile(outside); err == nil {
			t.Fatal("read escaped selected workspace sandbox")
		}
		if err := os.WriteFile(outside, []byte("changed"), 0644); err == nil {
			t.Fatal("write escaped selected workspace sandbox")
		}
		if err := os.WriteFile(filepath.Join(repo, "inside.txt"), []byte("allowed"), 0644); err != nil {
			t.Fatalf("sandbox blocked contained worker write: %v", err)
		}
		fmt.Println("LANDLOCK_OK")
		return
	}
	root, repo, _ := workspaceFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerLandlockFailsClosedOutside$")
	cmd.Env = append(os.Environ(), "ASA_LANDLOCK_PROBE=1", "ASA_LANDLOCK_ROOT="+root, "ASA_LANDLOCK_REPO="+repo, "ASA_LANDLOCK_OUTSIDE="+outside)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kernel sandbox regression: %v: %s", err, output)
	}
	if strings.Contains(string(output), "LANDLOCK_UNAVAILABLE") {
		t.Skip("kernel lacks Landlock ABI v2; tool fails closed")
	}
	if !strings.Contains(string(output), "LANDLOCK_OK") {
		t.Fatalf("sandbox did not run: %s", output)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "sensitive" {
		t.Fatalf("outside file changed: %v %q", err, data)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "inside.txt")); err != nil || string(data) != "allowed" {
		t.Fatalf("worker sandbox inside write failed: %v %q", err, data)
	}
}

// Exposes the precise race: a directory FD is pinned while another process
// re-parents that directory outside the workspace. The worker must not be
// able to create a new file through the stale descriptor after reparenting.
func TestWorkerLandlockReparentedParent(t *testing.T) {
	if os.Getenv("ASA_LANDLOCK_REPARENT") == "1" {
		root, repo := os.Getenv("ASA_LANDLOCK_ROOT"), os.Getenv("ASA_LANDLOCK_REPO")
		unlock, err := workerLandlockRestrict(root, repo, false)
		if err != nil {
			fmt.Println("UNAVAILABLE")
			return
		}
		defer unlock()
		op := workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Path: "src/created.txt"}
		ws, err := openWorkerWorkspace(op)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		parent, _, err := openWorkerParent(int(ws.Fd()), op.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		fmt.Println("READY")
		signal := make([]byte, 1)
		if _, err := os.Stdin.Read(signal); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Openat(int(parent.Fd()), "created.txt", unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, 0600)
		if err == nil {
			unix.Close(fd)
			t.Fatal("Landlock allowed creation after parent escaped workspace")
		}
		fmt.Println("DENIED")
		return
	}
	root, repo, _ := workspaceFixture(t)
	outside := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerLandlockReparentedParent$")
	cmd.Env = append(os.Environ(), "ASA_LANDLOCK_REPARENT=1", "ASA_LANDLOCK_ROOT="+root, "ASA_LANDLOCK_REPO="+repo)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("missing child ready signal: %v", scanner.Err())
	}
	signal := scanner.Text()
	if signal == "UNAVAILABLE" {
		stdin.Close()
		cmd.Wait()
		t.Skip("Landlock unsupported, workspace tool fails closed")
	}
	if signal != "READY" {
		t.Fatalf("child response %q", signal)
	}
	if err := os.Rename(filepath.Join(repo, "src"), filepath.Join(outside, "src")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("g")); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	if !scanner.Scan() {
		t.Fatalf("missing result: %v", scanner.Err())
	}
	result := scanner.Text()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child sandbox failure %v, result=%s", err, result)
	}
	if result != "DENIED" {
		t.Fatalf("unexpected race outcome %q", result)
	}
	if _, err := os.Stat(filepath.Join(outside, "src", "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("write escaped: %v", err)
	}
}

func TestWorkerLandlockMovedSelectedRoot(t *testing.T) {
	if os.Getenv("ASA_LANDLOCK_MOVE_ROOT") == "1" {
		root, repo := os.Getenv("ASA_LANDLOCK_ROOT"), os.Getenv("ASA_LANDLOCK_REPO")
		unlock, err := workerLandlockRestrict(root, repo, false)
		if err != nil {
			fmt.Println("UNAVAILABLE")
			return
		}
		defer unlock()
		op := workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Path: "src/moved-created.txt"}
		ws, err := openWorkerWorkspace(op)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		parent, _, err := openWorkerParent(int(ws.Fd()), op.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		fmt.Println("READY")
		signal := make([]byte, 1)
		if _, err := os.Stdin.Read(signal); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Openat(int(parent.Fd()), "moved-created.txt", unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, 0600)
		if err == nil {
			unix.Close(fd)
			t.Fatal("Landlock allowed creation in selected root moved outside configured workspace")
		}
		fmt.Println("DENIED")
		return
	}
	root, repo, _ := workspaceFixture(t)
	outside := t.TempDir()
	moved := filepath.Join(outside, "repo")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerLandlockMovedSelectedRoot$")
	cmd.Env = append(os.Environ(), "ASA_LANDLOCK_MOVE_ROOT=1", "ASA_LANDLOCK_ROOT="+root, "ASA_LANDLOCK_REPO="+repo)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("missing child ready signal: %v", scanner.Err())
	}
	signal := scanner.Text()
	if signal == "UNAVAILABLE" {
		stdin.Close()
		cmd.Wait()
		t.Skip("Landlock unsupported, helper fails closed")
	}
	if signal != "READY" {
		t.Fatalf("child response %q", signal)
	}
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("g")); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	if !scanner.Scan() {
		t.Fatalf("missing result: %v", scanner.Err())
	}
	result := scanner.Text()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child sandbox failure: %v, result=%s", err, result)
	}
	if result != "DENIED" {
		t.Fatalf("unexpected root reparent outcome %q", result)
	}
	if _, err := os.Stat(filepath.Join(moved, "src", "moved-created.txt")); !os.IsNotExist(err) {
		t.Fatalf("write escaped: %v", err)
	}
}
