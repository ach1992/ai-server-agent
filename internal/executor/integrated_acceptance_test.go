package executor

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

// This isolated acceptance fixture needs the executor's real privilege boundary.
// It never starts/stops systemd units or uses an installed Agent socket/state.
func TestIntegratedExecutorCapacityAndDataPaths(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("isolated executor acceptance requires root to switch to aiworker")
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Skip("isolated executor acceptance requires the existing aiworker account")
	}
	uid, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil || uid == 0 {
		t.Fatalf("invalid unprivileged worker identity: uid=%d gid=%q err=%v", uid, worker.Gid, err)
	}
	fixture, err := os.MkdirTemp("", "aisa-load-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fixture)
	if err := os.Chmod(fixture, 0711); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(fixture, "workspace")
	state := filepath.Join(fixture, "state")
	jobs := filepath.Join(state, "jobs")
	for _, dir := range []string{workspace, state, jobs} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(workspace, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:       config.Config{WorkspaceDir: workspace, StateDir: state},
		token:     "isolated-acceptance-token",
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(fixture, "audit.jsonl")),
		workerUID: uint32(uid),
		workerGID: uint32(gid),
		runs:      newRunLimiter(),
	}
	socket := filepath.Join(fixture, "executor.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var handlers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() { defer handlers.Done(); s.handle(c) }()
		}
	}()
	defer func() { _ = ln.Close(); <-accepted; handlers.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		kind string
		resp Response
		err  error
		wait time.Duration
	}
	call := func(req Request, kind string) result {
		started := time.Now()
		resp, err := ClientCallContext(ctx, socket, s.token, req)
		return result{kind: kind, resp: resp, err: err, wait: time.Since(started)}
	}
	release := filepath.Join(workspace, "release")
	defer os.WriteFile(release, nil, 0600)
	active := cap(s.runs.worker) + cap(s.runs.root)
	completed := make(chan result, active)
	const noisyBytes = 8 * maxSyncOutputBytes
	for i := 0; i < active; i++ {
		ready := filepath.Join(workspace, fmt.Sprintf("ready-%d", i))
		command := fmt.Sprintf("set -e; printf HEAD; head -c %d /dev/zero | tr '\\000' x; printf TAIL; : > %s; while [ ! -e %s ]; do sleep 0.01; done", noisyBytes, shellQuote(ready), shellQuote(release))
		req := Request{Action: "run", Command: command, Root: i >= cap(s.runs.worker), TimeoutMS: 25000}
		go func() { completed <- call(req, "active") }()
	}
	for i := 0; i < active; i++ {
		ready := filepath.Join(workspace, fmt.Sprintf("ready-%d", i))
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			select {
			case failed := <-completed:
				t.Fatalf("command finished before occupancy barrier: error=%v code=%s output=%q", failed.err, failed.resp.ErrorCode, failed.resp.Output)
			case <-ctx.Done():
				t.Fatal("commands did not reach occupancy barrier")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	filePath := filepath.Join(fixture, "large.bin")
	file, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}
	const fileBytes = 64 << 20
	if err := file.Truncate(fileBytes); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(jobs, "123.log")
	if err := os.WriteFile(logPath, nil, 0640); err != nil {
		t.Fatal(err)
	}
	writer, err := newJobLogWriter(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	chunk := bytes.Repeat([]byte("J"), 64<<10)
	for n := int64(0); n <= maxJobLogBytes; n += int64(len(chunk)) {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	gate := make(chan struct{})
	batch := make(chan result, 28)
	for i := 0; i < 28; i++ {
		req := Request{Action: "run", Command: "printf must-not-run"}
		kind := "worker_busy"
		switch {
		case i >= 24:
			req = Request{Action: "job_output", JobID: "123", Limit: 2 * maxJobOutputReadBytes}
			kind = "job_output"
		case i >= 20:
			req = Request{Action: "read_file", Path: filePath, Offset: 1 << 20, Limit: maxFileReadBytes}
			kind = "read_file"
		case i >= 16:
			req.Root = true
			kind = "root_busy"
		}
		go func() { <-gate; batch <- call(req, kind) }()
	}
	logDone := make(chan error, 1)
	var logWrites sync.WaitGroup
	logWrites.Add(1)
	defer logWrites.Wait()
	go func() {
		defer logWrites.Done()
		<-gate
		for n := int64(0); n < maxJobLogBytes; n += int64(len(chunk)) {
			if _, err := writer.Write(chunk); err != nil {
				logDone <- err
				return
			}
		}
		logDone <- nil
	}()
	started := time.Now()
	close(gate)
	var maxBusyWait, maxDataWait time.Duration
	for i := 0; i < 28; i++ {
		got := <-batch
		if got.err != nil {
			t.Fatalf("%s transport failed: %v", got.kind, got.err)
		}
		resp := got.resp
		switch got.kind {
		case "worker_busy", "root_busy":
			maxBusyWait = max(maxBusyWait, got.wait)
			if resp.OK || resp.ErrorCode != "resource_limit" || resp.ErrorClass != "resource" || resp.Status != "busy" || !resp.Retryable || resp.Output != "" {
				t.Fatalf("%s failed to refuse without execution: %+v", got.kind, resp)
			}
		case "read_file":
			maxDataWait = max(maxDataWait, got.wait)
			if !resp.OK || resp.BytesReturned != maxFileReadBytes || resp.NextOffset == nil || *resp.NextOffset != 2<<20 || resp.FileSize == nil || *resp.FileSize != fileBytes || resp.FileVersion == "" {
				t.Fatalf("file range failed under saturation: ok=%v code=%s bytes=%d", resp.OK, resp.ErrorCode, resp.BytesReturned)
			}
		case "job_output":
			maxDataWait = max(maxDataWait, got.wait)
			if !resp.OK || !resp.RetentionTruncated || resp.BytesReturned != maxJobOutputReadBytes || resp.NextOffset == nil || *resp.NextOffset != resp.AvailableFromOffset+resp.BytesReturned || resp.Output != strings.Repeat("J", maxJobOutputReadBytes) {
				t.Fatalf("retained job range failed under saturation: ok=%v code=%s bytes=%d available=%d", resp.OK, resp.ErrorCode, resp.BytesReturned, resp.AvailableFromOffset)
			}
		}
	}
	if err := <-logDone; err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(logPath)
	if err != nil || fi.Size() != maxJobLogBytes+jobLogHeaderSize {
		t.Fatalf("streaming ring physical bound: size=%v error=%v", fi, err)
	}
	batchWait := time.Since(started)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < active; i++ {
		got := <-completed
		resp := got.resp
		if got.err != nil || !resp.OK || resp.ExitCode != 0 || resp.TimedOut || !resp.Truncated || resp.OutputEncoding != "utf-8" || resp.BytesSeen != noisyBytes+8 || resp.BytesReturned != maxSyncOutputBytes || resp.OmittedBytes != noisyBytes+8-maxSyncOutputBytes || !strings.HasPrefix(resp.Output, "HEAD") || !strings.HasSuffix(resp.Output, "TAIL") {
			t.Fatalf("noisy command bound/diagnostics failed: err=%v ok=%v code=%s seen=%d returned=%d", got.err, resp.OK, resp.ErrorCode, resp.BytesSeen, resp.BytesReturned)
		}
		if len(encodeExecutorResponse(resp)) >= maxExecutorResponseBytes {
			t.Fatal("bounded command exceeded executor frame budget")
		}
	}
	for _, root := range []bool{false, true} {
		got := call(Request{Action: "run", Root: root, Command: "printf recovered"}, "recovery")
		if got.err != nil || !got.resp.OK || got.resp.Output != "recovered" || got.resp.Truncated {
			t.Fatalf("capacity did not recover: root=%v error=%v code=%s", root, got.err, got.resp.ErrorCode)
		}
	}
	t.Logf("GOMAXPROCS=%d worker_slots=%d root_slots=%d active_noisy=%d raw_bytes_each=%d retained_bytes_each=%d excess_worker=16 excess_root=4 file_ranges=4 job_ranges=4 range_bytes=%d job_log_physical=%d job_logical_bytes=%d batch=%s max_busy=%s max_data=%s", runtime.GOMAXPROCS(0), cap(s.runs.worker), cap(s.runs.root), active, noisyBytes+8, maxSyncOutputBytes, maxFileReadBytes, fi.Size(), 2*maxJobLogBytes+int64(len(chunk)), batchWait, maxBusyWait, maxDataWait)
}
