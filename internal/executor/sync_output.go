package executor

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var errSyncOutputLimit = errors.New("synchronous output exceeds configured retention limit")

// syncOutputCapture retains a bounded in-response preview while keeping the
// complete stream recoverable through job_output. Output is spooled only when
// it exceeds the response preview or text-only MCP fallback budget.
type syncOutputCapture struct {
	mu         sync.Mutex
	server     *Server
	preview    *boundedOutputCollector
	buffer     []byte
	file       *os.File
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

func (w *syncOutputCapture) spill() error {
	jobsDir, claimsDir, err := w.server.ensureJobState()
	if err != nil {
		return err
	}
	// Preserve the existing audit/persistent-job safety envelope in addition
	// to the configured maximum transcript. Disk pressure is an explicit
	// failure, never permission to consume audit space.
	reserve := jobAuditSafetyReserveAtStartup(w.server.cfg) + w.max
	if safe, available, err := jobStateHasReserve(jobsDir, reserve); err != nil {
		return fmt.Errorf("check output spool disk reserve: %w", err)
	} else if !safe {
		return fmt.Errorf("output spool disk capacity is insufficient: %d bytes available; %d-byte reserve required", available, reserve)
	}
	// Reuse the Job completed-artifact retention lifecycle. Never stall
	// another accepted command while job admission owns the state lock.
	if w.server.jobsMu.TryLock() {
		_ = w.server.cleanupCompletedJobArtifacts(jobsDir, claimsDir)
		w.server.jobsMu.Unlock()
	}
	for n := 0; n < 3; n++ {
		nonce, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return err
		}
		id := nonce.String() // existing job_output IDs are decimal
		paths := jobPathsFor(jobsDir, id)
		if err := createJobFile(paths.log, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return err
		}
		if err := createJobFile(paths.status, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
			_ = os.Remove(paths.log)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return err
		}
		f, err := os.OpenFile(paths.log, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
		if err == nil {
			err = writeComplete(f, w.buffer)
		}
		if err != nil {
			if f != nil {
				_ = f.Close()
			}
			_ = os.Remove(paths.log)
			_ = os.Remove(paths.status)
			return err
		}
		w.file = f
		w.jobID = id
		w.statusPath = paths.status
		w.buffer = nil
		return nil
	}
	return errors.New("could not allocate unique output artifact")
}

// Finish is called after the process has exited/been stopped. Do not
// advertise a recovery ID until the complete output is durable.
func (w *syncOutputCapture) Finish(resp *Response, maxTextFallbackBytes int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil && w.failure == nil && len(w.buffer) > 0 {
		// The MCP text fallback uses indented JSON and adds request metadata
		// after Executor completion. Keep a small envelope margin so a
		// borderline response does not silently lose its continuation.
		wire, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			w.failure = err
		} else if len(wire)+2048 > maxTextFallbackBytes {
			w.failure = w.spill()
		}
	}
	if w.file != nil {
		err := w.file.Sync()
		err = errors.Join(err, w.file.Close())
		w.file = nil
		if err == nil {
			f, openErr := os.OpenFile(w.statusPath, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
			if openErr != nil {
				err = openErr
			} else {
				_, writeErr := fmt.Fprintln(f, strconv.Itoa(resp.ExitCode))
				err = errors.Join(writeErr, f.Sync(), f.Close())
			}
		}
		if err != nil || w.failure != nil {
			w.failure = errors.Join(w.failure, err)
			// A partial transcript must never be returned as a complete job.
			_ = os.Remove(strings.TrimSuffix(w.statusPath, ".status") + ".log")
			_ = os.Remove(w.statusPath)
		} else {
			resp.JobID = w.jobID
		}
	}
	return w.failure
}
