package executor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	transferChunkMax    = 256 << 10
	transferManifestMax = 8 << 10
	transferScanMax     = 4096
	transferDiskReserve = 16 << 20
)

type transferRecord struct {
	ID             string `json:"id"`
	PrincipalID    string `json:"principal_id"`
	PrincipalClass string `json:"principal_class"`
	Workspace      string `json:"workspace"`
	Path           string `json:"path"`
	ExpectedSize   int64  `json:"expected_size"`
	SHA256         string `json:"sha256"`
	Received       int64  `json:"received"`
	FileVersion    string `json:"file_version,omitempty"`
	MustNotExist   bool   `json:"must_not_exist"`
	Status         string `json:"status"`
	ExpiresUnix    int64  `json:"expires_unix"`
	ResultVersion  string `json:"result_version,omitempty"`
	PreparedDev    uint64 `json:"prepared_dev,omitempty"`
	PreparedIno    uint64 `json:"prepared_ino,omitempty"`
	DisplacedDev   uint64 `json:"displaced_dev,omitempty"`
	DisplacedIno   uint64 `json:"displaced_ino,omitempty"`
	TempName       string `json:"temp_name,omitempty"`
}

func transferDigestValid(v string) bool {
	return len(v) == 64 && strings.Trim(v, "0123456789abcdef") == ""
}
func transferIDValid(v string) bool {
	return len(v) == 35 && strings.HasPrefix(v, "tr_") && strings.Trim(v[3:], "0123456789abcdef") == ""
}
func transferKeyValid(s string) bool {
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}
func transferTempName(id string) string { return ".ai-server-agent-xfer-" + id[3:] }

func transferTempValid(name string) bool {
	const prefix = ".ai-server-agent-xfer-"
	return len(name) == len(prefix)+32 && strings.HasPrefix(name, prefix) && strings.Trim(name[len(prefix):], "0123456789abcdef") == ""
}

func transferIDFor(op workspaceFileOperation) string {
	encoded, _ := json.Marshal([]string{op.PrincipalClass, op.PrincipalID, filepath.Clean(op.Workspace), op.Path, op.TransferKey})
	digest := sha256.Sum256(encoded)
	return "tr_" + hex.EncodeToString(digest[:16])
}
func validateWorkspaceTransferRequest(req Request) error {
	if req.PrincipalID == "" || req.PrincipalClass == "" {
		return errors.New("authenticated principal identity required for durable transfer")
	}
	if _, err := safeWorkspaceRelativeFile(req.Path); err != nil {
		return err
	}
	if req.Root || req.Approval || req.Mode != 0 || req.Content != "" || len(req.WorkspaceEdits) > 0 {
		return errors.New("transfer accepts only worker-authority byte chunks")
	}
	switch req.TransferOp {
	case "begin":
		if !transferKeyValid(req.TransferKey) || req.TransferID != "" || req.ExpectedSize <= 0 || !transferDigestValid(req.SHA256) || req.MustNotExist == (req.FileVersion != "") || req.Offset != 0 || req.ChunkBase64 != "" || req.ChunkSHA256 != "" {
			return errors.New("begin requires total bytes, whole sha256 and exactly one destination precondition")
		}
	case "chunk":
		if req.TransferKey != "" || !transferIDValid(req.TransferID) || req.Offset < 0 || req.ChunkBase64 == "" || !transferDigestValid(req.ChunkSHA256) || len(req.ChunkBase64) > base64.StdEncoding.EncodedLen(transferChunkMax) || req.ExpectedSize != 0 || req.SHA256 != "" || req.FileVersion != "" || req.MustNotExist {
			return errors.New("chunk requires handle, bounded base64, offset and chunk sha256 only")
		}
	case "status", "commit", "abort":
		if req.TransferKey != "" || !transferIDValid(req.TransferID) || req.ExpectedSize != 0 || req.SHA256 != "" || req.ChunkBase64 != "" || req.ChunkSHA256 != "" || req.Offset != 0 || req.FileVersion != "" || req.MustNotExist {
			return errors.New("status/commit/abort require only handle and original destination identity")
		}
	default:
		return errors.New("transfer operation must be begin, chunk, status, commit or abort")
	}
	return nil
}

// No arbitrary host path is accepted from the client. This setup is run only
// as the unprivileged aiworker, before per-request Landlock confinement.
func prepareWorkerTransferRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Base(root) != "transfers" {
		return errors.New("invalid configured staging root")
	}
	parentFD, err := unix.Openat2(unix.AT_FDCWD, filepath.Dir(root), &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return fmt.Errorf("open worker staging parent: %w", err)
	}
	defer unix.Close(parentFD)
	var owner unix.Stat_t
	if err = unix.Fstat(parentFD, &owner); err != nil {
		return err
	}
	if owner.Uid != uint32(os.Geteuid()) || owner.Mode&0777 != 0700 {
		return errors.New("worker staging parent must be worker-owned mode 0700")
	}
	if err = unix.Mkdirat(parentFD, "transfers", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	fd, err := unix.Openat2(parentFD, "transfers", &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0700 {
		return errors.New("worker staging must be worker-owned mode 0700")
	}
	return nil
}

func transferError(code, class, msg string) Response {
	return fileError(code, class, errors.New(msg))
}

func transferReceipt(r transferRecord) Response {
	out := Response{OK: true, Status: r.Status, TransferID: r.ID, ExpectedSize: r.ExpectedSize,
		TransferredBytes: r.Received, SHA256: r.SHA256,
		ExpiresAt:  time.Unix(r.ExpiresUnix, 0).UTC().Format(time.RFC3339),
		NextOffset: int64Ptr(r.Received),
	}
	if r.Status == "committed" {
		out.FileVersion = r.ResultVersion
		out.FileSize = int64Ptr(r.ExpectedSize)
		out.EOF = boolPtr(true)
	}
	return out
}

func transferReadRecord(session string) (transferRecord, error) {
	var r transferRecord
	fd, err := unix.Open(filepath.Join(session, "manifest.json"), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return r, err
	}
	f := os.NewFile(uintptr(fd), "manifest.json")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > transferManifestMax {
		return r, errors.New("invalid manifest")
	}
	b, err := io.ReadAll(io.LimitReader(f, transferManifestMax+1))
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	if !transferIDValid(r.ID) || !transferDigestValid(r.SHA256) || r.ExpectedSize <= 0 || r.Received < 0 || r.Received > r.ExpectedSize ||
		(r.Status != "uploading" && r.Status != "committing" && r.Status != "committed" && r.Status != "aborted") || r.ExpiresUnix <= 0 {
		return r, errors.New("invalid transfer manifest")
	}
	return r, nil
}

func transferSaveRecord(session string, r transferRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > transferManifestMax {
		return errors.New("transfer manifest too large")
	}
	dir, err := unix.Open(session, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	fd, name, err := createTempFileAt(dir)
	if err != nil {
		return err
	}
	owned := true
	defer func() {
		_ = unix.Close(fd)
		if owned {
			_ = unix.Unlinkat(dir, name, 0)
		}
	}()
	if err = writeAllFD(fd, b); err != nil {
		return err
	}
	if err = unix.Fsync(fd); err != nil {
		return err
	}
	if err = unix.Renameat(dir, name, dir, "manifest.json"); err != nil {
		return err
	}
	owned = false
	return unix.Fsync(dir)
}

func transferRemoveData(session string) error {
	dir, err := unix.Open(session, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err = unix.Unlinkat(dir, "data", 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(dir)
}
func transferRemoveSession(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 128 {
		return errors.New("unexpected number of files in transfer state")
	}
	for _, entry := range entries {
		name := entry.Name()
		prefix := ".ai-server-agent-write-"
		tmp := strings.HasPrefix(name, prefix) && len(name) == len(prefix)+24 && strings.Trim(name[len(prefix):], "0123456789abcdef") == ""
		if name != "data" && name != "manifest.json" && !tmp {
			return errors.New("unrecognized transfer state file")
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return errors.New("unsafe transfer state entry")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("non-regular transfer state entry")
		}
		if err = os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

func transferInventory(op workspaceFileOperation, now int64) (int, int64, error) {
	root := op.TransferRoot
	fd, err := os.Open(root)
	if err != nil {
		return 0, 0, err
	}
	names, err := fd.Readdirnames(transferScanMax + 1)
	_ = fd.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, err
	}
	if len(names) > transferScanMax {
		return 0, 0, errors.New("too many staging entries; operator cleanup required")
	}
	count := 0
	var reserved int64
	for _, name := range names {
		if !transferIDValid(name) {
			return 0, 0, errors.New("unexpected entry in managed transfer storage")
		}
		dir := filepath.Join(root, name)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return 0, 0, errors.New("unsafe staging directory")
		}
		rec, err := transferReadRecord(dir)
		if err != nil || rec.ID != name {
			if info.ModTime().Unix()+3600 > now {
				return 0, 0, errors.New("recent interrupted transfer begin requires recovery or expiration")
			}
			if err = transferRemoveSession(dir); err != nil {
				return 0, 0, fmt.Errorf("cleanup interrupted transfer begin: %w", err)
			}
			continue
		}
		if rec.ExpiresUnix <= now {
			cleanupOp := workspaceFileOperation{WorkspaceRoot: op.WorkspaceRoot, Workspace: rec.Workspace, Path: rec.Path, TransferOp: "status"}
			if rec.Status == "committing" {
				result := transferReconcileCommit(cleanupOp, dir, &rec)
				if !result.OK && result.ErrorCode != "transfer_commit_pending" {
					return 0, 0, errors.New("uncertain transfer publication requires operator reconciliation")
				}
				if result.ErrorCode == "transfer_commit_pending" {
					if e := transferDiscardPrepared(cleanupOp, rec); e != nil {
						return 0, 0, fmt.Errorf("uncertain transfer cannot be discarded safely: %w", e)
					}
				}
			} else if rec.Status == "uploading" || rec.Status == "aborted" {
				if err = transferCleanupUnpublishedTemp(cleanupOp, rec); err != nil {
					return 0, 0, fmt.Errorf("cleanup expired staged destination: %w", err)
				}
			}
			if err = transferRemoveSession(dir); err != nil {
				return 0, 0, fmt.Errorf("expire transfer safely: %w", err)
			}
			continue
		}
		if rec.Status == "uploading" || rec.Status == "committing" {
			count++
			reserved += rec.ExpectedSize
		}
	}
	return count, reserved, nil
}

// This function executes solely inside the aiworker/Landlock file helper.
// The global flock caps concurrent disk/commit activity and serializes
// durable progress between independent MCP calls and after restarts.
func workerWorkspaceTransfer(op workspaceFileOperation) Response {
	req := Request{Action: "workspace_transfer", Workspace: op.Workspace, Path: op.Path, PrincipalID: op.PrincipalID, PrincipalClass: op.PrincipalClass, TransferKey: op.TransferKey,
		TransferOp: op.TransferOp, TransferID: op.TransferID, ExpectedSize: op.ExpectedSize, SHA256: op.SHA256,
		ChunkBase64: op.ChunkBase64, ChunkSHA256: op.ChunkSHA256, Offset: op.Offset, FileVersion: op.FileVersion, MustNotExist: op.MustNotExist}
	if err := validateWorkspaceTransferRequest(req); err != nil {
		return fileError("invalid_transfer", "validation", err)
	}
	if op.TransferQuotaMiB <= 0 || op.TransferTTLSeconds <= 0 || op.TransferMaxSessions <= 0 || op.TransferMaxSessions > 64 {
		return transferError("invalid_transfer_config", "state", "valid operator transfer settings are required")
	}
	workspace, err := openWorkerWorkspace(op)
	if err != nil {
		return fileError("invalid_workspace", "validation", err)
	}
	_ = workspace.Close()
	rootFD, err := unix.Open(op.TransferRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fileError("transfer_storage_unavailable", "state", err)
	}
	defer unix.Close(rootFD)
	if err = unix.Flock(rootFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return transferError("transfer_busy", "capacity", "another transfer is in progress; retry the exact call")
		}
		return fileError("transfer_lock_failed", "io", err)
	}
	defer unix.Flock(rootFD, unix.LOCK_UN)
	now := time.Now().Unix()
	if op.TransferOp == "begin" {
		id := transferIDFor(op)
		session := filepath.Join(op.TransferRoot, id)
		if info, e := os.Lstat(session); e == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return transferError("transfer_storage_invalid", "state", "unsafe idempotent transfer session")
			}
			old, e := transferReadRecord(session)
			if e != nil {
				return transferError("transfer_begin_unknown", "state", "previous begin is incomplete; status may require expiry or operator cleanup")
			}
			if old.PrincipalID != op.PrincipalID || old.PrincipalClass != op.PrincipalClass || old.Workspace != filepath.Clean(op.Workspace) || old.Path != op.Path || old.ExpectedSize != op.ExpectedSize || old.SHA256 != op.SHA256 || old.FileVersion != op.FileVersion || old.MustNotExist != op.MustNotExist {
				return transferError("transfer_begin_conflict", "conflict", "idempotency key was reused for a different transfer")
			}
			return transferReceipt(old)
		} else if !errors.Is(e, os.ErrNotExist) {
			return fileError("transfer_storage_invalid", "state", e)
		}
		count, reserved, err := transferInventory(op, now)
		if err != nil {
			return fileError("transfer_storage_invalid", "state", err)
		}
		capBytes := int64(op.TransferQuotaMiB) << 20
		if count >= op.TransferMaxSessions || op.ExpectedSize > capBytes || reserved > capBytes-op.ExpectedSize {
			return transferError("transfer_quota_exceeded", "resource", "configured staging quota or session capacity exhausted; reduce active transfers or change operator settings")
		}
		return transferBegin(op, now)
	}
	session := filepath.Join(op.TransferRoot, op.TransferID)
	rec, err := transferReadRecord(session)
	if err != nil {
		return transferError("transfer_not_found", "state", "transfer unavailable, expired or incomplete")
	}
	if rec.ID != op.TransferID || rec.PrincipalID != op.PrincipalID || rec.PrincipalClass != op.PrincipalClass ||
		rec.Workspace != filepath.Clean(op.Workspace) || rec.Path != op.Path {
		return transferError("transfer_scope_mismatch", "policy", "transfer identity does not match authenticated principal, workspace and destination")
	}
	if rec.ExpiresUnix <= now && rec.Status != "committing" {
		return transferError("transfer_expired", "state", "transfer expired; begin a new upload")
	}
	switch op.TransferOp {
	case "status":
		if rec.Status == "committing" {
			return transferReconcileCommit(op, session, &rec)
		}
		return transferReceipt(rec)
	case "chunk":
		if rec.Status != "uploading" {
			return transferError("transfer_not_uploading", "state", "transfer is not accepting bytes; inspect status")
		}
		return transferChunk(op, session, &rec, now)
	case "commit":
		if rec.Status == "committing" {
			return transferReconcileCommit(op, session, &rec)
		}
		if rec.Status == "committed" {
			return transferReceipt(rec)
		}
		if rec.Status != "uploading" {
			return transferError("transfer_not_uploading", "state", "transfer aborted")
		}
		if rec.Received != rec.ExpectedSize {
			return transferError("transfer_incomplete", "validation", "all declared bytes must arrive before commit")
		}
		return transferCommit(op, session, &rec, now)
	case "abort":
		if rec.Status == "committed" || rec.Status == "committing" {
			return transferError("transfer_already_publishing", "state", "cannot abort a committed or uncertain atomic publish; check status")
		}
		if rec.Status == "uploading" {
			if err = transferCleanupUnpublishedTemp(op, rec); err != nil {
				return fileError("transfer_temp_cleanup_failed", "io", err)
			}
			rec.Status = "aborted"
			if err := transferSaveRecord(session, rec); err != nil {
				return fileError("transfer_state_write_failed", "io", err)
			}
			if err := transferRemoveData(session); err != nil {
				return fileError("transfer_cleanup_failed", "io", err)
			}
		}
		return transferReceipt(rec)
	}
	return transferError("invalid_transfer", "validation", "unsupported transfer operation")
}

func transferBegin(op workspaceFileOperation, now int64) Response {
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	_ = parent.Close()
	id := transferIDFor(op)
	session := filepath.Join(op.TransferRoot, id)
	if err := os.Mkdir(session, 0700); err != nil {
		return fileError("transfer_begin_failed", "io", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = transferRemoveSession(session)
		}
	}()
	data, err := os.OpenFile(filepath.Join(session, "data"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return fileError("transfer_begin_failed", "io", err)
	}
	if err = data.Sync(); err != nil {
		_ = data.Close()
		return fileError("transfer_begin_failed", "io", err)
	}
	_ = data.Close()
	r := transferRecord{ID: id, PrincipalID: op.PrincipalID, PrincipalClass: op.PrincipalClass,
		Workspace: filepath.Clean(op.Workspace), Path: op.Path, ExpectedSize: op.ExpectedSize,
		SHA256: op.SHA256, FileVersion: op.FileVersion, MustNotExist: op.MustNotExist,
		Status: "uploading", ExpiresUnix: now + int64(op.TransferTTLSeconds), TempName: transferTempName(id)}
	if err := transferSaveRecord(session, r); err != nil {
		return fileError("transfer_begin_failed", "io", err)
	}
	fd, err := os.Open(op.TransferRoot)
	if err != nil {
		return fileError("transfer_begin_failed", "io", err)
	}
	err = fd.Sync()
	_ = fd.Close()
	if err != nil {
		return fileError("transfer_begin_failed", "io", err)
	}
	complete = true
	return transferReceipt(r)
}

func openWorkerParentForTransfer(op workspaceFileOperation) (*os.File, error) {
	w, err := openWorkerWorkspace(op)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	parent, _, err := openWorkerParent(int(w.Fd()), op.Path)
	if err != nil {
		return nil, err
	}
	if err = rejectPseudoFilesystem(int(parent.Fd())); err != nil {
		_ = parent.Close()
		return nil, err
	}
	return parent, nil
}

func transferChunk(op workspaceFileOperation, session string, r *transferRecord, now int64) Response {
	raw, err := base64.StdEncoding.Strict().DecodeString(op.ChunkBase64)
	if err != nil || len(raw) == 0 || len(raw) > transferChunkMax {
		return transferError("invalid_chunk", "validation", "chunk is not valid, nonempty and bounded base64")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != op.ChunkSHA256 {
		return transferError("chunk_hash_mismatch", "conflict", "decoded chunk SHA256 does not match")
	}
	if op.Offset > r.Received || int64(len(raw)) > r.ExpectedSize-op.Offset {
		return transferError("transfer_offset_conflict", "conflict", "chunk must append at next_offset or replay an earlier complete chunk")
	}
	file, err := os.OpenFile(filepath.Join(session, "data"), os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fileError("transfer_storage_unavailable", "io", err)
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() < r.Received {
		return transferError("transfer_storage_corrupt", "state", "staged data does not match durable progress")
	}
	if st.Size() > r.Received {
		if err = file.Truncate(r.Received); err != nil {
			return fileError("transfer_recovery_failed", "io", err)
		}
		if err = file.Sync(); err != nil {
			return fileError("transfer_recovery_failed", "io", err)
		}
	}
	if op.Offset < r.Received {
		if int64(len(raw)) > r.Received-op.Offset {
			return transferError("transfer_offset_conflict", "conflict", "replay must exactly match a completed range")
		}
		old := make([]byte, len(raw))
		if _, err = file.ReadAt(old, op.Offset); err != nil {
			return fileError("transfer_replay_failed", "io", err)
		}
		if sha256.Sum256(old) != sum {
			return transferError("transfer_replay_conflict", "conflict", "repeated offset has different bytes")
		}
		return transferReceipt(*r)
	}
	var fs unix.Statfs_t
	if err = unix.Fstatfs(int(file.Fd()), &fs); err != nil {
		return fileError("transfer_disk_check_failed", "io", err)
	}
	if fs.Bsize <= 0 || fs.Bavail <= uint64((transferDiskReserve+len(raw))/int(fs.Bsize)+1) {
		return transferError("transfer_disk_full", "resource", "insufficient free disk for chunk plus safety reserve")
	}
	n, err := file.WriteAt(raw, r.Received)
	if err != nil || n != len(raw) {
		return transferError("transfer_chunk_io", "io", "chunk write incomplete; query status before retry")
	}
	if err = file.Sync(); err != nil {
		return fileError("transfer_chunk_io", "io", err)
	}
	r.Received += int64(len(raw))
	r.ExpiresUnix = now + int64(op.TransferTTLSeconds)
	if err = transferSaveRecord(session, *r); err != nil {
		return transferError("transfer_checkpoint_unknown", "state", "staged bytes may exist but durable progress is unknown; query status")
	}
	return transferReceipt(*r)
}

func transferDiscardPrepared(op workspaceFileOperation, r transferRecord) error {
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return err
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	current, exists, err := destinationState(pfd, filepath.Base(op.Path))
	if err != nil {
		return err
	}
	if r.MustNotExist && exists {
		return errors.New("destination unexpectedly exists")
	}
	if !r.MustNotExist && (!exists || fileVersion(current) != r.FileVersion || uint64(current.Dev) != r.DisplacedDev || current.Ino != r.DisplacedIno) {
		return errors.New("destination precondition no longer holds")
	}
	prepared, found, err := pathState(pfd, r.TempName)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if uint64(prepared.Dev) != r.PreparedDev || prepared.Ino != r.PreparedIno {
		return errors.New("prepared inode no longer matches durable intent")
	}
	return transferCleanupUnpublishedTemp(op, r)
}

func transferCleanupUnpublishedTemp(op workspaceFileOperation, r transferRecord) error {
	if !transferTempValid(r.TempName) || r.TempName != transferTempName(r.ID) {
		return errors.New("unsafe transfer temp name")
	}
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return err
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	st, exists, err := pathState(pfd, r.TempName)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) {
		return errors.New("unexpected temp file type, mode or owner; manual inspection required")
	}
	if err = unix.Unlinkat(pfd, r.TempName, 0); err != nil {
		return err
	}
	return unix.Fsync(pfd)
}

func transferCommit(op workspaceFileOperation, session string, r *transferRecord, now int64) Response {
	data, err := os.OpenFile(filepath.Join(session, "data"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fileError("transfer_storage_unavailable", "io", err)
	}
	defer data.Close()
	st, err := data.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != r.ExpectedSize {
		return transferError("transfer_storage_corrupt", "state", "staged byte count differs from expected complete transfer")
	}
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	base := filepath.Base(op.Path)
	old, exists, err := destinationState(pfd, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if r.MustNotExist && exists {
		return transferError("file_exists", "conflict", "destination already exists")
	}
	if !r.MustNotExist && (!exists || fileVersion(old) != r.FileVersion) {
		return transferError("file_changed", "conflict", "destination file_version has changed")
	}
	// The temp name is journaled in begin's durable record. A killed helper
	// can leave a partial, never-published copy: retry/expiry locates ONLY this
	// transfer's exact name, and does not unlink unrelated workspace files.
	if err := transferCleanupUnpublishedTemp(op, *r); err != nil {
		return fileError("transfer_temp_recovery_failed", "state", err)
	}
	tempName := r.TempName
	tempFD, err := unix.Openat(pfd, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fileError("transfer_commit_io", "io", err)
	}
	owned := true
	defer func() {
		_ = unix.Close(tempFD)
		if owned {
			_ = unix.Unlinkat(pfd, tempName, 0)
		}
	}()
	mode := uint32(0644)
	if exists {
		mode = old.Mode & 0777
	}
	temp := os.NewFile(uintptr(tempFD), "prepared transfer")
	hasher := sha256.New()
	copied, err := io.CopyBuffer(io.MultiWriter(temp, hasher), data, make([]byte, 64<<10))
	if err != nil || copied != r.ExpectedSize {
		return transferError("transfer_commit_io", "io", "staged byte stream could not be copied completely")
	}
	if hex.EncodeToString(hasher.Sum(nil)) != r.SHA256 {
		return transferError("transfer_hash_mismatch", "conflict", "whole-object SHA256 verification failed; staged bytes were not published")
	}
	if err = unix.Fchmod(tempFD, mode); err != nil {
		return fileError("transfer_commit_io", "io", err)
	}
	if err = unix.Fsync(tempFD); err != nil {
		return fileError("transfer_commit_io", "io", err)
	}
	var prepared unix.Stat_t
	if err = unix.Fstat(tempFD, &prepared); err != nil {
		return fileError("transfer_commit_io", "io", err)
	}
	current, currentExists, err := destinationState(pfd, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if exists && (!currentExists || !sameFileSnapshot(old, current)) {
		return transferError("file_changed", "conflict", "destination changed during staged copying")
	}
	if !exists && currentExists {
		return transferError("file_changed", "conflict", "destination appeared during staged copying")
	}
	r.Status = "committing"
	r.ExpiresUnix = now + int64(op.TransferTTLSeconds)
	r.PreparedDev = uint64(prepared.Dev)
	r.PreparedIno = prepared.Ino
	r.TempName = tempName
	if exists {
		r.DisplacedDev = uint64(old.Dev)
		r.DisplacedIno = old.Ino
	}
	if err = transferSaveRecord(session, *r); err != nil {
		return fileError("transfer_checkpoint_failed", "io", err)
	}
	// A durable recovery intent now owns the prepared temp file. Even if this
	// helper crashes, a later commit/status can distinguish before and after
	// the atomic rename by the pinned prepared inode identity.
	owned = false
	return transferFinishPrepared(op, session, r)
}

func transferStateAtCommit(pfd int, base string, r transferRecord) (unix.Stat_t, bool, error) {
	current, exists, err := destinationState(pfd, base)
	if err != nil {
		return current, exists, err
	}
	if r.MustNotExist {
		if exists {
			return current, exists, errors.New("destination appeared before staged commit")
		}
		return current, false, nil
	}
	if !exists || fileVersion(current) != r.FileVersion ||
		uint64(current.Dev) != r.DisplacedDev || current.Ino != r.DisplacedIno {
		return current, exists, errors.New("destination changed before staged commit")
	}
	return current, true, nil
}

func transferFinishPrepared(op workspaceFileOperation, session string, r *transferRecord) Response {
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	base := filepath.Base(op.Path)
	if !transferTempValid(r.TempName) || r.TempName != transferTempName(r.ID) {
		return transferError("transfer_commit_unresolved", "state", "durable commit temp identity is missing")
	}
	prepared, exists, err := pathState(pfd, r.TempName)
	if err != nil || !exists || uint64(prepared.Dev) != r.PreparedDev || prepared.Ino != r.PreparedIno {
		return transferError("transfer_commit_unresolved", "state", "prepared transfer inode is unavailable; inspect destination and status")
	}
	old, existed, err := transferStateAtCommit(pfd, base, *r)
	if err != nil {
		return transferError("file_changed", "conflict", "destination changed since commit intent; no data published by this retry")
	}
	if existed {
		if err = unix.Renameat2(pfd, r.TempName, pfd, base, unix.RENAME_EXCHANGE); err != nil {
			return fileError("transfer_commit_io", "io", err)
		}
		displaced, found, e := pathState(pfd, r.TempName)
		if e != nil || !found {
			return transferError("transfer_commit_unresolved", "state", "displaced destination identity could not be verified; query status")
		}
		if !sameFileCommitState(old, displaced) {
			if rollbackErr := rollbackExistingExchange(pfd, base, r.TempName, prepared, displaced); rollbackErr != nil {
				return transferError("transfer_commit_unresolved", "state", "concurrent exchange conflict; rollback unproven")
			}
			return transferError("file_changed", "conflict", "destination changed during atomic exchange; original restored")
		}
		if err = unix.Unlinkat(pfd, r.TempName, 0); err != nil {
			return transferError("transfer_commit_unresolved", "state", "displaced file cleanup failed; inspect durable commit status")
		}
	} else {
		if err = unix.Renameat2(pfd, r.TempName, pfd, base, unix.RENAME_NOREPLACE); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return transferError("file_exists", "conflict", "new destination appeared during atomic publish")
			}
			return fileError("transfer_commit_io", "io", err)
		}
	}
	return transferConfirmPublished(op, session, r, pfd, base)
}

func transferConfirmPublished(op workspaceFileOperation, session string, r *transferRecord, pfd int, base string) Response {
	final, exists, err := destinationState(pfd, base)
	if err != nil || !exists || uint64(final.Dev) != r.PreparedDev || final.Ino != r.PreparedIno || final.Size != r.ExpectedSize {
		return transferError("transfer_commit_unresolved", "state", "published file identity could not be verified; query status")
	}
	if err = unix.Fsync(pfd); err != nil {
		return transferError("transfer_commit_unresolved", "state", "published directory sync failed; query status")
	}
	final, exists, err = destinationState(pfd, base)
	if err != nil || !exists || uint64(final.Dev) != r.PreparedDev || final.Ino != r.PreparedIno {
		return transferError("transfer_commit_unresolved", "state", "destination changed after commit; inspect before retry")
	}
	r.Status = "committed"
	r.ResultVersion = fileVersion(final)
	if err = transferSaveRecord(session, *r); err != nil {
		return transferError("transfer_commit_unresolved", "state", "file may be published but durable receipt failed; query status")
	}
	if err = transferRemoveData(session); err != nil {
		// The durable committed receipt is already written; cleanup is retryable
		// through status/expiry, never re-publish the target.
		return transferError("transfer_cleanup_failed", "io", "file committed but staged bytes could not be released")
	}
	return transferReceipt(*r)
}

// A recovered committing state is not evidence that target bytes were
// published. Compare pinned inode IDs to avoid blind duplicate publication.
func transferReconcileCommit(op workspaceFileOperation, session string, r *transferRecord) Response {
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	base := filepath.Base(op.Path)
	final, exists, err := destinationState(pfd, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if exists && uint64(final.Dev) == r.PreparedDev && final.Ino == r.PreparedIno && final.Size == r.ExpectedSize {
		if r.TempName != "" {
			displaced, found, e := pathState(pfd, r.TempName)
			if e != nil {
				return transferError("transfer_commit_unresolved", "state", "displaced-file identity check failed")
			}
			if found {
				if uint64(displaced.Dev) != r.DisplacedDev || displaced.Ino != r.DisplacedIno {
					return transferError("transfer_commit_unresolved", "state", "unexpected data at staged commit temp; never delete blindly")
				}
				if e = unix.Unlinkat(pfd, r.TempName, 0); e != nil {
					return fileError("transfer_cleanup_failed", "io", e)
				}
			}
		}
		return transferConfirmPublished(op, session, r, pfd, base)
	}
	if op.TransferOp == "commit" {
		// Lost response before atomic publication. The still-pinned prepared
		// file and original destination precondition permit an exact safe retry.
		return transferFinishPrepared(op, session, r)
	}
	return transferError("transfer_commit_pending", "state", "not yet proven published; use commit with this same handle or inspect destination")
}
