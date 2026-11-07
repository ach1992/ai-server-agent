package executor

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var errSyncOutputLimit = errors.New("synchronous output exceeds configured retention limit")

const syncSpoolMarkerSuffix = ".sync-spool"

// syncOutputCapture keeps the bounded in-response preview and preserves the
// exact output stream for existing job_output offset reads. A private marker
// holds an exclusive flock for the lifetime of each in-flight transcript:
// after an Executor crash the marker survives but its lock does not.
type syncOutputCapture struct {
	mu         sync.Mutex
	server     *Server
	preview    *boundedOutputCollector
	buffer     []byte
	file       *os.File
	marker     *os.File
	markerPath string
	statusPath string
	jobID      string
	seen       int64
	max        int64
	failure    error
}

func newSyncOutputCapture(s *Server) *syncOutputCapture {
	return &syncOutputCapture{
		server: s, preview: newBoundedOutputCollector(maxSyncOutputBytes),
		max: int64(s.cfg.EffectiveRuntime().SyncOutputSpoolMiB) << 20,
	}
}

func (w *syncOutputCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failure != nil {
		return 0, w.failure
	}
	if int64(len(p)) > w.max-w.seen {
		w.failure = fmt.Errorf("%w: maximum %d MiB (runtime.sync_output_spool_mib)", errSyncOutputLimit, w.max>>20)
		return 0, w.failure
	}
	if w.file == nil && len(w.buffer)+len(p) > maxSyncOutputBytes {
		if err := w.spill(); err != nil {
			w.failure = fmt.Errorf("preserve command output: %w", err)
			return 0, w.failure
		}
	}
	if w.file == nil {
		w.buffer = append(w.buffer, p...)
	} else if err := writeComplete(w.file, p); err != nil {
		w.failure = fmt.Errorf("write retained command output: %w", err)
		return 0, w.failure
	}
	w.seen += int64(len(p))
	_, _ = w.preview.Write(p)
	return len(p), nil
}

func writeComplete(f *os.File, p []byte) error {
	n, err := f.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// spill runs only while the capture mutex is held. The short admission mutex
// is shared with persistent-Job disk checks, not with command execution.
// An actual fallocate reservation consumes available filesystem blocks; the
// next concurrent admission sees the reduced free capacity. Thus the audit
// and persistent-Job reserve cannot be double-booked by concurrent spools.
func (w *syncOutputCapture) spill() error {
	jobsDir, claimsDir, err := w.server.ensureJobState()
	if err != nil {
		return err
	}
	if w.server.jobsMu.TryLock() {
		_ = w.server.cleanupCompletedJobArtifacts(jobsDir, claimsDir)
		w.server.jobsMu.Unlock()
	}
	w.server.syncSpoolMu.Lock()
	defer w.server.syncSpoolMu.Unlock()

	reserve := jobAuditSafetyReserveAtStartup(w.server.cfg) + w.max
	checkReserve := jobStateHasReserve
	if w.server.syncSpoolReserveCheckTestHook != nil {
		checkReserve = w.server.syncSpoolReserveCheckTestHook
	}
	if safe, free, err := checkReserve(jobsDir, reserve); err != nil {
		return fmt.Errorf("check output spool disk reserve: %w", err)
	} else if !safe {
		return fmt.Errorf("output spool disk capacity is insufficient: %d bytes available; %d-byte reserve required", free, reserve)
	}

	for n := 0; n < 3; n++ {
		nonce, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return err
		}
		id := nonce.String() // existing job_output IDs are decimal
		paths := jobPathsFor(jobsDir, id)
		markerPath := filepath.Join(jobsDir, id+syncSpoolMarkerSuffix)
		marker, err := os.OpenFile(markerPath, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		logCreated, statusCreated := false, false
		var f *os.File
		fail := func(cause error) error {
			if f != nil {
				cause = errors.Join(cause, f.Close())
			}
			if statusCreated {
				cause = errors.Join(cause, removeIfExists(paths.status))
			}
			if logCreated {
				cause = errors.Join(cause, removeIfExists(paths.log))
			}
			cause = errors.Join(cause, marker.Close(), removeIfExists(markerPath))
			return cause
		}
		if err := syscall.Flock(int(marker.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fail(err)
		}
		if err := createJobFile(paths.log, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
			if errors.Is(err, os.ErrExist) {
				_ = fail(nil)
				continue
			}
			return fail(err)
		}
		logCreated = true
		if err := createJobFile(paths.status, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
			if errors.Is(err, os.ErrExist) {
				_ = fail(nil)
				continue
			}
			return fail(err)
		}
		statusCreated = true
		f, err = os.OpenFile(paths.log, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fail(err)
		}
		// FALLOC_FL_KEEP_SIZE would leave invisible reserved extents after
		// completion. Ordinary fallocate plus Truncate(actual bytes) releases
		// all unused blocks, including on cancellation and error cleanup.
		if err := unix.Fallocate(int(f.Fd()), 0, 0, w.max); err != nil {
			return fail(fmt.Errorf("reserve %d bytes of transcript space: %w", w.max, err))
		}
		if err := writeComplete(f, w.buffer); err != nil {
			return fail(err)
		}
		w.file = f
		w.marker = marker
		w.markerPath = markerPath
		w.jobID = id
		w.statusPath = paths.status
		w.buffer = nil
		return nil
	}
	return errors.New("could not allocate unique output artifact")
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// abortSyncSpool never advertises partial output and returns the reserved
// blocks to the filesystem. The marker is removed while its lock is held.
func (w *syncOutputCapture) abortSyncSpool(cause error) error {
	if w.file != nil {
		cause = errors.Join(cause, w.file.Close())
		w.file = nil
	}
	if w.statusPath != "" {
		cause = errors.Join(cause, removeIfExists(w.statusPath))
		cause = errors.Join(cause, removeIfExists(strings.TrimSuffix(w.statusPath, ".status")+".log"))
	}
	if w.markerPath != "" {
		cause = errors.Join(cause, removeIfExists(w.markerPath))
	}
	if w.marker != nil {
		cause = errors.Join(cause, w.marker.Close())
		w.marker = nil
	}
	return cause
}

// Finish is called after the process has exited or has been stopped. Only
// after truncate/fsync/close and a durable numeric status may it advertise
// the existing job_output recovery ID.
func (w *syncOutputCapture) Finish(resp *Response, maxTextFallbackBytes int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil && w.failure == nil && len(w.buffer) > 0 {
		wire, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			w.failure = err
		} else if len(wire)+2048 > maxTextFallbackBytes {
			w.failure = w.spill()
		}
	}
	if w.file == nil {
		return w.failure
	}
	if w.failure != nil {
		w.failure = w.abortSyncSpool(w.failure)
		return w.failure
	}
	if err := w.file.Truncate(w.seen); err != nil {
		w.failure = w.abortSyncSpool(err)
		return w.failure
	}
	if err := w.file.Sync(); err != nil {
		w.failure = w.abortSyncSpool(err)
		return w.failure
	}
	if err := w.file.Close(); err != nil {
		w.file = nil
		w.failure = w.abortSyncSpool(err)
		return w.failure
	}
	w.file = nil
	if err := writeJobStatus(w.statusPath, resp.ExitCode); err != nil {
		w.failure = w.abortSyncSpool(err)
		return w.failure
	}
	if err := removeIfExists(w.markerPath); err != nil {
		w.failure = w.abortSyncSpool(err)
		return w.failure
	}
	if err := w.marker.Close(); err != nil {
		w.marker = nil
		w.failure = errors.Join(w.failure, err)
		return w.failure
	}
	w.marker = nil
	resp.JobID = w.jobID
	return nil
}

// cleanupInterruptedSyncSpools is safe both on Executor startup and during
// normal Job retention. It never retires an active capture: flock remains held
// for the entire lifetime of the owning process. Crash-left partial output is
// discarded; a fully committed numeric status retains its completed Job.
func (s *Server) cleanupInterruptedSyncSpools(jobsDir string) error {
	s.syncSpoolMu.Lock()
	defer s.syncSpoolMu.Unlock()
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), syncSpoolMarkerSuffix) {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), syncSpoolMarkerSuffix)
		if _, err := safeID(id); err != nil {
			continue
		}
		markerPath := filepath.Join(jobsDir, entry.Name())
		marker, err := s.openJobFile(markerPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := syscall.Flock(int(marker.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = marker.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			return err
		}
		paths := jobPathsFor(jobsDir, id)
		status, _, err := s.readJobStatusValue(paths.status)
		if err != nil && !os.IsNotExist(err) {
			_ = marker.Close()
			return err
		}
		complete := false
		if status != "" && status != jobStatusUnknown && status != jobStatusStopped {
			if _, parseErr := strconv.Atoi(status); parseErr == nil {
				if f, openErr := s.openJobFile(paths.log); openErr == nil {
					complete = true
					_ = f.Close()
				}
			}
		}
		if !complete {
			if err := removeJobPaths(paths); err != nil {
				_ = marker.Close()
				return err
			}
		}
		if err := removeIfExists(markerPath); err != nil {
			_ = marker.Close()
			return err
		}
		if err := marker.Close(); err != nil {
			return err
		}
	}
	return nil
}
