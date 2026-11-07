package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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
)

// This optional, disposable root-only CI/Dev test exercises actual Linux host
// UID mappings and metadata syscalls. No login accounts, Agent service,
// cgroup service, real Browser profile, or installed files are modified.
// Pinned Chromium/Playwright feature coverage runs separately in Browser CI.
func TestMappedBrowserSessionHostUIDAcrossPeers(t *testing.T) {
	if os.Getenv("ASA_REQUIRE_BROWSER_MAPPED_HOST_UID") != "1" {
		t.Skip("root-only synthetic mapped-host-UID acceptance is opt-in")
	}
	if os.Geteuid() != 0 {
		t.Fatal("mapped host UID acceptance must run as root")
	}
	for _, uid := range []uint32{browserSessionHostUIDBase, browserSessionHostUIDBase + 1} {
		if !validBrowserSessionHostUID(uid) {
			t.Fatal("invalid reserved Browser host UID")
		}
		if _, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
			t.Fatalf("reserved Browser UID %d is already an OS account", uid)
		} else {
			var unknown user.UnknownUserIdError
			if !errors.As(err, &unknown) {
				t.Fatalf("host UID lookup failed: %v", err)
			}
		}
	}
	const insideUID, insideGID, hostGID = 999, 987, 987
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("/var/tmp", "asa-browser-mapped-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("reclaim disposable synthetic UID fixture: %v", err)
		}
	}()
	if err := os.Chmod(root, 0711); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "isolated-sessions")
	if err := os.Mkdir(scratch, 0711); err != nil {
		t.Fatal(err)
	}
	// The installed Executor service runs with a private primary group;
	// the installer explicitly provisions this root-controlled parent
	// as root:root, independent of the service's group.
	if err := os.Chown(scratch, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scratch, 0711); err != nil {
		t.Fatal(err)
	}
	profiles := []string{filepath.Join(scratch, "asa-profile-"+strings.Repeat("a", 32)),
		filepath.Join(scratch, "asa-profile-"+strings.Repeat("b", 32))}
	traces := []string{filepath.Join(scratch, "asa-trace-"+strings.Repeat("a", 32)),
		filepath.Join(scratch, "asa-trace-"+strings.Repeat("b", 32))}
	for i, hostUID := range []uint32{browserSessionHostUIDBase, browserSessionHostUIDBase + 1} {
		for _, dir := range []string{profiles[i], traces[i]} {
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(dir, int(hostUID), hostGID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Root-owned parent remains searchable but not writable; each Browser
	// owns only its own two private directories.
	run := func(i int, script string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-u", "-c", script)
		cmd.Dir = "/"
		cmd.Env = append(os.Environ(),
			"OWN_PROFILE="+profiles[i], "OWN_TRACE="+traces[i],
			"PEER_PROFILE="+profiles[1-i], "PEER_TRACE="+traces[1-i])
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWUSER,
			UidMappings: []syscall.SysProcIDMap{{
				ContainerID: insideUID, HostID: int(browserSessionHostUIDBase) + i, Size: 1,
			}},
			GidMappings: []syscall.SysProcIDMap{{
				ContainerID: insideGID, HostID: hostGID, Size: 1,
			}},
			GidMappingsEnableSetgroups: false,
			Credential: &syscall.Credential{
				Uid: insideUID, Gid: insideGID, NoSetGroups: true,
			},
		}
		return cmd
	}
	bScript := `import os,sys
from pathlib import Path
p=Path(os.environ["OWN_PROFILE"])
t=Path(os.environ["OWN_TRACE"])/"trace.zip"
(p/"cookie").write_bytes(b"B-ORIGINAL")
os.chmod(p/"cookie",0o600)
t.write_bytes(b"PK"+b"B-TRACE")
os.chmod(t,0o600)
print("B_READY",flush=True)
sys.stdin.readline()
assert (p/"cookie").read_bytes()==b"B-ORIGINAL"
assert t.read_bytes()==b"PK"+b"B-TRACE"
(p/"next-browser-action").write_text("B-CONTINUES")
# Authorized code is free to change its own scratch metadata; verified
# root cleanup must remain possible even after that ordinary operation.
os.chmod(p,0o755)
os.chmod(Path(os.environ["OWN_TRACE"]),0o755)
print("B_CONTINUES",flush=True)
`
	b := run(1, bScript)
	stdin, err := b.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := b.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	b.Stderr = &stderr
	if err := b.Start(); err != nil {
		t.Fatalf("start isolated peer B: %v", err)
	}
	defer func() {
		_ = b.Process.Kill()
		_ = b.Wait()
	}()
	reader := bufio.NewReader(stdout)
	ready, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "B_READY" {
		t.Fatalf("isolated peer B not ready: %q %v %s", ready, err, stderr.String())
	}
	cookie := filepath.Join(profiles[1], "cookie")
	trace := filepath.Join(traces[1], "trace.zip")
	beforeCookie, err := os.ReadFile(cookie)
	if err != nil {
		t.Fatal(err)
	}
	beforeTrace, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	beforeTraceInfo, err := os.Stat(trace)
	if err != nil {
		t.Fatal(err)
	}
	aScript := `import os,shutil
from pathlib import Path
own=Path(os.environ["OWN_PROFILE"])
peer=Path(os.environ["PEER_PROFILE"])
trace=Path(os.environ["PEER_TRACE"])/"trace.zip"
cookie=peer/"cookie"
blocked=[]
for attempt in (
 lambda:os.chmod(peer,0o777),
 lambda:os.chmod(cookie,0o777),
 lambda:os.chmod(trace,0o777),
 lambda:os.utime(trace,(1,1)),
 lambda:cookie.write_bytes(b"CORRUPTED"),
 lambda:os.close(os.open(cookie,os.O_RDONLY|os.O_TRUNC)),
 lambda:cookie.unlink(),
 lambda:shutil.rmtree(peer),
):
 try: attempt();blocked.append(False)
 except (PermissionError,FileNotFoundError):blocked.append(True)
own_file=own/"own-cookie"
own_file.write_bytes(b"A-OWN")
os.chmod(own_file,0o600)
os.utime(own_file,(10,10))
print("A_BLOCKED="+str(sum(blocked))+" OWN="+own_file.read_text(),flush=True)
if not all(blocked):raise SystemExit(4)
`
	a := run(0, aScript)
	output, err := a.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("A_BLOCKED=8 OWN=A-OWN")) {
		t.Fatalf("isolated peer A metadata/content denial failed: %v %s", err, output)
	}
	afterCookie, err := os.ReadFile(cookie)
	if err != nil {
		t.Fatal(err)
	}
	afterTrace, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	afterTraceInfo, err := os.Stat(trace)
	if err != nil {
		t.Fatal(err)
	}
	peerProfileInfo, err := os.Stat(profiles[1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeCookie, afterCookie) ||
		sha256.Sum256(beforeTrace) != sha256.Sum256(afterTrace) ||
		!beforeTraceInfo.ModTime().Equal(afterTraceInfo.ModTime()) ||
		beforeTraceInfo.Mode().Perm() != afterTraceInfo.Mode().Perm() ||
		peerProfileInfo.Mode().Perm() != 0700 {
		t.Fatal("peer Browser cookie, trace or profile metadata was altered")
	}
	if _, err := stdin.Write([]byte("continue\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(next) != "B_CONTINUES" {
		t.Fatalf("peer Browser could not continue after attempted corruption: %v %q %s", err, next, stderr.String())
	}
	if err := b.Wait(); err != nil {
		t.Fatalf("peer Browser exit: %v %s", err, stderr.String())
	}
	if action, err := os.ReadFile(filepath.Join(profiles[1], "next-browser-action")); err != nil ||
		string(action) != "B-CONTINUES" {
		t.Fatalf("peer Browser post-corruption action missing: %v %q", err, action)
	}
	// The original Executor broker calls this only after verified full
	// process/cgroup stop. A mode changed by the session itself must not
	// strand its slot, provided inode type and exact host ownership match.
	var executor Server
	for _, own := range []string{profiles[1], traces[1]} {
		info, err := os.Stat(own)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("session did not change its own scratch mode: %v %v", info, err)
		}
		if err := executor.browserReclaimScratchAt(scratch, browserSessionHostUIDBase+1, hostGID,
			filepath.Base(own), true); err != nil {
			t.Fatalf("verified root cleanup refused a session-owned mode change: %v", err)
		}
		if _, err := os.Lstat(own); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("exact retired Browser scratch still present: %v", err)
		}
	}
	fmt.Println("MAPPED_HOST_UID_PEER_INTEGRITY_OK")
}
