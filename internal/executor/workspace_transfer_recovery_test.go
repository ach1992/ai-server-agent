package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Child exits in the middle of the *real* filesystem state transition.
// This exercises crash semantics, not a normal return/defer cleanup path.
func TestTransferFaultChild(t *testing.T) {
	spec := os.Getenv("ASA_TRANSFER_FAULT_SPEC")
	if spec == "" {
		t.Skip("only invoked as an isolated crash child")
	}
	b, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	var op workspaceFileOperation
	if err = json.Unmarshal(b, &op); err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv("ASA_TRANSFER_FAULT_PHASE")
	transferFaultHook = func(point string) {
		if point == "before_existing_exchange" && os.Getenv("ASA_TRANSFER_RACE_REPLACE") == "1" {
			dest := filepath.Join(op.Workspace, op.Path)
			rival := dest + ".concurrent"
			if e := os.WriteFile(rival, []byte("concurrent writer must be preserved"), 0644); e != nil {
				t.Fatal(e)
			}
			if e := os.Rename(rival, dest); e != nil {
				t.Fatal(e)
			}
		}
		if point == phase {
			os.Exit(73)
		}
	}
	result := workerWorkspaceTransfer(op)
	t.Fatalf("crash phase %q not reached, result %+v", phase, result)
}

func transferCrashAt(t *testing.T, op workspaceFileOperation, phase string) {
	transferCrashAtWithRace(t, op, phase, false)
}
func transferCrashAtWithRace(t *testing.T, op workspaceFileOperation, phase string, race bool) {
	t.Helper()
	dir := t.TempDir()
	spec := filepath.Join(dir, "operation.json")
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec, b, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTransferFaultChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ASA_TRANSFER_FAULT_SPEC="+spec, "ASA_TRANSFER_FAULT_PHASE="+phase)
	if race {
		cmd.Env = append(cmd.Env, "ASA_TRANSFER_RACE_REPLACE=1")
	}
	output, err := cmd.CombinedOutput()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 73 {
		t.Fatalf("expected abrupt helper exit 73 at %s, error=%v output=%s", phase, err, output)
	}
}

func transferSeedReady(t *testing.T, size int) (workspaceFileOperation, string, []byte) {
	t.Helper()
	op, target := transferFixture(t)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + 109)
	}
	op.TransferOp = "begin"
	op.ExpectedSize = int64(size)
	op.SHA256 = bytesDigest(data)
	begin := applyTransfer(t, op)
	session := filepath.Join(op.TransferRoot, begin.TransferID)
	f, err := os.OpenFile(filepath.Join(session, "data"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.WriteAt(data, 0)
	if err != nil || n != size {
		t.Fatalf("seed data: %v wrote=%d", err, n)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	rec, err := transferReadRecord(session)
	if err != nil {
		t.Fatal(err)
	}
	rec.Received = int64(size)
	if err = transferSaveRecord(session, rec); err != nil {
		t.Fatal(err)
	}
	return transferFollowup(op, begin.TransferID, "commit"), target, data
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file differs: %s bytes=%d err=%v", path, len(got), err)
	}
}

func TestTransferCrashRecoveryPartialBeginSameKey(t *testing.T) {
	for _, phase := range []string{"begin_after_mkdir", "begin_after_data", "begin_after_manifest"} {
		t.Run(phase, func(t *testing.T) {
			op, target := transferFixture(t)
			op.TransferOp = "begin"
			op.ExpectedSize = 1
			op.SHA256 = bytesDigest([]byte{37})
			transferCrashAt(t, op, phase)
			pending := filepath.Join(op.TransferRoot, transferIDFor(op))
			if _, err := os.Stat(pending); err != nil {
				t.Fatal(err)
			}
			changed := op
			changed.SHA256 = bytesDigest([]byte{38})
			if conflict := workerWorkspaceTransfer(changed); conflict.OK || conflict.ErrorCode != "transfer_begin_conflict" {
				t.Fatalf("different material key incorrectly accepted: %+v", conflict)
			}
			recovered := applyTransfer(t, op)
			if recovered.TransferID != transferIDFor(op) {
				t.Fatal("exact key recovery changed session identity")
			}
			putTransferChunk(t, op, recovered.TransferID, 0, []byte{37})
			done := applyTransfer(t, transferFollowup(op, recovered.TransferID, "commit"))
			if done.Status != "committed" {
				t.Fatalf("not committed: %+v", done)
			}
			assertBytes(t, target, []byte{37})
		})
	}
}

func TestTransferCrashRecoveryBoundedPreparation(t *testing.T) {
	op, target, data := transferSeedReady(t, (13<<20)+19)
	transferCrashAt(t, op, "prepare_window_before_checkpoint")
	session := filepath.Join(op.TransferRoot, op.TransferID)
	rec, err := transferReadRecord(session)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "preparing" || rec.PreparedBytes != 0 {
		t.Fatalf("checkpoint advanced before durable write %+v", rec)
	}
	prepared, err := os.Stat(filepath.Join(filepath.Dir(target), rec.TempName))
	if err != nil || prepared.Size() != transferPrepareWindowBytes {
		t.Fatalf("expected unacknowledged prepared tail: %v %+v", err, prepared)
	}
	recovered := applyTransfer(t, transferFollowup(op, op.TransferID, "status"))
	if recovered.Status != "preparing" || recovered.PreparedBytes != 0 {
		t.Fatalf("status lost checkpoint %+v", recovered)
	}
	next := workerWorkspaceTransfer(op)
	if !next.OK || next.Status != "preparing" || next.PreparedBytes != transferPrepareWindowBytes {
		t.Fatalf("retry failed to safely truncate/replay the uncommitted window %+v", next)
	}
	transferCrashAt(t, op, "prepare_window_after_checkpoint")
	rec, err = transferReadRecord(session)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PreparedBytes != 2*transferPrepareWindowBytes {
		t.Fatalf("persisted checkpoint lost after abrupt exit: %+v", rec)
	}
	resumed := workerWorkspaceTransfer(op)
	if !resumed.OK || resumed.PreparedBytes != 3*transferPrepareWindowBytes {
		t.Fatalf("did not resume from recorded progress: %+v", resumed)
	}
	final := applyTransfer(t, op)
	if final.Status != "committed" {
		t.Fatal(final)
	}
	assertBytes(t, target, data)
}

func TestTransferCrashRecoveryInitialTempAndCommitRename(t *testing.T) {
	for _, phase := range []string{"prepare_temp_before_checkpoint", "prepare_checkpoint_before_link", "prepare_link_after_checkpoint", "committing_after_checkpoint", "published_before_receipt"} {
		t.Run(phase, func(t *testing.T) {
			op, target, data := transferSeedReady(t, 128<<10)
			transferCrashAt(t, op, phase)
			state, err := transferReadRecord(filepath.Join(op.TransferRoot, op.TransferID))
			if err != nil {
				t.Fatal(err)
			}
			if phase == "prepare_temp_before_checkpoint" && state.Status != "uploading" {
				t.Fatalf("prepared before manifest checkpoint: %+v", state)
			}
			if phase == "prepare_checkpoint_before_link" && state.Status != "preparing" {
				t.Fatalf("durable unnamed inode intent missing: %+v", state)
			}
			if phase == "prepare_link_after_checkpoint" && state.Status != "preparing" {
				t.Fatalf("durable linked inode intent missing: %+v", state)
			}
			if phase == "committing_after_checkpoint" && state.Status != "committing" {
				t.Fatalf("missing commit intent: %+v", state)
			}
			if phase == "published_before_receipt" && state.Status != "committing" {
				t.Fatalf("missing published commit intent: %+v", state)
			}
			final := applyTransfer(t, op)
			if final.Status != "committed" {
				t.Fatalf("could not reconcile interrupted commit: %+v", final)
			}
			assertBytes(t, target, data)
			replay := applyTransfer(t, op)
			if replay.FileVersion != final.FileVersion {
				t.Fatal("replayed publication or lost durable receipt")
			}
		})
	}
}

func TestTransferCrashTerminalPhysicalQuotaAndReconciliation(t *testing.T) {
	t.Run("aborted", func(t *testing.T) {
		op, target, data := transferSeedReady(t, 700<<10)
		abort := transferFollowup(op, op.TransferID, "abort")
		transferCrashAt(t, abort, "aborted_after_checkpoint")
		_, reserved, _, err := transferInventory(op, time.Now().Unix())
		if err != nil || reserved != int64(len(data)) {
			t.Fatalf("aborted retained data not quota-accounted: %d %v", reserved, err)
		}
		status := applyTransfer(t, transferFollowup(op, op.TransferID, "status"))
		if status.Status != "aborted" {
			t.Fatalf("abort receipt lost %+v", status)
		}
		if _, err = os.Stat(filepath.Join(op.TransferRoot, op.TransferID, "data")); !os.IsNotExist(err) {
			t.Fatalf("status did not clean staging bytes: %v", err)
		}
		if _, err = os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("abort published destination: %v", err)
		}
		_, reserved, _, err = transferInventory(op, time.Now().Unix())
		if err != nil || reserved != 0 {
			t.Fatalf("abort cleanup did not release quota: %d %v", reserved, err)
		}
	})
	t.Run("committed", func(t *testing.T) {
		op, target, data := transferSeedReady(t, 700<<10)
		transferCrashAt(t, op, "committed_after_checkpoint")
		assertBytes(t, target, data)
		before, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		_, reserved, _, err := transferInventory(op, time.Now().Unix())
		if err != nil || reserved != int64(len(data)) {
			t.Fatalf("committed leftover staging not charged: %d %v", reserved, err)
		}
		status := applyTransfer(t, transferFollowup(op, op.TransferID, "status"))
		if status.Status != "committed" {
			t.Fatalf("committed receipt lost %+v", status)
		}
		after, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) {
			t.Fatal("status republished committed target")
		}
		if _, err = os.Stat(filepath.Join(op.TransferRoot, op.TransferID, "data")); !os.IsNotExist(err) {
			t.Fatalf("committed status did not reclaim staged bytes: %v", err)
		}
		_, reserved, _, err = transferInventory(op, time.Now().Unix())
		if err != nil || reserved != 0 {
			t.Fatalf("committed cleanup did not release quota: %d %v", reserved, err)
		}
	})
}

func TestTransferSweeperWithoutFutureClientBegin(t *testing.T) {
	op, target, _ := transferSeedReady(t, (5<<20)+3)
	started := workerWorkspaceTransfer(op)
	if !started.OK || started.Status != "preparing" {
		t.Fatalf("no prepared window: %+v", started)
	}
	session := filepath.Join(op.TransferRoot, op.TransferID)
	rec, err := transferReadRecord(session)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(filepath.Dir(target), rec.TempName)
	if _, err = os.Stat(temp); err != nil {
		t.Fatal(err)
	}
	rec.ExpiresUnix = time.Now().Add(-time.Second).Unix()
	if err = transferSaveRecord(session, rec); err != nil {
		t.Fatal(err)
	}
	gc := op
	gc.Action = "workspace_transfer_gc"
	gc.Workspace = op.WorkspaceRoot
	if result := workerWorkspaceTransferGC(gc); !result.OK {
		t.Fatalf("background sweep failed: %+v", result)
	}
	if _, err = os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("expired staging directory survived independent sweep: %v", err)
	}
	if _, err = os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("expired prepared file survived independent sweep: %v", err)
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("GC published incomplete target: %v", err)
	}
}

func TestTransferTTLCheckpointUsesActualCompletionTime(t *testing.T) {
	op, target, data := transferSeedReady(t, 13<<10)
	session := filepath.Join(op.TransferRoot, op.TransferID)
	rec, err := transferReadRecord(session)
	if err != nil {
		t.Fatal(err)
	}
	checkpointStart := time.Now().Unix()
	res := transferCommit(op, session, &rec)
	if !res.OK || res.Status != "committed" {
		t.Fatalf("expired commit from stale operation time: %+v", res)
	}
	if rec.ExpiresUnix < checkpointStart+int64(op.TransferTTLSeconds) {
		t.Fatalf("commit receipt did not use a current durable checkpoint clock: %+v", rec)
	}
	assertBytes(t, target, data)
}

func TestTransferLargePreparationMakesMonotonicProgress(t *testing.T) {
	op, target, data := transferSeedReady(t, (33<<20)+13)
	last := int64(0)
	steps := 0
	for {
		resp := workerWorkspaceTransfer(op)
		if !resp.OK {
			t.Fatalf("step %d: %+v", steps, resp)
		}
		if resp.Status == "committed" {
			break
		}
		steps++
		if resp.Status != "preparing" || resp.PreparedBytes <= last || resp.PreparedBytes-last > transferPrepareWindowBytes {
			t.Fatalf("unbounded/nonprogress preparation at step=%d %+v", steps, resp)
		}
		last = resp.PreparedBytes
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("published before full verified commit: %v", err)
		}
		if steps > 30 {
			t.Fatal("preparation did not converge")
		}
	}
	if steps < 8 {
		t.Fatalf("large fixture did not exercise multiple bounded checkpoints: %d", steps)
	}
	assertBytes(t, target, data)
	digest := sha256.Sum256(data)
	result, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(result) != digest {
		t.Fatal("large file digest changed")
	}
	t.Log(fmt.Sprintf("large transfer preparation: bytes=%d bounded_windows=%d", len(data), steps+1))
}

func TestTransferSweepAfterQuotaReductionAndInterruptedBegin(t *testing.T) {
	t.Run("reduced_quota", func(t *testing.T) {
		op, target, _ := transferSeedReady(t, 17<<20)
		session := filepath.Join(op.TransferRoot, op.TransferID)
		rec, err := transferReadRecord(session)
		if err != nil {
			t.Fatal(err)
		}
		rec.ExpiresUnix = time.Now().Add(-time.Second).Unix()
		if err = transferSaveRecord(session, rec); err != nil {
			t.Fatal(err)
		}
		op.TransferQuotaMiB = 16 // previously accepted object must still be reclaimable
		gc := op
		gc.Workspace = op.WorkspaceRoot
		gc.Action = "workspace_transfer_gc"
		if resp := workerWorkspaceTransferGC(gc); !resp.OK {
			t.Fatalf("quota reduction blocked expiry: %+v", resp)
		}
		if _, err = os.Stat(session); !os.IsNotExist(err) {
			t.Fatal("orphan after quota reduction")
		}
		if _, err = os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("expired uncommitted bytes published")
		}
	})
	t.Run("incomplete_manifestless_begin", func(t *testing.T) {
		for _, phase := range []string{"begin_after_mkdir", "begin_after_data"} {
			t.Run(phase, func(t *testing.T) {
				op, _ := transferFixture(t)
				op.TransferOp = "begin"
				op.ExpectedSize = 16
				op.SHA256 = bytesDigest(make([]byte, 16))
				op.TransferTTLSeconds = 60
				transferCrashAt(t, op, phase)
				session := filepath.Join(op.TransferRoot, transferIDFor(op))
				gc := op
				gc.Workspace = op.WorkspaceRoot
				gc.Action = "workspace_transfer_gc"
				if resp := workerWorkspaceTransferGC(gc); !resp.OK {
					t.Fatalf("GC failed: %+v", resp)
				}
				if _, err := os.Stat(session); err != nil {
					t.Fatalf("unexpired material reservation erased: %v", err)
				}
				other := op
				other.SHA256 = bytesDigest([]byte("different"))
				if response := workerWorkspaceTransfer(other); response.OK || response.ErrorCode != "transfer_begin_conflict" {
					t.Fatalf("GC lost key-material reservation: %+v", response)
				}
				recovered := applyTransfer(t, op)
				if recovered.TransferID != transferIDFor(op) {
					t.Fatal("same-key recovery changed transfer ID")
				}
				record, err := transferReadRecord(session)
				if err != nil {
					t.Fatal(err)
				}
				record.ExpiresUnix = time.Now().Add(-time.Second).Unix()
				if err := transferSaveRecord(session, record); err != nil {
					t.Fatal(err)
				}
				if resp := workerWorkspaceTransferGC(gc); !resp.OK {
					t.Fatalf("post-retention GC failed: %+v", resp)
				}
				if _, err := os.Stat(session); !os.IsNotExist(err) {
					t.Fatalf("expired reservation was not reclaimed: %v", err)
				}
				different := applyTransfer(t, other)
				if different.TransferID == recovered.TransferID {
					t.Fatal("different material reused old opaque handle")
				}
			})
		}
	})
}

// A slow commit was already covered; begin and chunk must likewise use the
// clock at the durable checkpoint rather than the call-entry timestamp.
func TestTransferBeginChunkExpiryUsesCheckpointClock(t *testing.T) {
	op, _ := transferFixture(t)
	op.TransferTTLSeconds = 60
	op.TransferOp = "begin"
	op.ExpectedSize = 3
	op.SHA256 = bytesDigest([]byte("abc"))
	initial := time.Now().Unix()
	transferFaultHook = func(phase string) {
		if phase == "begin_before_checkpoint" {
			time.Sleep(2200 * time.Millisecond)
		}
	}
	began := workerWorkspaceTransfer(op)
	transferFaultHook = nil
	if !began.OK {
		t.Fatalf("begin failed: %+v", began)
	}
	record, err := transferReadRecord(filepath.Join(op.TransferRoot, began.TransferID))
	if err != nil {
		t.Fatal(err)
	}
	if record.ExpiresUnix < initial+61 {
		t.Fatalf("begin TTL stale: initial=%d expires=%d", initial, record.ExpiresUnix)
	}
	appendOp := transferFollowup(op, began.TransferID, "chunk")
	appendOp.ChunkBase64 = "YWJj"
	appendOp.ChunkSHA256 = bytesDigest([]byte("abc"))
	secondStart := time.Now().Unix()
	transferFaultHook = func(phase string) {
		if phase == "chunk_before_checkpoint" {
			time.Sleep(2200 * time.Millisecond)
		}
	}
	appended := workerWorkspaceTransfer(appendOp)
	transferFaultHook = nil
	if !appended.OK {
		t.Fatalf("chunk failed: %+v", appended)
	}
	record, err = transferReadRecord(filepath.Join(op.TransferRoot, began.TransferID))
	if err != nil {
		t.Fatal(err)
	}
	if record.ExpiresUnix < secondStart+61 {
		t.Fatalf("chunk TTL stale: initial=%d expires=%d", secondStart, record.ExpiresUnix)
	}
}

// A worker-owned inode at the transfer temp path is not proof of ownership.
// The crash here leaves an UNNAMED inode (automatically reclaimed by the
// kernel), so an unrelated named inode must survive all cleanup attempts.
func TestTransferUnprovenTempIsNeverDeleted(t *testing.T) {
	for _, operation := range []string{"commit", "abort", "gc"} {
		t.Run(operation, func(t *testing.T) {
			op, target, _ := transferSeedReady(t, 4096)
			transferCrashAt(t, op, "prepare_temp_before_checkpoint")
			session := filepath.Join(op.TransferRoot, op.TransferID)
			record, err := transferReadRecord(session)
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != "uploading" || record.PreparedIno != 0 {
				t.Fatal("unnamed inode unexpectedly journaled")
			}
			fake := filepath.Join(filepath.Dir(target), record.TempName)
			payload := []byte("OTHER WORKER DATA: do not unlink")
			if err := os.WriteFile(fake, payload, 0600); err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(fake)
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "commit":
				resp := workerWorkspaceTransfer(op)
				if resp.OK || resp.ErrorCode != "transfer_temp_recovery_failed" {
					t.Fatalf("unsafe retry accepted: %+v", resp)
				}
			case "abort":
				resp := workerWorkspaceTransfer(transferFollowup(op, op.TransferID, "abort"))
				if resp.OK || resp.ErrorCode != "transfer_temp_cleanup_failed" {
					t.Fatalf("unsafe abort accepted: %+v", resp)
				}
			case "gc":
				record.ExpiresUnix = time.Now().Add(-time.Second).Unix()
				if err := transferSaveRecord(session, record); err != nil {
					t.Fatal(err)
				}
				gc := op
				gc.Action = "workspace_transfer_gc"
				gc.Workspace = op.WorkspaceRoot
				resp := workerWorkspaceTransferGC(gc)
				if resp.OK || resp.ErrorCode != "transfer_cleanup_failed" {
					t.Fatalf("unsafe expiry cleanup accepted: %+v", resp)
				}
			}
			assertBytes(t, fake, payload)
			after, err := os.Stat(fake)
			if err != nil || !os.SameFile(original, after) {
				t.Fatalf("unrelated inode replaced or deleted: %v", err)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("unproven temp published: %v", err)
			}
		})
	}
}

func transferSeedReplacement(t *testing.T) (workspaceFileOperation, string, []byte) {
	t.Helper()
	op, target := transferFixture(t)
	if err := os.WriteFile(target, []byte("prior authorized destination"), 0644); err != nil {
		t.Fatal(err)
	}
	st := readWorkerWorkspaceFile(workspaceFileOperation{Action: "workspace_stat", WorkspaceRoot: op.WorkspaceRoot, Workspace: op.Workspace, Path: op.Path})
	if !st.OK {
		t.Fatal(st)
	}
	body := bytes.Repeat([]byte("new verified upload "), 8192)
	op.TransferOp = "begin"
	op.MustNotExist = false
	op.FileVersion = st.FileVersion
	op.ExpectedSize = int64(len(body))
	op.SHA256 = bytesDigest(body)
	began := applyTransfer(t, op)
	for pos := 0; pos < len(body); {
		n := 256 << 10
		if n > len(body)-pos {
			n = len(body) - pos
		}
		putTransferChunk(t, op, began.TransferID, int64(pos), body[pos:pos+n])
		pos += n
	}
	return transferFollowup(op, began.TransferID, "commit"), target, body
}

func TestTransferReplacementCrashBetweenExchangeAndVerification(t *testing.T) {
	for _, phase := range []string{"exchange_after_rename_before_displaced_check", "exchange_after_rollback_before_checkpoint"} {
		t.Run(phase, func(t *testing.T) {
			op, target, _ := transferSeedReplacement(t)
			transferCrashAtWithRace(t, op, phase, true)
			session := filepath.Join(op.TransferRoot, op.TransferID)
			record, err := transferReadRecord(session)
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != "committing" || record.ExchangeVerified {
				t.Fatalf("unexpected unverified exchange checkpoint: %+v", record)
			}
			conflict := workerWorkspaceTransfer(transferFollowup(op, op.TransferID, "status"))
			if conflict.OK || conflict.ErrorCode != "file_changed" {
				t.Fatalf("recovery did not reject raced destination: %+v", conflict)
			}
			assertBytes(t, target, []byte("concurrent writer must be preserved"))
			receipt := applyTransfer(t, transferFollowup(op, op.TransferID, "status"))
			if receipt.Status != "conflicted" {
				t.Fatalf("lost durable conflict: %+v", receipt)
			}
			repeated := workerWorkspaceTransfer(op)
			if repeated.OK || repeated.ErrorCode != "file_changed" {
				t.Fatalf("conflicted commit retried publication: %+v", repeated)
			}
			assertBytes(t, target, []byte("concurrent writer must be preserved"))
			if _, err := os.Stat(filepath.Join(filepath.Dir(target), record.TempName)); !os.IsNotExist(err) {
				t.Fatalf("candidate temp survived safe conflict cleanup: %v", err)
			}
			if _, err := os.Stat(filepath.Join(session, "data")); !os.IsNotExist(err) {
				t.Fatalf("staged transfer not released after conflict: %v", err)
			}
		})
	}
}

func TestTransferReplacementExchangeDurableVerificationBeforeCleanup(t *testing.T) {
	op, target, body := transferSeedReplacement(t)
	transferCrashAt(t, op, "exchange_verified_before_cleanup")
	session := filepath.Join(op.TransferRoot, op.TransferID)
	rec, err := transferReadRecord(session)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "committing" || !rec.ExchangeVerified {
		t.Fatalf("missing durable displaced-original proof: %+v", rec)
	}
	committed := applyTransfer(t, transferFollowup(op, op.TransferID, "status"))
	if committed.Status != "committed" {
		t.Fatal(committed)
	}
	assertBytes(t, target, body)
	if _, err = os.Stat(filepath.Join(filepath.Dir(target), rec.TempName)); !os.IsNotExist(err) {
		t.Fatalf("displaced original not cleaned: %v", err)
	}
}

func TestTransferReplacementRefusesMissingUnverifiedDisplaced(t *testing.T) {
	op, target, _ := transferSeedReplacement(t)
	transferCrashAt(t, op, "exchange_after_rename_before_displaced_check")
	rec, err := transferReadRecord(filepath.Join(op.TransferRoot, op.TransferID))
	if err != nil {
		t.Fatal(err)
	}
	displacedPath := filepath.Join(filepath.Dir(target), rec.TempName)
	if err = os.Remove(displacedPath); err != nil {
		t.Fatal(err)
	}
	result := workerWorkspaceTransfer(transferFollowup(op, op.TransferID, "status"))
	if result.OK || result.ErrorCode != "transfer_commit_unresolved" {
		t.Fatalf("publication accepted without displaced proof: %+v", result)
	}
}

func TestTransferExpiredRacedExchangeGCRecoversConcurrentDestination(t *testing.T) {
	op, target, _ := transferSeedReplacement(t)
	transferCrashAtWithRace(t, op, "exchange_after_rename_before_displaced_check", true)
	session := filepath.Join(op.TransferRoot, op.TransferID)
	rec, err := transferReadRecord(session)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "committing" {
		t.Fatal(rec)
	}
	rec.ExpiresUnix = time.Now().Add(-time.Second).Unix()
	if err = transferSaveRecord(session, rec); err != nil {
		t.Fatal(err)
	}
	gc := op
	gc.Action = "workspace_transfer_gc"
	gc.Workspace = op.WorkspaceRoot
	if response := workerWorkspaceTransferGC(gc); !response.OK {
		t.Fatalf("GC failed to reconcile verified rollback: %+v", response)
	}
	assertBytes(t, target, []byte("concurrent writer must be preserved"))
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("expired resolved transfer retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), rec.TempName)); !os.IsNotExist(err) {
		t.Fatalf("prepared temp retained: %v", err)
	}
}

func TestTransferVerifiedExchangeRefusesToOverwriteLaterWriter(t *testing.T) {
	op, target, _ := transferSeedReplacement(t)
	transferCrashAt(t, op, "exchange_verified_before_cleanup")
	if err := os.WriteFile(target+".later", []byte("later writer owns destination"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target+".later", target); err != nil {
		t.Fatal(err)
	}
	follow := workerWorkspaceTransfer(op)
	if follow.OK || follow.ErrorCode != "transfer_commit_unresolved" {
		t.Fatalf("verified publication must not be replayed over later writer: %+v", follow)
	}
	assertBytes(t, target, []byte("later writer owns destination"))
}

func TestTransferInterruptedBeginRetentionExpiresOnlyAtTTL(t *testing.T) {
	op, _ := transferFixture(t)
	op.TransferOp = "begin"
	op.TransferTTLSeconds = 60
	op.ExpectedSize = 16
	op.SHA256 = bytesDigest(make([]byte, 16))
	transferCrashAt(t, op, "begin_after_mkdir")
	session := filepath.Join(op.TransferRoot, transferIDFor(op))
	gc := op
	gc.Workspace = op.WorkspaceRoot
	gc.Action = "workspace_transfer_gc"
	if response := workerWorkspaceTransferGC(gc); !response.OK {
		t.Fatal(response)
	}
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("fresh reservation was deleted: %v", err)
	}
	changed := op
	changed.SHA256 = bytesDigest([]byte("different-16-byte"))
	if response := workerWorkspaceTransfer(changed); response.OK || response.ErrorCode != "transfer_begin_conflict" {
		t.Fatalf("same-key alternate material accepted before TTL: %+v", response)
	}
	expired := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(session, expired, expired); err != nil {
		t.Fatal(err)
	}
	if response := workerWorkspaceTransferGC(gc); !response.OK {
		t.Fatal(response)
	}
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("expired partial reservation not reclaimed: %v", err)
	}
	accepted := applyTransfer(t, changed)
	if accepted.TransferID == transferIDFor(op) {
		t.Fatal("material fingerprint was reused")
	}
}
