package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func transferFixture(t *testing.T) (workspaceFileOperation, string) {
	t.Helper()
	root, repo, op := workspaceFixture(t)
	home := filepath.Join(root, "worker-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	op.TransferRoot = filepath.Join(home, "transfers")
	if err := prepareWorkerTransferRoot(op.TransferRoot); err != nil {
		t.Fatal(err)
	}
	op.Action = "workspace_transfer"
	op.PrincipalID = "direct-default"
	op.PrincipalClass = "direct"
	op.TransferQuotaMiB = 64
	op.TransferTTLSeconds = 3600
	op.TransferMaxSessions = 8
	op.MustNotExist = true
	op.TransferKey = "round-trip-test-00001"
	op.Path = "src/binary.bin"
	_ = repo
	return op, filepath.Join(root, "repo", "src", "binary.bin")
}

func bytesDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func applyTransfer(t *testing.T, op workspaceFileOperation) Response {
	t.Helper()
	res := workerWorkspaceTransfer(op)
	if !res.OK {
		t.Fatalf("transfer %s failed: code=%s error=%s", op.TransferOp, res.ErrorCode, res.Error)
	}
	return res
}
func putTransferChunk(t *testing.T, op workspaceFileOperation, id string, offset int64, data []byte) Response {
	t.Helper()
	op.TransferOp = "chunk"
	op.TransferID = id
	op.TransferKey = ""
	op.ExpectedSize = 0
	op.SHA256 = ""
	op.MustNotExist = false
	op.FileVersion = ""
	op.ChunkBase64 = base64.StdEncoding.EncodeToString(data)
	op.ChunkSHA256 = bytesDigest(data)
	op.Offset = offset
	return applyTransfer(t, op)
}
func transferFollowup(op workspaceFileOperation, id, kind string) workspaceFileOperation {
	op.TransferOp = kind
	op.TransferID = id
	op.TransferKey = ""
	op.ExpectedSize = 0
	op.SHA256 = ""
	op.MustNotExist = false
	op.FileVersion = ""
	op.Offset = 0
	op.ChunkBase64 = ""
	op.ChunkSHA256 = ""
	return op
}

func TestWorkspaceTransferBinaryResumeAndAtomicCommit(t *testing.T) {
	for _, size := range []int{110, 32 << 10, (5 << 20) + 17} {
		t.Run(strings.ReplaceAll(string(rune(size%26+'a')), " ", ""), func(t *testing.T) {
			op, target := transferFixture(t)
			op.ExpectedSize = int64(size)
			body := make([]byte, size)
			for i := range body {
				body[i] = byte(i*19 + 17)
			}
			op.SHA256 = bytesDigest(body)
			op.TransferOp = "begin"
			first := applyTransfer(t, op)
			if !transferIDValid(first.TransferID) || first.TransferredBytes != 0 {
				t.Fatalf("bad start %+v", first)
			}
			again := applyTransfer(t, op)
			if again.TransferID != first.TransferID {
				t.Fatalf("begin was not idempotent: %+v", again)
			}
			op.SHA256 = bytesDigest([]byte("conflict"))
			if conflict := workerWorkspaceTransfer(op); conflict.OK || conflict.ErrorCode != "transfer_begin_conflict" {
				t.Fatalf("reused key conflict: %+v", conflict)
			}
			op.SHA256 = bytesDigest(body)
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("partial target published at begin: %v", err)
			}
			bad := transferFollowup(op, first.TransferID, "chunk")
			bad.Offset = 1
			bad.ChunkBase64 = base64.StdEncoding.EncodeToString([]byte("wrong"))
			bad.ChunkSHA256 = bytesDigest([]byte("wrong"))
			if got := workerWorkspaceTransfer(bad); got.OK || got.ErrorCode != "transfer_offset_conflict" {
				t.Fatalf("out of order accepted: %+v", got)
			}
			offset := int64(0)
			for offset < int64(size) {
				n := 32 << 10
				if int64(n) > int64(size)-offset {
					n = int(int64(size) - offset)
				}
				window := body[offset : offset+int64(n)]
				receipt := putTransferChunk(t, op, first.TransferID, offset, window)
				if receipt.TransferredBytes != offset+int64(n) || *receipt.NextOffset != offset+int64(n) {
					t.Fatalf("checkpoint wrong %+v", receipt)
				}
				if offset == 0 {
					replay := putTransferChunk(t, op, first.TransferID, offset, window)
					if replay.TransferredBytes != int64(n) {
						t.Fatalf("replay double-counted %+v", replay)
					}
					altered := bytes.Clone(window)
					altered[0] ^= 0xff
					invalid := transferFollowup(op, first.TransferID, "chunk")
					invalid.Offset = 0
					invalid.ChunkBase64 = base64.StdEncoding.EncodeToString(altered)
					invalid.ChunkSHA256 = bytesDigest(altered)
					if got := workerWorkspaceTransfer(invalid); got.OK || got.ErrorCode != "transfer_replay_conflict" {
						t.Fatalf("conflicting replay accepted: %+v", got)
					}
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("published before commit: %v", err)
				}
				offset += int64(n)
			}
			status := applyTransfer(t, transferFollowup(op, first.TransferID, "status"))
			if status.TransferredBytes != int64(size) || status.Status != "uploading" {
				t.Fatalf("bad durable status %+v", status)
			}
			committed := applyTransfer(t, transferFollowup(op, first.TransferID, "commit"))
			if committed.Status != "committed" || committed.FileVersion == "" || committed.FileSize == nil || *committed.FileSize != int64(size) {
				t.Fatalf("bad commit %+v", committed)
			}
			repeated := applyTransfer(t, transferFollowup(op, first.TransferID, "commit"))
			if repeated.FileVersion != committed.FileVersion {
				t.Fatalf("duplicate commit changed destination %+v", repeated)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("committed payload mismatch err=%v", err)
			}
			read := readWorkerWorkspaceFile(workspaceFileOperation{Action: "workspace_stat", WorkspaceRoot: op.WorkspaceRoot, Workspace: op.Workspace, Path: op.Path, FileVersion: committed.FileVersion})
			if !read.OK || read.FileSize == nil || *read.FileSize != int64(size) {
				t.Fatalf("inconsistent published stat %+v", read)
			}
		})
	}
}

func TestWorkspaceTransferStaleDestinationAndFullDigest(t *testing.T) {
	op, target := transferFixture(t)
	if err := os.WriteFile(target, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	current := readWorkerWorkspaceFile(workspaceFileOperation{Action: "workspace_stat", WorkspaceRoot: op.WorkspaceRoot, Workspace: op.Workspace, Path: op.Path})
	if !current.OK {
		t.Fatal(current)
	}
	op.MustNotExist = false
	op.FileVersion = current.FileVersion
	op.TransferOp = "begin"
	op.TransferKey = "replace-first-0001"
	op.ExpectedSize = 3
	op.SHA256 = bytesDigest([]byte("new"))
	first := applyTransfer(t, op)
	putTransferChunk(t, op, first.TransferID, 0, []byte("new"))
	if err := os.WriteFile(target, []byte("changed outside"), 0644); err != nil {
		t.Fatal(err)
	}
	changed := workerWorkspaceTransfer(transferFollowup(op, first.TransferID, "commit"))
	if changed.OK || changed.ErrorCode != "file_changed" {
		t.Fatalf("stale replace not rejected %+v", changed)
	}
	actual, _ := os.ReadFile(target)
	if string(actual) != "changed outside" {
		t.Fatalf("stale replace destroyed content %q", actual)
	}
	// A self-consistent chunk sequence can still have the wrong declared full digest.
	op.TransferKey = "bad-global-hash-0001"
	newstat := readWorkerWorkspaceFile(workspaceFileOperation{Action: "workspace_stat", WorkspaceRoot: op.WorkspaceRoot, Workspace: op.Workspace, Path: op.Path})
	op.FileVersion = newstat.FileVersion
	op.SHA256 = bytesDigest([]byte("wrong"))
	second := applyTransfer(t, op)
	putTransferChunk(t, op, second.TransferID, 0, []byte("new"))
	bad := workerWorkspaceTransfer(transferFollowup(op, second.TransferID, "commit"))
	if bad.OK || bad.ErrorCode != "transfer_hash_mismatch" {
		t.Fatalf("wrong full digest published %+v", bad)
	}
	final, _ := os.ReadFile(target)
	if string(final) != "changed outside" {
		t.Fatal("wrong full digest changed destination")
	}
}

func TestWorkspaceTransferAbortIsolationExpiryAndQuota(t *testing.T) {
	op, target := transferFixture(t)
	op.TransferOp = "begin"
	op.ExpectedSize = 120
	op.SHA256 = bytesDigest(make([]byte, 120))
	a := applyTransfer(t, op)
	badPrincipal := transferFollowup(op, a.TransferID, "status")
	badPrincipal.PrincipalID = "mcp-gateway"
	badPrincipal.PrincipalClass = "gateway"
	if res := workerWorkspaceTransfer(badPrincipal); res.OK || res.ErrorCode != "transfer_scope_mismatch" {
		t.Fatalf("principal leaked %+v", res)
	}
	badWorkspace := transferFollowup(op, a.TransferID, "status")
	badWorkspace.Workspace = filepath.Join(filepath.Dir(op.Workspace), "another")
	if res := workerWorkspaceTransfer(badWorkspace); res.OK {
		t.Fatalf("cross workspace leaked %+v", res)
	}
	badPath := transferFollowup(op, a.TransferID, "status")
	badPath.Path = "src/another.bin"
	if res := workerWorkspaceTransfer(badPath); res.OK || res.ErrorCode != "transfer_scope_mismatch" {
		t.Fatalf("different path leaked %+v", res)
	}
	badPath.Path = "../escape"
	if res := workerWorkspaceTransfer(badPath); res.OK || res.ErrorCode != "invalid_transfer" {
		t.Fatalf("traversal accepted %+v", res)
	}
	op.TransferMaxSessions = 1
	op.TransferKey = "abort-second-key-0002"
	if res := workerWorkspaceTransfer(op); res.OK || res.ErrorCode != "transfer_quota_exceeded" {
		t.Fatalf("capacity not enforced %+v", res)
	}
	aborted := applyTransfer(t, transferFollowup(op, a.TransferID, "abort"))
	if aborted.Status != "aborted" {
		t.Fatalf("abort %+v", aborted)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("aborted file visible %v", err)
	}
	// Expiration runs on begin; a failed/resumable record remains inspectable until TTL.
	dir := filepath.Join(op.TransferRoot, a.TransferID)
	rec, err := transferReadRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.ExpiresUnix = time.Now().Add(-time.Second).Unix()
	if err := transferSaveRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	op.TransferKey = "reusable-after-expiry-key-3"
	next := applyTransfer(t, op)
	if next.TransferID == a.TransferID {
		t.Fatal("new session reused expired key")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expired session still on disk: %v", err)
	}
}

func TestWorkspaceTransferSymlinkAndPrecondition(t *testing.T) {
	op, target := transferFixture(t)
	op.TransferOp = "begin"
	op.ExpectedSize = 1
	op.SHA256 = bytesDigest([]byte{1})
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	res := applyTransfer(t, op)
	putTransferChunk(t, op, res.TransferID, 0, []byte{1})
	bad := workerWorkspaceTransfer(transferFollowup(op, res.TransferID, "commit"))
	if bad.OK || bad.ErrorCode != "unsafe_file_type" {
		t.Fatalf("symlink destination not refused %+v", bad)
	}
	actual, _ := os.ReadFile(outside)
	if string(actual) != "sensitive" {
		t.Fatal("symlink target modified")
	}
	op.TransferKey = "new-linked-parent-key-2"
	op.Path = "linked-parent/out.dat"
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(op.Workspace, "linked-parent")); err != nil {
		t.Fatal(err)
	}
	if got := workerWorkspaceTransfer(op); got.OK {
		t.Fatalf("symlinked parent accepted %+v", got)
	}
}
