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
	"syscall"
	"time"
)

// Fault injection is nil in production and is set only by isolated tests.
// Each injected crash is a real os.Exit in a disposable child process.
var transferFaultHook func(string)

func transferFaultPoint(phase string) {
	if transferFaultHook != nil {
		transferFaultHook(phase)
	}
}

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
	PreparedBytes  int64  `json:"prepared_bytes,omitempty"`
	PreparedHash   string `json:"prepared_hash,omitempty"`
	StagedVersion  string `json:"staged_version,omitempty"`
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
	return len(v) == 67 && strings.HasPrefix(v, "tr_") && strings.Trim(v[3:], "0123456789abcdef") == ""
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
	return len(name) == len(prefix)+64 && strings.HasPrefix(name, prefix) && strings.Trim(name[len(prefix):], "0123456789abcdef") == ""
}

func transferIDFor(op workspaceFileOperation) string {
	keyInput, _ := json.Marshal([]string{op.PrincipalClass, op.PrincipalID, filepath.Clean(op.Workspace), op.Path, op.TransferKey})
	materialInput, _ := json.Marshal([]any{op.ExpectedSize, op.SHA256, op.FileVersion, op.MustNotExist})
	keyHash, materialHash := sha256.Sum256(keyInput), sha256.Sum256(materialInput)
	return "tr_" + hex.EncodeToString(keyHash[:16]) + hex.EncodeToString(materialHash[:16])
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
		NextOffset: int64Ptr(r.Received), PreparedBytes: r.PreparedBytes,
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
	if !transferIDValid(r.ID) || !transferDigestValid(r.SHA256) || r.ExpectedSize <= 0 || r.Received < 0 || r.Received > r.ExpectedSize || r.ExpectedSize > (int64(1)<<40) || r.PreparedBytes < 0 || r.PreparedBytes > r.ExpectedSize ||
		(r.Status != "uploading" && r.Status != "preparing" && r.Status != "committing" && r.Status != "committed" && r.Status != "aborted") || r.ExpiresUnix <= 0 {
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

// Reconcile terminal cleanup on every retry; a durable receipt must never
// allow a leftover full staging file to evade quota accounting.
func transferTerminalReceipt(session string, r *transferRecord) Response {
	if r.Status != "aborted" && r.Status != "committed" {
		return transferError("transfer_invalid_state", "state", "not a terminal record")
	}
	if err := transferRemoveData(session); err != nil {
		return fileError("transfer_cleanup_failed", "io", err)
	}
	return transferReceipt(*r)
}

func transferRemoveSession(dir string) error {
	var current unix.Stat_t
	if err := unix.Lstat(dir, &current); err != nil {
		return err
	}
	if current.Mode&unix.S_IFMT != unix.S_IFDIR || current.Mode&0777 != 0700 || current.Uid != uint32(os.Geteuid()) {
		return errors.New("unexpected transfer session directory ownership or type")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 128 {
		return errors.New("unexpected number of files in transfer state")
	}
	// Validate the COMPLETE namespace before removing a single entry.
	for _, entry := range entries {
		name := entry.Name()
		const prefix = ".ai-server-agent-write-"
		tmp := strings.HasPrefix(name, prefix) && len(name) == len(prefix)+24 && strings.Trim(name[len(prefix):], "0123456789abcdef") == ""
		if name != "data" && name != "manifest.json" && !tmp {
			return errors.New("unrecognized transfer state file")
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return errors.New("unsafe transfer state entry")
		}
		info, e := entry.Info()
		if e != nil || !info.Mode().IsRegular() {
			return errors.New("non-regular transfer state entry")
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
			return errors.New("transfer state entry is not worker-owned")
		}
	}
	for _, entry := range entries {
		if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

// A helper killed during mkdir/data creation can leave a directory without
// a committed intent. Since the transfer ID embeds exact key+material and
// the serialized staging lock excludes concurrent helpers, only an empty,
// manifest-less partial begin may be reclaimed without destroying user bytes.
func transferIncompleteBeginSafe(session string) bool {
	if _, err := os.Lstat(filepath.Join(session, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	data, err := os.Lstat(filepath.Join(session, "data"))
	return errors.Is(err, os.ErrNotExist) || (err == nil && data.Mode().IsRegular() && data.Size() == 0)
}

func transferInventory(op workspaceFileOperation, now int64) (int, int64, int, error) {
	root := op.TransferRoot
	fd, err := os.Open(root)
	if err != nil {
		return 0, 0, 0, err
	}
	names, err := fd.Readdirnames(transferScanMax + 1)
	_ = fd.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, 0, err
	}
	if len(names) > transferScanMax {
		return 0, 0, 0, errors.New("too many staging entries; operator cleanup required")
	}
	count := 0
	retained := 0
	var reserved int64
	for _, name := range names {
		if !transferIDValid(name) {
			return 0, 0, 0, errors.New("unexpected entry in managed transfer storage")
		}
		dir := filepath.Join(root, name)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return 0, 0, 0, errors.New("unsafe staging directory")
		}
		rec, err := transferReadRecord(dir)
		if err != nil || rec.ID != name {
			if !transferIncompleteBeginSafe(dir) {
				return 0, 0, 0, errors.New("unsafe or corrupt durable transfer requires investigation")
			}
			if err = transferRemoveSession(dir); err != nil {
				return 0, 0, 0, fmt.Errorf("cleanup interrupted transfer begin: %w", err)
			}
			continue
		}
		if rec.ExpiresUnix <= now {
			cleanupOp := workspaceFileOperation{WorkspaceRoot: op.WorkspaceRoot, Workspace: rec.Workspace, Path: rec.Path, TransferOp: "status"}
			if rec.Status == "committing" {
				result := transferReconcileCommit(cleanupOp, dir, &rec)
				if !result.OK && result.ErrorCode != "transfer_commit_pending" {
					return 0, 0, 0, errors.New("uncertain transfer publication requires operator reconciliation")
				}
				if result.ErrorCode == "transfer_commit_pending" {
					if e := transferDiscardPrepared(cleanupOp, rec); e != nil {
						return 0, 0, 0, fmt.Errorf("uncertain transfer cannot be discarded safely: %w", e)
					}
				}
			} else if rec.Status == "uploading" || rec.Status == "preparing" || rec.Status == "aborted" {
				if err = transferCleanupUnpublishedTemp(cleanupOp, rec); err != nil {
					return 0, 0, 0, fmt.Errorf("cleanup expired staged destination: %w", err)
				}
			}
			if err = transferRemoveData(dir); err != nil {
				return 0, 0, 0, fmt.Errorf("cleanup terminal/expired staging bytes: %w", err)
			}
			if err = transferRemoveSession(dir); err != nil {
				return 0, 0, 0, fmt.Errorf("expire transfer safely: %w", err)
			}
			continue
		}
		retained++
		if rec.Status == "uploading" || rec.Status == "preparing" || rec.Status == "committing" {
			count++
			// A later operator quota reduction blocks NEW admissions, not
			// recovery or expiry cleanup of already accepted objects.
			reserved += rec.ExpectedSize
		} else {
			// A terminal receipt is NOT evidence that its staged bytes were
			// physically removed. Count the actual retained file until unlink.
			st, e := os.Lstat(filepath.Join(dir, "data"))
			if e == nil {
				if !st.Mode().IsRegular() {
					return 0, 0, 0, errors.New("terminal staging has unsafe file type")
				}
				if st.Size() < 0 || st.Size() > (int64(1)<<40) {
					return 0, 0, 0, errors.New("terminal staging exceeds supported maximum size")
				}
				reserved += st.Size()
			} else if !errors.Is(e, os.ErrNotExist) {
				return 0, 0, 0, e
			}
		}
	}
	return count, reserved, retained, nil
}

// This function executes solely inside the aiworker/Landlock file helper.
// The global flock caps concurrent disk/commit activity and serializes
// durable progress between independent MCP calls and after restarts.
// The directory name contains both the idempotency-key hash and material
// fingerprint. It is therefore safe to reclaim a same-material *incomplete*
// begin that never published a manifest, without accepting a different
// material request at the same key or requiring another transfer to sweep.
func transferResolveBegin(root string, op workspaceFileOperation) (Response, bool) {
	id := transferIDFor(op)
	listing, err := os.Open(root)
	if err != nil {
		return fileError("transfer_storage_invalid", "state", err), true
	}
	names, err := listing.Readdirnames(transferScanMax + 1)
	_ = listing.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return fileError("transfer_storage_invalid", "state", err), true
	}
	if len(names) > transferScanMax {
		return transferError("transfer_receipt_capacity", "resource", "bounded transfer namespace full; automatic expiry sweep runs independently"), true
	}
	for _, name := range names {
		if strings.HasPrefix(name, id[:35]) && name != id {
			return transferError("transfer_begin_conflict", "conflict", "same transfer key already reserved for a different material request"), true
		}
	}
	session := filepath.Join(root, id)
	info, err := os.Lstat(session)
	if errors.Is(err, os.ErrNotExist) {
		return Response{}, false
	}
	if err != nil {
		return fileError("transfer_storage_invalid", "state", err), true
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return transferError("transfer_storage_invalid", "state", "unsafe existing transfer session"), true
	}
	var stat unix.Stat_t
	if e := unix.Lstat(session, &stat); e != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0700 {
		return transferError("transfer_storage_invalid", "state", "transfer state owner or mode invalid"), true
	}
	prior, e := transferReadRecord(session)
	if e == nil {
		if prior.ID != id || prior.PrincipalID != op.PrincipalID || prior.PrincipalClass != op.PrincipalClass || prior.Workspace != filepath.Clean(op.Workspace) || prior.Path != op.Path || prior.ExpectedSize != op.ExpectedSize || prior.SHA256 != op.SHA256 || prior.FileVersion != op.FileVersion || prior.MustNotExist != op.MustNotExist {
			return transferError("transfer_begin_conflict", "conflict", "idempotency key and durable material do not match"), true
		}
		if prior.ExpiresUnix <= time.Now().Unix() {
			return transferError("transfer_expired", "state", "idempotent transfer has expired; use a new key after retention cleanup"), true
		}
		if prior.Status == "committed" || prior.Status == "aborted" {
			return transferTerminalReceipt(session, &prior), true
		}
		return transferReceipt(prior), true
	}
	// Corrupt accepted sessions with nonempty data remain fail-closed.
	if !transferIncompleteBeginSafe(session) {
		return transferError("transfer_begin_unknown", "state", "existing durable transfer may contain user bytes; refuse unsafe reclamation"), true
	}
	if e = transferRemoveSession(session); e != nil {
		return fileError("transfer_begin_recovery_failed", "io", e), true
	}
	fd, e := os.Open(root)
	if e != nil {
		return fileError("transfer_begin_recovery_failed", "io", e), true
	}
	e = fd.Sync()
	_ = fd.Close()
	if e != nil {
		return fileError("transfer_begin_recovery_failed", "io", e), true
	}
	return Response{}, false
}

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
		if result, found := transferResolveBegin(op.TransferRoot, op); found {
			return result
		}
		count, reserved, retained, err := transferInventory(op, now)
		if err != nil {
			return fileError("transfer_storage_invalid", "state", err)
		}
		capBytes := int64(op.TransferQuotaMiB) << 20
		if retained >= transferScanMax {
			return transferError("transfer_receipt_capacity", "resource", "staging receipt namespace is full; bounded automatic expiry cleanup will free entries")
		}
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
	if rec.ExpiresUnix <= now && rec.Status != "committing" && rec.Status != "committed" && rec.Status != "aborted" {
		return transferError("transfer_expired", "state", "transfer expired; begin a new upload")
	}
	switch op.TransferOp {
	case "status":
		if rec.Status == "committed" || rec.Status == "aborted" {
			return transferTerminalReceipt(session, &rec)
		}
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
			return transferTerminalReceipt(session, &rec)
		}
		if rec.Status == "preparing" {
			return transferPrepareStep(op, session, &rec)
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
			if rec.Status == "committed" {
				if cleaned := transferTerminalReceipt(session, &rec); !cleaned.OK {
					return cleaned
				}
			}
			return transferError("transfer_already_publishing", "state", "cannot abort a committed or uncertain atomic publish; check status")
		}
		if rec.Status == "aborted" {
			return transferTerminalReceipt(session, &rec)
		}
		if rec.Status == "uploading" || rec.Status == "preparing" {
			if err = transferCleanupUnpublishedTemp(op, rec); err != nil {
				return fileError("transfer_temp_cleanup_failed", "io", err)
			}
			rec.Status = "aborted"
			if err := transferSaveRecord(session, rec); err != nil {
				return fileError("transfer_state_write_failed", "io", err)
			}
			transferFaultPoint("aborted_after_checkpoint")
		}
		return transferTerminalReceipt(session, &rec)
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
	transferFaultPoint("begin_after_mkdir")
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
	transferFaultPoint("begin_after_data")
	r := transferRecord{ID: id, PrincipalID: op.PrincipalID, PrincipalClass: op.PrincipalClass,
		Workspace: filepath.Clean(op.Workspace), Path: op.Path, ExpectedSize: op.ExpectedSize,
		SHA256: op.SHA256, FileVersion: op.FileVersion, MustNotExist: op.MustNotExist,
		Status: "uploading", ExpiresUnix: now + int64(op.TransferTTLSeconds), TempName: transferTempName(id)}
	if err := transferSaveRecord(session, r); err != nil {
		return fileError("transfer_begin_failed", "io", err)
	}
	transferFaultPoint("begin_after_manifest")
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
	if r.Status == "preparing" && (uint64(st.Dev) != r.PreparedDev || st.Ino != r.PreparedIno) {
		return errors.New("pinned preparation identity mismatch; refuse unsafe deletion")
	}
	if err = unix.Unlinkat(pfd, r.TempName, 0); err != nil {
		return err
	}
	return unix.Fsync(pfd)
}

// Commit begins durable, bounded preparation; subsequent commit calls resume
// at prepared_bytes without repeating already checkpointed copy/hash work.
func transferCommit(op workspaceFileOperation, session string, r *transferRecord, _ int64) Response {
	return transferStartPreparation(op, session, r)
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
	transferFaultPoint("published_before_receipt")
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
	transferFaultPoint("committed_after_checkpoint")
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
