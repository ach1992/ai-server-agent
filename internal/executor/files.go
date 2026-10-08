package executor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/policy"
	"golang.org/x/sys/unix"
)

const (
	maxFileReadBytes    = 1 << 20
	maxFileWriteBytes   = 1 << 20
	maxFileEncodedBytes = 2 << 20
)

func fileError(code, class string, err error) Response {
	return Response{
		Error:      err.Error(),
		ReasonCode: code,
		ErrorCode:  code,
		ErrorClass: class,
	}
}

func fileApprovalResponse(dec policy.Decision) Response {
	return Response{
		Error:      "approval_required",
		ReasonCode: "approval_required",
		ErrorCode:  "approval_required",
		ErrorClass: "approval",
		Approval:   dec,
	}
}

func validateFilePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("file path must be absolute")
	}
	clean := filepath.Clean(path)
	if clean == "/" || filepath.Base(clean) == "." {
		return "", errors.New("file path must name a file")
	}
	return clean, nil
}

func openFileParent(path string) (*os.File, string, string, error) {
	clean, err := validateFilePath(path)
	if err != nil {
		return nil, "", "", err
	}
	parent := filepath.Dir(clean)
	base := filepath.Base(clean)
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, parent, how)
	if err != nil {
		return nil, "", "", fmt.Errorf("open file parent: %w", err)
	}
	f := os.NewFile(uintptr(fd), parent)
	if f == nil {
		_ = unix.Close(fd)
		return nil, "", "", errors.New("open file parent: invalid directory descriptor")
	}
	resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		_ = f.Close()
		return nil, "", "", fmt.Errorf("resolve file parent: %w", err)
	}
	if strings.HasSuffix(resolved, " (deleted)") || !filepath.IsAbs(resolved) {
		_ = f.Close()
		return nil, "", "", errors.New("file parent changed during resolution")
	}
	return f, base, filepath.Clean(resolved), nil
}

func openTargetPath(parentFD int, base string) (*os.File, unix.Stat_t, string, error) {
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(parentFD, base, how)
	if err != nil {
		return nil, unix.Stat_t{}, "", err
	}
	f := os.NewFile(uintptr(fd), base)
	if f == nil {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, "", errors.New("invalid file descriptor")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, unix.Stat_t{}, "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = f.Close()
		return nil, unix.Stat_t{}, "", errors.New("file tools support regular files only")
	}
	resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		_ = f.Close()
		return nil, unix.Stat_t{}, "", err
	}
	if strings.HasSuffix(resolved, " (deleted)") || !filepath.IsAbs(resolved) {
		_ = f.Close()
		return nil, unix.Stat_t{}, "", errors.New("file changed during resolution")
	}
	return f, st, filepath.Clean(resolved), nil
}

func rejectPseudoFilesystem(fd int) error {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return err
	}
	// The first file slice deliberately supports ordinary regular files only.
	// Kernel/control pseudo-files remain available through the explicit shell
	// boundary rather than being treated as normal ranged file-transfer input.
	switch uint64(fs.Type) {
	case 0x9fa0, // proc
		0x62656572, // sysfs
		0x64626720, // debugfs
		0x73636673, // securityfs
		0x74726163, // tracefs
		0xcafe4a11, // bpf
		0x27e0eb,   // cgroup v1
		0x63677270, // cgroup v2
		0x62656570: // configfs
		return errors.New("file tools do not support kernel/control pseudo-filesystems")
	default:
		return nil
	}
}

func fileVersion(st unix.Stat_t) string {
	material := fmt.Sprintf(
		"v1:%d:%d:%d:%d:%d:%d:%d:%d:%d:%d",
		st.Dev,
		st.Ino,
		st.Size,
		st.Mode,
		st.Uid,
		st.Gid,
		st.Mtim.Sec,
		st.Mtim.Nsec,
		st.Ctim.Sec,
		st.Ctim.Nsec,
	)
	sum := sha256.Sum256([]byte(material))
	return "v1:" + hex.EncodeToString(sum[:])
}

func sameFileIdentity(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}

func (s *Server) fileDecision(action, requestedPath, effectivePath string) policy.Decision {
	requested := s.guard.Evaluate(action+" "+requestedPath, true)
	if !requested.Allowed || requested.RequiresApproval {
		return requested
	}
	if effectivePath != "" && effectivePath != requestedPath {
		effective := s.guard.Evaluate(action+" "+effectivePath, true)
		if !effective.Allowed || effective.RequiresApproval {
			return effective
		}
	}
	return requested
}

func (s *Server) readFile(req Request) Response {
	path, err := validateFilePath(req.Path)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	if req.Offset < 0 {
		return fileError("invalid_offset", "validation", errors.New("offset must be non-negative"))
	}
	limit := req.Limit
	if limit == 0 {
		limit = maxFileReadBytes
	}
	if limit < 0 || limit > maxFileReadBytes {
		return fileError("invalid_limit", "validation", fmt.Errorf("limit must be 0 (default) or between 1 and %d bytes", maxFileReadBytes))
	}

	parent, base, _, err := openFileParent(path)
	if err != nil {
		return fileError("file_unavailable", "state", err)
	}
	defer parent.Close()

	pathFile, before, effectivePath, err := openTargetPath(int(parent.Fd()), base)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fileError("file_not_found", "state", err)
		}
		return fileError("unsafe_file_type", "validation", err)
	}
	defer pathFile.Close()
	if err := rejectPseudoFilesystem(int(pathFile.Fd())); err != nil {
		return fileError("unsupported_file_type", "validation", err)
	}

	dec := s.fileDecision("read", path, effectivePath)
	if !dec.Allowed {
		return fileError("policy_denied", "policy", errors.New(dec.Reason))
	}
	if dec.RequiresApproval && !req.Approval {
		return fileApprovalResponse(dec)
	}
	version := fileVersion(before)
	if req.FileVersion != "" && req.FileVersion != version {
		return fileError("file_changed", "conflict", errors.New("file version does not match requested consistency token"))
	}

	dataFD, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", pathFile.Fd()), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fileError("file_unavailable", "state", fmt.Errorf("open resolved file: %w", err))
	}
	defer unix.Close(dataFD)
	var opened unix.Stat_t
	if err := unix.Fstat(dataFD, &opened); err != nil {
		return fileError("file_unavailable", "state", err)
	}
	if !sameFileIdentity(before, opened) {
		return fileError("file_changed", "conflict", errors.New("file changed while opening ranged read"))
	}

	buf := make([]byte, limit)
	n, err := unix.Pread(dataFD, buf, req.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fileError("file_read_failed", "io", err)
	}
	buf = buf[:n]
	var after unix.Stat_t
	if err := unix.Fstat(dataFD, &after); err != nil {
		return fileError("file_unavailable", "state", err)
	}
	if fileVersion(after) != version {
		return fileError("file_changed", "conflict", errors.New("file changed during ranged read"))
	}

	output, encoding := encodeOutputBytes(buf)
	if len(output) > maxFileEncodedBytes {
		return fileError("response_too_large", "resource", errors.New("encoded file response exceeds safety limit"))
	}
	next := req.Offset + int64(n)
	eof := next >= before.Size
	omitted := int64(0)
	if next < before.Size {
		omitted = before.Size - next
	}
	_ = s.audit.Write(audit.Entry{Action: "read_file", Mode: "root", Command: path, Success: true, PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass, PrincipalName: req.PrincipalName})
	return Response{
		OK:              true,
		Output:          output,
		OutputEncoding:  encoding,
		BytesSeen:       int64(n),
		BytesReturned:   int64(n),
		Truncated:       !eof,
		OmittedBytes:    omitted,
		RequestedOffset: req.Offset,
		Offset:          int64Ptr(req.Offset),
		NextOffset:      int64Ptr(next),
		FileSize:        int64Ptr(before.Size),
		FileVersion:     version,
		EOF:             boolPtr(eof),
	}
}

type fileWriteTestHooks struct {
	beforeExistingCommit func()
	beforeNewCommit      func()
	afterCommit          func()
}

func pathState(parentFD int, base string) (unix.Stat_t, bool, error) {
	var st unix.Stat_t
	err := unix.Fstatat(parentFD, base, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return unix.Stat_t{}, false, nil
	}
	if err != nil {
		return unix.Stat_t{}, false, err
	}
	return st, true, nil
}

func destinationState(parentFD int, base string) (unix.Stat_t, bool, error) {
	st, exists, err := pathState(parentFD, base)
	if err != nil || !exists {
		return st, exists, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return unix.Stat_t{}, true, errors.New("destination must be a regular file")
	}
	return st, true, nil
}

func createTempFileAt(parentFD int) (int, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return -1, "", err
		}
		name := ".ai-server-agent-write-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(parentFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err == nil {
			return fd, name, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return -1, "", err
		}
	}
	return -1, "", errors.New("could not allocate atomic write temporary file")
}

func writeAllFD(fd int, b []byte) error {
	for len(b) > 0 {
		n, err := unix.Write(fd, b)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func sameFileSnapshot(a, b unix.Stat_t) bool {
	return sameFileIdentity(a, b) && fileVersion(a) == fileVersion(b)
}

func sameFileCommitState(a, b unix.Stat_t) bool {
	// RENAME_EXCHANGE is a namespace mutation and may update ctime on some
	// filesystems. Commit-boundary comparison therefore binds the exact inode
	// and all ordinary write-relevant metadata that exchange itself does not
	// change. The full file_version (including ctime) is still checked before
	// commit.
	return sameFileIdentity(a, b) &&
		a.Size == b.Size &&
		a.Mode == b.Mode &&
		a.Uid == b.Uid &&
		a.Gid == b.Gid &&
		a.Mtim.Sec == b.Mtim.Sec &&
		a.Mtim.Nsec == b.Mtim.Nsec
}

func (s *Server) runBeforeExistingCommitHook() {
	if s.fileWriteHooks != nil && s.fileWriteHooks.beforeExistingCommit != nil {
		s.fileWriteHooks.beforeExistingCommit()
	}
}

func (s *Server) runBeforeNewCommitHook() {
	if s.fileWriteHooks != nil && s.fileWriteHooks.beforeNewCommit != nil {
		s.fileWriteHooks.beforeNewCommit()
	}
}

func (s *Server) runAfterCommitHook() {
	if s.fileWriteHooks != nil && s.fileWriteHooks.afterCommit != nil {
		s.fileWriteHooks.afterCommit()
	}
}

func unknownFileCompletion(message string, st *unix.Stat_t) Response {
	resp := fileError("unknown_completion", "state", errors.New(message))
	if st != nil {
		resp.FileVersion = fileVersion(*st)
		resp.FileSize = int64Ptr(st.Size)
	}
	return resp
}

// rollbackExistingExchange tries to restore the object displaced by an
// optimistic RENAME_EXCHANGE. It returns nil only when both names are observed
// back in their expected state and the directory entry changes are synced.
func rollbackExistingExchange(parentFD int, base, tempName string, prepared, displaced unix.Stat_t) error {
	currentBase, baseExists, err := pathState(parentFD, base)
	if err != nil {
		return fmt.Errorf("inspect replacement before rollback: %w", err)
	}
	if !baseExists || !sameFileCommitState(prepared, currentBase) {
		return errors.New("replacement changed before rollback")
	}
	currentTemp, tempExists, err := pathState(parentFD, tempName)
	if err != nil {
		return fmt.Errorf("inspect displaced destination before rollback: %w", err)
	}
	if !tempExists || !sameFileCommitState(displaced, currentTemp) {
		return errors.New("displaced destination changed before rollback")
	}
	if err := unix.Renameat2(parentFD, tempName, parentFD, base, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("rollback exchange: %w", err)
	}
	restoredBase, baseExists, err := pathState(parentFD, base)
	if err != nil {
		return fmt.Errorf("inspect restored destination: %w", err)
	}
	restoredTemp, tempExists, tempErr := pathState(parentFD, tempName)
	if tempErr != nil {
		return fmt.Errorf("inspect prepared file after rollback: %w", tempErr)
	}
	if !baseExists || !sameFileCommitState(displaced, restoredBase) || !tempExists || !sameFileCommitState(prepared, restoredTemp) {
		return errors.New("rollback completed but restored identities could not be proven")
	}
	if err := unix.Fsync(parentFD); err != nil {
		return fmt.Errorf("sync restored destination directory: %w", err)
	}
	restoredBase, baseExists, err = pathState(parentFD, base)
	if err != nil || !baseExists || !sameFileCommitState(displaced, restoredBase) {
		return errors.New("restored destination changed after rollback sync")
	}
	restoredTemp, tempExists, err = pathState(parentFD, tempName)
	if err != nil || !tempExists || !sameFileCommitState(prepared, restoredTemp) {
		return errors.New("prepared file changed after rollback sync")
	}
	return nil
}

func (s *Server) writeFile(req Request) Response {
	path, err := validateFilePath(req.Path)
	if err != nil {
		return fileError("invalid_path", "validation", err)
	}
	if len(req.Content) > maxFileWriteBytes {
		return fileError("input_too_large", "validation", fmt.Errorf("file content exceeds the %d-byte tool limit", maxFileWriteBytes))
	}
	if req.FileVersion != "" && req.MustNotExist {
		return fileError("invalid_precondition", "validation", errors.New("file_version and must_not_exist cannot be combined"))
	}
	if req.Mode&^uint32(07777) != 0 {
		return fileError("invalid_mode", "validation", errors.New("mode may contain Unix permission/special bits only"))
	}

	parent, base, resolvedParent, err := openFileParent(path)
	if err != nil {
		return fileError("parent_unavailable", "state", err)
	}
	defer parent.Close()
	parentFD := int(parent.Fd())
	if err := rejectPseudoFilesystem(parentFD); err != nil {
		return fileError("unsupported_file_type", "validation", err)
	}
	effectivePath := filepath.Join(resolvedParent, base)
	dec := s.fileDecision("write", path, effectivePath)
	if !dec.Allowed {
		return fileError("policy_denied", "policy", errors.New(dec.Reason))
	}
	if dec.RequiresApproval && !req.Approval {
		return fileApprovalResponse(dec)
	}

	initial, exists, err := destinationState(parentFD, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if req.MustNotExist && exists {
		return fileError("file_exists", "conflict", errors.New("destination already exists"))
	}
	if req.FileVersion != "" {
		if !exists || fileVersion(initial) != req.FileVersion {
			return fileError("file_changed", "conflict", errors.New("destination version does not match write precondition"))
		}
	}

	mode := uint32(0644)
	uid := uint32(os.Geteuid())
	gid := uint32(os.Getegid())
	if exists {
		mode = initial.Mode & 07777
		uid = initial.Uid
		gid = initial.Gid
	}
	if req.Mode != 0 {
		mode = req.Mode
	}

	tempFD, tempName, err := createTempFileAt(parentFD)
	if err != nil {
		return fileError("file_write_failed", "io", err)
	}
	tempNameOwned := true
	defer func() {
		_ = unix.Close(tempFD)
		if tempNameOwned {
			_ = unix.Unlinkat(parentFD, tempName, 0)
		}
	}()
	if err := writeAllFD(tempFD, []byte(req.Content)); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	if err := unix.Fsync(tempFD); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	if uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		if err := unix.Fchown(tempFD, int(uid), int(gid)); err != nil {
			return fileError("file_write_failed", "io", err)
		}
	}
	if err := unix.Fchmod(tempFD, mode); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	if err := unix.Fsync(tempFD); err != nil {
		return fileError("file_write_failed", "io", err)
	}
	var prepared unix.Stat_t
	if err := unix.Fstat(tempFD, &prepared); err != nil {
		return fileError("file_write_failed", "io", fmt.Errorf("inspect prepared replacement: %w", err))
	}

	current, currentExists, err := destinationState(parentFD, base)
	if err != nil {
		return fileError("unsafe_file_type", "validation", err)
	}
	if exists {
		if !currentExists || !sameFileSnapshot(initial, current) {
			return fileError("file_changed", "conflict", errors.New("destination changed before atomic replacement"))
		}
	} else if currentExists {
		return fileError("file_changed", "conflict", errors.New("destination appeared before atomic replacement"))
	}

	if exists && req.FileVersion != "" {
		// RENAME_EXCHANGE atomically captures the exact object displaced at
		// commit time under tempName. That lets the optimistic precondition be
		// verified against the commit boundary rather than a preceding lookup.
		s.runBeforeExistingCommitHook()
		if err := unix.Renameat2(parentFD, tempName, parentFD, base, unix.RENAME_EXCHANGE); err != nil {
			return fileError("file_write_failed", "io", err)
		}
		// tempName now contains the atomically displaced destination, not our
		// prepared file. Never let generic cleanup unlink it on an uncertain path.
		tempNameOwned = false
		s.runAfterCommitHook()

		displaced, displacedExists, inspectErr := pathState(parentFD, tempName)
		if inspectErr != nil || !displacedExists {
			return unknownFileCompletion("replacement committed but displaced destination identity could not be inspected", nil)
		}
		if !sameFileCommitState(initial, displaced) {
			if err := rollbackExistingExchange(parentFD, base, tempName, prepared, displaced); err != nil {
				return unknownFileCompletion("replacement raced with a destination change and rollback could not be proven", nil)
			}
			// Proven rollback puts our prepared inode back under tempName.
			tempNameOwned = true
			return fileError("file_changed", "conflict", errors.New("destination changed at atomic replacement boundary"))
		}
		if err := unix.Unlinkat(parentFD, tempName, 0); err != nil {
			return unknownFileCompletion("replacement committed but displaced destination cleanup failed", &prepared)
		}
	} else if exists {
		if err := unix.Renameat(parentFD, tempName, parentFD, base); err != nil {
			return fileError("file_write_failed", "io", err)
		}
		tempNameOwned = false
		s.runAfterCommitHook()
	} else {
		s.runBeforeNewCommitHook()
		if err := unix.Renameat2(parentFD, tempName, parentFD, base, unix.RENAME_NOREPLACE); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return fileError("file_changed", "conflict", errors.New("destination appeared before atomic replacement"))
			}
			return fileError("file_write_failed", "io", err)
		}
		tempNameOwned = false
		s.runAfterCommitHook()
	}

	final, finalExists, err := destinationState(parentFD, base)
	if err != nil || !finalExists {
		return unknownFileCompletion("replacement committed but final destination could not be inspected", nil)
	}
	if !sameFileIdentity(prepared, final) {
		return unknownFileCompletion("replacement committed but final destination no longer names the prepared replacement", nil)
	}
	if err := unix.Fsync(parentFD); err != nil {
		return unknownFileCompletion(fmt.Sprintf("replacement committed but parent directory sync failed: %v", err), &final)
	}
	confirmed, confirmedExists, err := destinationState(parentFD, base)
	if err != nil || !confirmedExists || !sameFileIdentity(prepared, confirmed) {
		return unknownFileCompletion("replacement committed but final destination identity changed after directory sync", nil)
	}

	_ = s.audit.Write(audit.Entry{Action: "write_file", Mode: "root", Command: path, Success: true, PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass, PrincipalName: req.PrincipalName})
	return Response{
		OK:          true,
		Status:      "written",
		FileSize:    int64Ptr(confirmed.Size),
		FileVersion: fileVersion(confirmed),
	}
}
