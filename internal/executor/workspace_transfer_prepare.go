package executor

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// Each commit request does a finite amount of preparation, regardless of total
// file size. No single timeout bounds the total authorized transfer size.
const transferPrepareWindowBytes int64 = 4 << 20

func transferSHAState(h interface{ Sum([]byte) []byte }) (string, error) {
	marshaler, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return "", errors.New("SHA256 state serialization is unavailable")
	}
	state, err := marshaler.MarshalBinary()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(state), nil
}

func transferLoadSHAState(encoded string) (interface {
	io.Writer
	Sum([]byte) []byte
}, error) {
	h := sha256.New()
	b, err := hex.DecodeString(encoded)
	if err != nil || len(b) > 256 {
		return nil, errors.New("invalid SHA256 checkpoint")
	}
	unmarshal, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, errors.New("SHA256 state restoration is unavailable")
	}
	if err = unmarshal.UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("corrupt SHA256 checkpoint: %w", err)
	}
	return h, nil
}

func transferStageStat(session string) (*os.File, unix.Stat_t, error) {
	f, err := os.OpenFile(session+"/data", os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, unix.Stat_t{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = f.Close()
		return nil, unix.Stat_t{}, errors.New("staged data must be regular")
	}
	return f, st, nil
}

func transferStartPreparation(op workspaceFileOperation, session string, r *transferRecord) Response {
	source, st, err := transferStageStat(session)
	if err != nil {
		return fileError("transfer_storage_unavailable", "io", err)
	}
	defer source.Close()
	if st.Size != r.ExpectedSize {
		return transferError("transfer_storage_corrupt", "state", "staged byte count changed before preparation")
	}
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	base := filepath.Base(op.Path)
	original, exists, err := destinationState(pfd, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if r.MustNotExist && exists {
		return transferError("file_exists", "conflict", "destination already exists")
	}
	if !r.MustNotExist && (!exists || fileVersion(original) != r.FileVersion) {
		return transferError("file_changed", "conflict", "destination changed before preparation")
	}
	if err = transferCleanupUnpublishedTemp(op, *r); err != nil {
		return fileError("transfer_temp_recovery_failed", "state", err)
	}
	// Allocate an UNNAMED inode in the destination filesystem. The kernel
	// discards it on helper death before journal persistence, eliminating the
	// unidentifiable named-temp crash window. Never unlink an inode based on
	// a deterministic filename and matching Unix UID alone.
	p, err := unix.Openat(pfd, ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fileError("transfer_prepare_unavailable", "resource", fmt.Errorf("destination filesystem must support unnamed temporary files: %w", err))
	}
	defer unix.Close(p)
	mode := uint32(0644)
	if exists {
		mode = original.Mode & 0777
	}
	if err = unix.Fchmod(p, mode); err != nil {
		return fileError("transfer_prepare_failed", "io", err)
	}
	if err = unix.Fsync(p); err != nil {
		return fileError("transfer_prepare_failed", "io", err)
	}
	transferFaultPoint("prepare_temp_before_checkpoint")
	var prepared unix.Stat_t
	if err = unix.Fstat(p, &prepared); err != nil {
		return fileError("transfer_prepare_failed", "io", err)
	}
	hashState, err := transferSHAState(sha256.New())
	if err != nil {
		return fileError("transfer_prepare_failed", "state", err)
	}
	r.StagedVersion = fileVersion(st)
	r.PreparedDev = uint64(prepared.Dev)
	r.PreparedIno = prepared.Ino
	r.PreparedBytes = 0
	r.PreparedHash = hashState
	r.Status = "preparing"
	if exists {
		r.DisplacedDev = uint64(original.Dev)
		r.DisplacedIno = original.Ino
		r.DisplacedSnapshot = transferDisplacedDigest(original)
	}
	r.ExpiresUnix = time.Now().Unix() + int64(op.TransferTTLSeconds)
	if err = transferSaveRecord(session, *r); err != nil {
		return transferError("transfer_checkpoint_unknown", "state", "preparation journal may have persisted; retry status")
	}
	transferFaultPoint("prepare_checkpoint_before_link")
	// Only after an fsynced journal binds this exact dev/ino may its
	// destination-side pathname become visible. Direct AT_EMPTY_PATH is
	// preferred; /proc/self/fd works on kernels/filesystems which reject the
	// direct form for O_TMPFILE, without changing worker authority.
	err = unix.Linkat(p, "", pfd, r.TempName, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EPERM) {
		err = unix.Linkat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(p), pfd, r.TempName, unix.AT_SYMLINK_FOLLOW)
	}
	if err != nil {
		return fileError("transfer_prepare_link_failed", "io", fmt.Errorf("failed to link durably journaled prepared inode: %w", err))
	}
	if err = unix.Fsync(pfd); err != nil {
		return fileError("transfer_prepare_link_failed", "io", err)
	}
	transferFaultPoint("prepare_link_after_checkpoint")
	return transferPrepareStep(op, session, r)
}

// The copied range and SHA256 continuation are fsynced *before* their
// progress manifest. After a crash, any unacknowledged temp-file tail is
// truncated to the last committed offset; old bytes are never recopied.
func transferPrepareStep(op workspaceFileOperation, session string, r *transferRecord) Response {
	if r.Status != "preparing" {
		return transferError("transfer_invalid_state", "state", "preparation checkpoint not active")
	}
	if r.PreparedBytes < 0 || r.PreparedBytes > r.ExpectedSize || r.StagedVersion == "" {
		return transferError("transfer_storage_corrupt", "state", "invalid preparation checkpoint")
	}
	source, srcStat, err := transferStageStat(session)
	if err != nil {
		return fileError("transfer_storage_unavailable", "io", err)
	}
	defer source.Close()
	if srcStat.Size != r.ExpectedSize || fileVersion(srcStat) != r.StagedVersion {
		return transferError("transfer_storage_corrupt", "state", "staged object changed during finalization")
	}
	parent, err := openWorkerParentForTransfer(op)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	base := filepath.Base(op.Path)
	if _, _, err = transferStateAtCommit(pfd, base, *r); err != nil {
		return transferError("file_changed", "conflict", "destination precondition changed before continuation")
	}
	if !transferTempValid(r.TempName) || r.TempName != transferTempName(r.ID) {
		return transferError("transfer_storage_corrupt", "state", "invalid pinned preparation name")
	}
	fd, err := unix.Openat(pfd, r.TempName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) && r.PreparedBytes == 0 {
		// Crash after journaling the unnamed inode but before linking it:
		// kernel closed/reclaimed the unlinked inode on helper death.
		// A preparing state has never been published, so restart safely
		// after writing an uploading checkpoint. No named object is deleted.
		r.Status = "uploading"
		r.PreparedDev, r.PreparedIno, r.DisplacedDev, r.DisplacedIno = 0, 0, 0, 0
		r.PreparedHash, r.StagedVersion = "", ""
		r.ExpiresUnix = time.Now().Unix() + int64(op.TransferTTLSeconds)
		if e := transferSaveRecord(session, *r); e != nil {
			return fileError("transfer_prepare_recovery_failed", "io", e)
		}
		return transferStartPreparation(op, session, r)
	}
	if err != nil {
		return fileError("transfer_prepare_unavailable", "state", err)
	}
	destination := os.NewFile(uintptr(fd), "prepared transfer")
	defer destination.Close()
	var pinned unix.Stat_t
	if err = unix.Fstat(fd, &pinned); err != nil {
		return fileError("transfer_prepare_unavailable", "io", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG || uint64(pinned.Dev) != r.PreparedDev || pinned.Ino != r.PreparedIno {
		return transferError("transfer_prepare_unresolved", "state", "prepared file identity changed; inspect before retry")
	}
	if pinned.Size < r.PreparedBytes {
		return transferError("transfer_prepare_unresolved", "state", "prepared bytes missing after restart")
	}
	if pinned.Size > r.PreparedBytes {
		if err = unix.Ftruncate(fd, r.PreparedBytes); err != nil {
			return fileError("transfer_prepare_recovery_failed", "io", err)
		}
		if err = unix.Fsync(fd); err != nil {
			return fileError("transfer_prepare_recovery_failed", "io", err)
		}
	}
	hasher, err := transferLoadSHAState(r.PreparedHash)
	if err != nil {
		return fileError("transfer_storage_corrupt", "state", err)
	}
	left := r.ExpectedSize - r.PreparedBytes
	if left > 0 {
		n := left
		if n > transferPrepareWindowBytes {
			n = transferPrepareWindowBytes
		}
		var fs unix.Statfs_t
		if err = unix.Fstatfs(fd, &fs); err != nil {
			return fileError("transfer_disk_check_failed", "io", err)
		}
		if fs.Bsize <= 0 || fs.Bavail <= uint64((transferDiskReserve+n)/int64(fs.Bsize)+1) {
			return transferError("transfer_disk_full", "resource", "insufficient destination disk for bounded preparation window")
		}
		if _, err = unix.Seek(fd, r.PreparedBytes, io.SeekStart); err != nil {
			return fileError("transfer_prepare_io", "io", err)
		}
		section := io.NewSectionReader(source, r.PreparedBytes, n)
		copied, copyErr := io.CopyBuffer(io.MultiWriter(destination, hasher), section, make([]byte, 64<<10))
		if copyErr != nil || copied != n {
			return transferError("transfer_prepare_io", "io", "preparation window incomplete; retry from last durable prepared_bytes")
		}
		if err = unix.Fsync(fd); err != nil {
			return fileError("transfer_prepare_io", "io", err)
		}
		transferFaultPoint("prepare_window_before_checkpoint")
		nextState, e := transferSHAState(hasher)
		if e != nil {
			return fileError("transfer_prepare_io", "state", e)
		}
		r.PreparedBytes += n
		r.PreparedHash = nextState
		r.ExpiresUnix = time.Now().Unix() + int64(op.TransferTTLSeconds)
		if err = transferSaveRecord(session, *r); err != nil {
			return transferError("transfer_checkpoint_unknown", "state", "prepared bytes may be newer than durable checkpoint; inspect status")
		}
		transferFaultPoint("prepare_window_after_checkpoint")
	}
	if r.PreparedBytes < r.ExpectedSize {
		return transferReceipt(*r)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != r.SHA256 {
		return transferError("transfer_hash_mismatch", "conflict", "whole-object SHA256 differs; no destination published")
	}
	if err = unix.Fsync(fd); err != nil {
		return fileError("transfer_prepare_io", "io", err)
	}
	if _, _, err = transferStateAtCommit(pfd, base, *r); err != nil {
		return transferError("file_changed", "conflict", "destination changed before durable publication checkpoint")
	}
	r.Status = "committing"
	r.ExpiresUnix = time.Now().Unix() + int64(op.TransferTTLSeconds)
	if err = transferSaveRecord(session, *r); err != nil {
		return transferError("transfer_checkpoint_unknown", "state", "commit intent may have persisted; query status before retry")
	}
	transferFaultPoint("committing_after_checkpoint")
	return transferFinishPrepared(op, session, r)
}
