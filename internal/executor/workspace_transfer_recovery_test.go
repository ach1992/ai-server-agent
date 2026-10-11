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
		if point == phase {
			os.Exit(73)
		}
	}
	result := workerWorkspaceTransfer(op)
	t.Fatalf("crash phase %q not reached, result %+v", phase, result)
}

func transferCrashAt(t *testing.T, op workspaceFileOperation, phase string) {
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
	for _, phase := range []string{"prepare_temp_before_checkpoint", "committing_after_checkpoint", "published_before_receipt"} {
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
	oldTime := time.Now().Add(-3 * time.Hour).Unix()
	res := transferCommit(op, session, &rec, oldTime)
	if !res.OK || res.Status != "committed" {
		t.Fatalf("expired commit from stale operation time: %+v", res)
	}
	if rec.ExpiresUnix <= time.Now().Unix() {
		t.Fatalf("commit receipt expired during valid prepare: %+v", rec)
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
		op, _ := transferFixture(t)
		op.TransferOp = "begin"
		op.ExpectedSize = 16
		op.SHA256 = bytesDigest(make([]byte, 16))
		transferCrashAt(t, op, "begin_after_data")
		session := filepath.Join(op.TransferRoot, transferIDFor(op))
		gc := op
		gc.Workspace = op.WorkspaceRoot
		gc.Action = "workspace_transfer_gc"
		if resp := workerWorkspaceTransferGC(gc); !resp.OK {
			t.Fatalf("independent sweep blocked by interrupted begin: %+v", resp)
		}
		if _, err := os.Stat(session); !os.IsNotExist(err) {
			t.Fatal("manifestless interrupted begin leaked state")
		}
	})
}
