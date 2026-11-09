// Package cachemaintenance provides a deliberately narrow Go-first cache
// lifecycle. Managed per-command caches are separate from Go's shared cache.
// The ownership/lease helpers can serve another proven cache consumer later.
package cachemaintenance

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	cacheKind   = "go-build"
	leasePrefix = "go-run-"
	markerName  = ".agent-cache.json"
	lockName    = ".agent-cache.lock"
)

type marker struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Created int64  `json:"created_unix"`
}

type disk struct {
	Path           string `json:"path"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	UsedPercent    int    `json:"used_percent"`
}

type report struct {
	Operation            string `json:"operation"`
	Kind                 string `json:"kind"`
	Time                 string `json:"timestamp"`
	SharedCache          string `json:"shared_cache"`
	SharedBytes          int64  `json:"shared_bytes"`
	ManagedRoot          string `json:"managed_root"`
	ManagedBytes         int64  `json:"managed_bytes"`
	ManagedEntries       int    `json:"managed_entries"`
	ActiveEntries        int    `json:"active_entries"`
	InvalidEntries       int    `json:"invalid_entries"`
	RecentEntries        int    `json:"recent_entries,omitempty"`
	EligibleEntries      int    `json:"eligible_entries,omitempty"`
	DeletedEntries       int    `json:"deleted_entries,omitempty"`
	DeletedBytes         int64  `json:"deleted_bytes,omitempty"`
	ActualFreeDeltaBytes int64  `json:"actual_free_delta_bytes"`
	FreeDeltaMeasured    bool   `json:"free_delta_measured"`
	Error                string `json:"error,omitempty"`
	Pressure             bool   `json:"pressure"`
	Apply                bool   `json:"apply"`
	Disk                 disk   `json:"managed_filesystem"`
	SharedDisk           *disk  `json:"shared_filesystem,omitempty"`
}

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: ai-server-agent cache [status | run -- GO_ARGS... | prune [--apply] [--force] [--older-than=1h] [--max-used-percent=85] [--min-free-mib=2048]]")
		return 2
	}
	start := time.Now()
	switch args[0] {
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "cache status takes no arguments")
			return 2
		}
		r, err := inspect(start)
		if err != nil {
			fmt.Fprintln(stderr, "cache status:", err)
			return 1
		}
		r.Operation = "status"
		return emit(stdout, r)
	case "run":
		if os.Geteuid() == 0 {
			fmt.Fprintln(stderr, "cache run must execute as the worker, not root")
			return 1
		}
		argv := args[1:]
		if len(argv) > 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		if len(argv) == 0 {
			fmt.Fprintln(stderr, "usage: ai-server-agent cache run -- test ./...")
			return 2
		}
		code, err := runGo(argv, stdout, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "cache run:", err)
			return 1
		}
		return code
	case "prune":
		if os.Geteuid() == 0 {
			fmt.Fprintln(stderr, "cache prune must execute as the worker, not root")
			return 1
		}
		fs := flag.NewFlagSet("cache prune", flag.ContinueOnError)
		fs.SetOutput(stderr)
		apply := fs.Bool("apply", false, "actually delete eligible abandoned managed cache entries")
		force := fs.Bool("force", false, "ignore disk-pressure threshold, but not ownership, age or active leases")
		age := fs.Duration("older-than", time.Hour, "minimum age before an abandoned entry is eligible")
		maxUsed := fs.Int("max-used-percent", 85, "disk usage at which pruning becomes eligible")
		minFree := fs.Int64("min-free-mib", 2048, "minimum free space in MiB")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 || *age < 0 || *maxUsed < 1 || *maxUsed > 100 || *minFree < 0 || *minFree > 1<<30 {
			fmt.Fprintln(stderr, "invalid prune options")
			return 2
		}
		r, err := prune(start, *apply, *force, *age, *maxUsed, uint64(*minFree)*1024*1024)
		if err != nil {
			r.Error = err.Error()
		}
		if r.Operation == "prune" {
			if code := emit(stdout, r); code != 0 {
				fmt.Fprintln(stderr, "cache prune: could not write report")
				return code
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "cache prune:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(stderr, "unknown cache action:", args[0])
		return 2
	}
}

func emit(w io.Writer, r report) int {
	if err := json.NewEncoder(w).Encode(r); err != nil {
		return 1
	}
	return 0
}

func paths() (managed string, shared string, err error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	managed = filepath.Join(cacheDir, "ai-server-agent", "go-task-caches")
	// Query Go's *actual* configured shared cache, not an assumed mount.
	// Run outside a module, and prevent a status command downloading a toolchain.
	cmd := exec.Command("go", "env", "GOCACHE")
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	output, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("read effective Go GOCACHE: %w", err)
	}
	shared = strings.TrimSpace(string(output))
	if !filepath.IsAbs(shared) || shared == managed || strings.HasPrefix(shared, managed+string(os.PathSeparator)) {
		return "", "", errors.New("invalid effective Go cache path")
	}
	return managed, shared, nil
}

func managedPath() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ai-server-agent", "go-task-caches"), nil
}

func nearestExisting(path string) (string, error) {
	for {
		_, err := os.Stat(path)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", os.ErrNotExist
		}
		path = parent
	}
}

func diskAt(path string) (disk, error) {
	root, err := nearestExisting(path)
	if err != nil {
		return disk{}, err
	}
	var s syscall.Statfs_t
	if err := syscall.Statfs(root, &s); err != nil {
		return disk{}, err
	}
	if s.Blocks == 0 {
		return disk{}, errors.New("filesystem has no capacity data")
	}
	total := s.Blocks * uint64(s.Bsize)
	available := s.Bavail * uint64(s.Bsize)
	used := int(((total-available)*100 + total/2) / total)
	return disk{Path: root, TotalBytes: total, AvailableBytes: available, UsedPercent: used}, nil
}

func measure(path string) (int64, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("refusing symlink cache path %q", path)
	}
	var total int64
	err = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func verifyRoot(path string, create bool) error {
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !create {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("managed cache root is not a private non-symlink directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("managed cache root belongs to another user")
	}
	return nil
}

func list(root string) ([]os.DirEntry, error) {
	if err := verifyRoot(root, false); err != nil {
		return nil, err
	}
	items, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return items, err
}

// readOwnershipMarker never follows a symlink or waits for a FIFO. The
// descriptor checks defend against a file changing after the initial lstat,
// while the size-limited read protects against races and malformed data.
func readOwnershipMarker(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return nil, errors.New("ownership marker must be a small regular file")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return nil, errors.New("ownership marker changed or is oversized")
	}
	content, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil {
		return nil, err
	}
	if len(content) > 1024 {
		return nil, errors.New("ownership marker exceeds 1024 bytes")
	}
	return content, nil
}

// lockEntry never creates a missing lock. An unknown or modified directory
// is not owned by this lifecycle, so it can never become a prune candidate.
func lockEntry(dir, id string) (file *os.File, meta marker, active bool, err error) {
	di, err := os.Lstat(dir)
	if err != nil || !di.IsDir() || di.Mode().Perm()&0077 != 0 {
		return nil, marker{}, false, errors.New("unexpected entry directory")
	}
	b, err := readOwnershipMarker(filepath.Join(dir, markerName))
	if err != nil {
		return nil, marker{}, false, fmt.Errorf("invalid cache ownership marker: %w", err)
	}
	if err := json.Unmarshal(b, &meta); err != nil || meta.Version != 1 || meta.Kind != cacheKind || meta.ID != id || meta.Created <= 0 {
		return nil, marker{}, false, errors.New("cache ownership marker mismatch")
	}
	lockPath := filepath.Join(dir, lockName)
	li, err := os.Lstat(lockPath)
	if err != nil || !li.Mode().IsRegular() || li.Mode().Perm()&0077 != 0 {
		return nil, marker{}, false, errors.New("missing or invalid cache lease")
	}
	file, err = os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return nil, marker{}, false, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, meta, true, nil
		}
		return nil, marker{}, false, err
	}
	return file, meta, false, nil
}

func release(f *os.File) {
	if f != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func inspect(now time.Time) (report, error) {
	managed, shared, err := paths()
	if err != nil {
		return report{}, err
	}
	r := report{Operation: "status", Kind: cacheKind, Time: now.UTC().Format(time.RFC3339), SharedCache: shared, ManagedRoot: managed}
	// A Go user may deliberately keep the shared cache behind a symlink.
	// Status may follow that read-only mapping; deletion is strictly limited
	// to the private Agent-owned root below.
	sharedMeasured := shared
	if info, lookupErr := os.Lstat(shared); lookupErr == nil && info.Mode()&os.ModeSymlink != 0 {
		sharedMeasured, err = filepath.EvalSymlinks(shared)
		if err != nil {
			return r, err
		}
	}
	r.SharedBytes, err = measure(sharedMeasured)
	if err != nil {
		return r, err
	}
	r.Disk, err = diskAt(managed)
	if err != nil {
		return r, err
	}
	sharedDisk, err := diskAt(sharedMeasured)
	if err != nil {
		return r, err
	}
	r.SharedDisk = &sharedDisk
	r.Pressure = pressure(r.Disk, 85, 2048*1024*1024)
	items, err := list(managed)
	if err != nil {
		return r, err
	}
	for _, item := range items {
		if !strings.HasPrefix(item.Name(), leasePrefix) {
			continue
		}
		r.ManagedEntries++
		entry := filepath.Join(managed, item.Name())
		f, _, active, err := lockEntry(entry, item.Name())
		if err != nil {
			r.InvalidEntries++
			continue
		}
		if active {
			r.ActiveEntries++
		}
		size, err := measure(entry)
		if err != nil {
			r.InvalidEntries++
		} else {
			r.ManagedBytes += size
		}
		release(f)
	}
	return r, nil
}

func pressure(d disk, maxUsed int, minFree uint64) bool {
	return d.UsedPercent >= maxUsed || d.AvailableBytes < minFree
}

// freeBytesDelta compares the two snapshots taken inside one prune attempt.
// A later filesystem observation is not equivalent on shared CI/host storage.
func freeBytesDelta(before, after disk) int64 {
	return int64(after.AvailableBytes) - int64(before.AvailableBytes)
}

func prune(now time.Time, apply, force bool, minAge time.Duration, maxUsed int, minFree uint64) (r report, resultErr error) {
	root, err := managedPath()
	if err != nil {
		return report{}, err
	}
	r = report{Operation: "prune", Kind: cacheKind, Time: now.UTC().Format(time.RFC3339), ManagedRoot: root, Apply: apply}
	r.Disk, err = diskAt(root)
	if err != nil {
		return r, err
	}
	// Removal can fail after a previous entry or part of the current entry
	// was deleted. Measure on every path that attempted a destructive change,
	// including failures, so the operator sees partial progress.
	removalAttempted := false
	defer func() {
		if !removalAttempted {
			return
		}
		after, err := diskAt(root)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("measure disk after cache prune: %w", err))
			return
		}
		r.ActualFreeDeltaBytes = freeBytesDelta(r.Disk, after)
		r.FreeDeltaMeasured = true
	}()
	r.Pressure = pressure(r.Disk, maxUsed, minFree)
	r.ManagedBytes, err = measure(root)
	if err != nil {
		return r, err
	}
	items, err := list(root)
	if err != nil {
		return r, err
	}
	for _, item := range items {
		if !strings.HasPrefix(item.Name(), leasePrefix) {
			continue
		}
		r.ManagedEntries++
		entry := filepath.Join(root, item.Name())
		f, meta, active, err := lockEntry(entry, item.Name())
		if err != nil {
			r.InvalidEntries++
			continue
		}
		if active {
			r.ActiveEntries++
			continue
		}
		if now.Sub(time.Unix(meta.Created, 0)) < minAge {
			r.RecentEntries++
			release(f)
			continue
		}
		size, err := measure(entry)
		if err != nil {
			r.InvalidEntries++
			release(f)
			continue
		}
		if !force && !r.Pressure {
			release(f)
			continue
		}
		r.EligibleEntries++
		if apply {
			removalAttempted = true
			// The lock remains held during the entire deletion, preventing
			// overlap with another cooperative command's use of this lease.
			if err := os.RemoveAll(entry); err != nil {
				release(f)
				return r, fmt.Errorf("remove owned cache entry %q: %w", item.Name(), err)
			}
			r.DeletedEntries++
			r.DeletedBytes += size
		}
		release(f)
	}
	return r, nil
}

func runGo(argv []string, stdout, stderr io.Writer) (exitCode int, runErr error) {
	root, err := managedPath()
	if err != nil {
		return 1, err
	}
	if err := verifyRoot(root, true); err != nil {
		return 1, err
	}
	entry, err := os.MkdirTemp(root, leasePrefix)
	if err != nil {
		return 1, err
	}
	var lock *os.File
	defer func() {
		// Keep the lease held through complete removal. A failed cleanup
		// must be observable, not silently reported as a successful lifecycle.
		if cleanupErr := os.RemoveAll(entry); cleanupErr != nil {
			fmt.Fprintln(stderr, "cache run: isolated cache cleanup failed:", cleanupErr)
			if exitCode == 0 && runErr == nil {
				runErr = cleanupErr
			}
		}
		if lock != nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		}
	}()
	id := filepath.Base(entry)
	if err := os.Mkdir(filepath.Join(entry, "data"), 0700); err != nil {
		return 1, err
	}
	lock, err = os.OpenFile(filepath.Join(entry, lockName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 1, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 1, err
	}
	b, err := json.Marshal(marker{Version: 1, Kind: cacheKind, ID: id, Created: time.Now().Unix()})
	if err != nil {
		return 1, err
	}
	if err := os.WriteFile(filepath.Join(entry, markerName), b, 0600); err != nil {
		return 1, err
	}
	goCmd, err := exec.LookPath("go")
	if err != nil {
		return 1, fmt.Errorf("Go executable unavailable: %w", err)
	}
	cmd := exec.Command(goCmd, argv...)
	// The child holds the same flock open-file description as the wrapper.
	// If the wrapper is killed, a still-running Go process keeps the
	// abandoned task cache protected until it exits.
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(entry, "data"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}
