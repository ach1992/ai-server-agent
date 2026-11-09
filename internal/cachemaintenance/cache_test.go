package cachemaintenance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root, err := managedPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func testEntry(t *testing.T, root, id string, created time.Time) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "artifact"), bytes.Repeat([]byte{'a'}, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(marker{Version: 1, Kind: cacheKind, ID: id, Created: created.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, markerName), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockName), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPruneOnlyOldIdleOwnedCache(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	old := testEntry(t, root, "go-run-old", now.Add(-48*time.Hour))
	recent := testEntry(t, root, "go-run-recent", now)
	active := testEntry(t, root, "go-run-active", now.Add(-48*time.Hour))
	f, err := os.OpenFile(filepath.Join(active, lockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	dry, err := prune(now, false, true, time.Hour, 85, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dry.EligibleEntries != 1 || dry.ActiveEntries != 1 || dry.RecentEntries != 1 || dry.DeletedEntries != 0 {
		t.Fatalf("unexpected dry-run %+v", dry)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("dry-run removed old cache: %v", err)
	}

	// No pressure and no force: never delete idle caches.
	low, err := prune(now, true, false, time.Hour, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if low.Pressure || low.DeletedEntries != 0 {
		t.Fatalf("unexpected low-pressure deletion: %+v", low)
	}
	applied, err := prune(now, true, true, time.Hour, 85, 0)
	if err != nil {
		t.Fatal(err)
	}
	if applied.DeletedEntries != 1 || applied.DeletedBytes < 4096 {
		t.Fatalf("unexpected deletion metrics %+v", applied)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old entry remains: %v", err)
	}
	for _, p := range []string{active, recent} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("active/recent cache removed: %s: %v", p, err)
		}
	}
}

func TestPruneOnlyUnderPressureWhenNotForced(t *testing.T) {
	root := testRoot(t)
	old := testEntry(t, root, "go-run-pressure", time.Now().Add(-2*time.Hour))
	dry, err := prune(time.Now(), false, false, time.Hour, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.Pressure || dry.EligibleEntries != 1 {
		t.Fatalf("pressure did not select owned cache: %+v", dry)
	}
	real, err := prune(time.Now(), true, false, time.Hour, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if real.DeletedEntries != 1 {
		t.Fatalf("eligible cache not pruned: %+v", real)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("cache remains: %v", err)
	}
}

func TestPruneRejectsUnknownEntriesAndSymlinks(t *testing.T) {
	root := testRoot(t)
	outside := t.TempDir()
	external := filepath.Join(outside, "important")
	if err := os.WriteFile(external, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "go-run-foreign-link")); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, "go-run-unowned")
	if err := os.Mkdir(unknown, 0700); err != nil {
		t.Fatal(err)
	}
	damaged := testEntry(t, root, "go-run-mismatch", time.Now().Add(-48*time.Hour))
	if err := os.WriteFile(filepath.Join(damaged, markerName), []byte(`{"version":1,"kind":"wrong","id":"go-run-mismatch","created_unix":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := prune(time.Now(), true, true, 0, 85, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.DeletedEntries != 0 || r.InvalidEntries != 3 {
		t.Fatalf("unknown entries were not rejected: %+v", r)
	}
	if b, err := os.ReadFile(external); err != nil || string(b) != "keep" {
		t.Fatalf("external data changed: %q %v", b, err)
	}
}

func TestPruneRefusesSymlinkRoot(t *testing.T) {
	root := testRoot(t)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if _, err := prune(time.Now(), true, true, 0, 85, 0); err == nil {
		t.Fatal("symlinked root accepted")
	}
}

func TestPruneBadRootPermissions(t *testing.T) {
	root := testRoot(t)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := prune(time.Now(), true, true, 0, 85, 0); err == nil {
		t.Fatal("world-readable root accepted")
	}
}

func TestRunGoIsolatedCacheCleanupAndExitCode(t *testing.T) {
	root := testRoot(t)
	fake := t.TempDir()
	logPath := filepath.Join(fake, "received")
	script := "#!/bin/sh\nprintf '%s\\n' \"$GOCACHE\" \"$*\" > \"$CACHE_TEST_OUT\"\nexit \"$CACHE_TEST_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(fake, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+":"+os.Getenv("PATH"))
	t.Setenv("CACHE_TEST_OUT", logPath)
	t.Setenv("CACHE_TEST_EXIT", "9")
	var out, errors bytes.Buffer
	code, err := runGo([]string{"test", "./..."}, &out, &errors)
	if err != nil || code != 9 {
		t.Fatalf("nonzero exit incorrectly handled: code=%d err=%v", code, err)
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 || lines[1] != "test ./..." || !strings.HasPrefix(lines[0], root+string(os.PathSeparator)) || !strings.HasSuffix(lines[0], "/data") {
		t.Fatalf("wrong isolated Go invocation: %q", content)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("task cache leaked after command exit: %v, %v", entries, err)
	}
}

func TestRunGoHoldsActiveLeaseAgainstPrune(t *testing.T) {
	root := testRoot(t)
	fake := t.TempDir()
	releaseFile := filepath.Join(fake, "release")
	script := fmt.Sprintf("#!/bin/sh\nwhile [ ! -e %q ]; do sleep 0.02; done\n", releaseFile)
	if err := os.WriteFile(filepath.Join(fake, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+":"+os.Getenv("PATH"))
	done := make(chan error, 1)
	go func() {
		_, err := runGo([]string{"version"}, ioDiscard{}, ioDiscard{})
		done <- err
	}()
	var entry string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		items, err := os.ReadDir(root)
		if err == nil && len(items) == 1 {
			entry = filepath.Join(root, items[0].Name())
			if _, err := os.Stat(filepath.Join(entry, markerName)); err == nil {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if entry == "" {
		t.Fatal("managed Go job did not create a cache")
	}
	r, err := prune(time.Now().Add(2*time.Hour), true, true, 0, 85, 0)
	if err != nil || r.ActiveEntries != 1 || r.DeletedEntries != 0 {
		t.Fatalf("active cache was unsafe to prune: %+v, %v", r, err)
	}
	if err := os.WriteFile(releaseFile, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not exit")
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("cache still present after normal process cleanup: %v", err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(b []byte) (int, error) { return len(b), nil }

func TestPressureInputsAndNoMountedCache(t *testing.T) {
	if pressure(disk{UsedPercent: 20, AvailableBytes: 1 << 40}, 85, 2<<30) {
		t.Fatal("low pressure incorrectly selected")
	}
	if !pressure(disk{UsedPercent: 86, AvailableBytes: 1 << 40}, 85, 2<<30) {
		t.Fatal("high usage not selected")
	}
	if !pressure(disk{UsedPercent: 20, AvailableBytes: 1 << 30}, 85, 2<<30) {
		t.Fatal("low free bytes not selected")
	}
	root := testRoot(t)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	r, err := prune(time.Now(), false, false, time.Hour, 85, 2<<30)
	if err != nil || r.ManagedEntries != 0 {
		t.Fatalf("missing managed cache did not safely noop: %+v %v", r, err)
	}
}

func TestStatusUsesActualGoCacheAndReportsDisk(t *testing.T) {
	root := testRoot(t)
	shared := filepath.Join(t.TempDir(), "custom-go-cache")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "artifact"), []byte("reported-cache"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1 $2\" = 'env GOCACHE' ]; then printf '%%s\\n' %q; exit 0; fi\nexit 2\n", shared)
	if err := os.WriteFile(filepath.Join(fake, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+":"+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status error %d: %s", code, stderr.String())
	}
	var r report
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.SharedCache != shared || r.SharedBytes != int64(len("reported-cache")) || r.ManagedRoot != root || r.Disk.TotalBytes == 0 || r.SharedDisk == nil || r.SharedDisk.TotalBytes == 0 {
		t.Fatalf("incorrect status JSON: %+v", r)
	}
	// Read-only status does not create an isolated task-cache entry.
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("status changed managed cache: %v %v", entries, err)
	}
}

func TestOwnedCacheWithSymlinkInsideNeverFollowsOutsideTarget(t *testing.T) {
	root := testRoot(t)
	entry := testEntry(t, root, "go-run-safe-symlink", time.Now().Add(-24*time.Hour))
	outside := t.TempDir()
	proof := filepath.Join(outside, "protected")
	if err := os.WriteFile(proof, []byte("no-delete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(entry, "data", "external")); err != nil {
		t.Fatal(err)
	}
	r, err := prune(time.Now(), true, true, time.Hour, 85, 0)
	if err != nil || r.DeletedEntries != 1 {
		t.Fatalf("owned symlink cache cleanup failed: %+v %v", r, err)
	}
	if content, err := os.ReadFile(proof); err != nil || string(content) != "no-delete" {
		t.Fatalf("symlink target was affected: %q %v", content, err)
	}
}

func TestPruneCLIRejectsInvalidThresholds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cache prune CLI is worker-only")
	}
	for _, args := range [][]string{
		{"prune", "--max-used-percent=0"},
		{"prune", "--max-used-percent=101"},
		{"prune", "--min-free-mib=-1"},
		{"prune", "--older-than=-1h"},
		{"prune", "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("invalid options %v accepted: code=%d output=%q", args, code, stdout.String())
		}
	}
}

// A killed wrapper must not make its still-running Go subprocess appear
// abandoned. The subprocess retains an inherited lease FD until it exits.
func TestRunGoInterruptedWrapperPreservesChildLease(t *testing.T) {
	root := testRoot(t)
	fake := t.TempDir()
	ready := filepath.Join(fake, "ready")
	releasePath := filepath.Join(fake, "release")
	script := "#!/bin/sh\ntest -e /proc/self/fd/3 || exit 91\n: > \"$CACHE_CHILD_READY\"\nwhile [ ! -e \"$CACHE_CHILD_RELEASE\" ]; do sleep 0.02; done\n"
	if err := os.WriteFile(filepath.Join(fake, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestRunGoWrapperHelper$")
	command.Env = append(os.Environ(),
		"CACHE_WRAPPER_HELPER=1",
		"CACHE_CHILD_READY="+ready,
		"CACHE_CHILD_RELEASE="+releasePath,
		"XDG_CACHE_HOME="+os.Getenv("XDG_CACHE_HOME"),
		"PATH="+fake+":"+os.Getenv("PATH"),
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(releasePath, nil, 0600)
		_ = command.Process.Kill()
		_ = command.Wait()
	}()

	var entry string
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(ready); err != nil {
			continue
		}
		items, err := os.ReadDir(root)
		if err == nil && len(items) == 1 {
			entry = filepath.Join(root, items[0].Name())
			break
		}
	}
	if entry == "" {
		t.Fatal("helper Go process did not start with private cache lease")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	applied, err := prune(time.Now().Add(2*time.Hour), true, true, 0, 85, 0)
	if err != nil || applied.ActiveEntries != 1 || applied.DeletedEntries != 0 {
		t.Fatalf("orphan Go process lost lease after wrapper kill: %+v %v", applied, err)
	}
	if err := os.WriteFile(releasePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// After the Go child exits, the exact previously owned cache can be reclaimed.
	var recovered bool
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		p, err := prune(time.Now().Add(2*time.Hour), true, true, 0, 85, 0)
		if err != nil {
			t.Fatal(err)
		}
		if p.DeletedEntries == 1 {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatal("orphaned task cache remained locked after child exit")
	}
}

func TestRunGoWrapperHelper(t *testing.T) {
	if os.Getenv("CACHE_WRAPPER_HELPER") != "1" {
		return
	}
	_, err := runGo([]string{"version"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatal(err)
	}
}

// Only verified, bounded regular files can identify Agent-owned cache entries.
// A symlink to a stream or a FIFO must never block cache status/prune.
func TestMarkerReadRejectsSymlinkFIFOAndOversize(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	valid := testEntry(t, root, "go-run-valid-marker", now.Add(-2*time.Hour))
	symlinked := testEntry(t, root, "go-run-symlink-marker", now.Add(-2*time.Hour))
	fifo := testEntry(t, root, "go-run-fifo-marker", now.Add(-2*time.Hour))
	oversize := testEntry(t, root, "go-run-oversize-marker", now.Add(-2*time.Hour))
	if err := os.Remove(filepath.Join(symlinked, markerName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(symlinked, markerName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fifo, markerName)); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fifo, markerName), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oversize, markerName), bytes.Repeat([]byte{'x'}, 1025), 0600); err != nil {
		t.Fatal(err)
	}
	validData, err := readOwnershipMarker(filepath.Join(valid, markerName))
	if err != nil {
		t.Fatal(err)
	}
	var owned marker
	if err := json.Unmarshal(validData, &owned); err != nil || owned.Kind != cacheKind || owned.ID != "go-run-valid-marker" {
		t.Fatalf("valid owner marker changed: %+v %v", owned, err)
	}
	for _, entry := range []string{symlinked, fifo, oversize} {
		if _, err := readOwnershipMarker(filepath.Join(entry, markerName)); err == nil {
			t.Errorf("unsafe marker accepted at %s", entry)
		}
	}
	result, err := prune(now, false, true, time.Hour, 85, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ManagedEntries != 4 || result.InvalidEntries != 3 || result.EligibleEntries != 1 || result.DeletedEntries != 0 {
		t.Fatalf("marker checks failed: %+v", result)
	}
}

func TestFreeBytesDeltaFromSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after uint64
		want          int64
	}{
		{name: "freed", before: 10000, after: 18192, want: 8192},
		{name: "unchanged", before: 10000, after: 10000, want: 0},
		{name: "occupied", before: 18192, after: 10000, want: -8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := freeBytesDelta(disk{AvailableBytes: tc.before}, disk{AvailableBytes: tc.after})
			if got != tc.want {
				t.Fatalf("free-byte delta: got %d want %d", got, tc.want)
			}
		})
	}
}

func TestPrunePartialFailureReturnsStructuredReport(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied filesystem fixture requires an unprivileged worker")
	}
	root := testRoot(t)
	now := time.Now()
	first := testEntry(t, root, "go-run-a-first", now.Add(-48*time.Hour))
	second := testEntry(t, root, "go-run-b-second", now.Add(-48*time.Hour))
	privateData := filepath.Join(second, "data")
	if err := os.Chmod(privateData, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(privateData, 0700)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"prune", "--apply", "--force", "--older-than=0"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("partial delete failure falsely succeeded")
	}
	var result report
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("missing structured error report: %s; error: %v", stdout.String(), err)
	}
	if result.Operation != "prune" || result.DeletedEntries != 1 || result.DeletedBytes < 4096 || !result.FreeDeltaMeasured || result.Error == "" {
		t.Fatalf("lost partial cleanup evidence: %+v stderr=%s", result, stderr.String())
	}
	if !strings.Contains(result.Error, "go-run-b-second") || !strings.Contains(stderr.String(), "go-run-b-second") {
		t.Fatalf("failure lacks the affected entry: %+v stderr=%s", result, stderr.String())
	}
	// The report already includes the after-prune snapshot. A new diskAt
	// call can differ as unrelated work changes free space on shared CI disks.
	// FreeDeltaMeasured proves the partial-failure measurement path; the
	// arithmetic is verified by TestFreeBytesDeltaFromSnapshots.
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("first completed removal missing: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("failed candidate unexpectedly removed: %v", err)
	}
}
