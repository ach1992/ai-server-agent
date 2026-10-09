package audit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	SchemaVersion          = 2
	fingerprintKeyVersion  = 1
	defaultMaxFileBytes    = int64(8 << 20)
	defaultRotatedFiles    = 4
	defaultSafetyReserve   = int64(64 << 20)
	maxMetadataStringBytes = 256
)

var ErrDegraded = errors.New("audit logger is degraded until executor restart after audit-path recovery")

type Logger struct {
	path           string
	keyPath        string
	allowKeyCreate bool
	maxFileBytes   int64
	maxFiles       int
	safetyReserve  int64

	mu       sync.Mutex
	key      []byte
	degraded bool

	beforeLockHook func(Entry)       // test-only interleaving observation
	writeHook      func(Entry) error // test-only failure/interleaving injection
}

type Entry struct {
	SchemaVersion         int    `json:"schema_version"`
	EventID               string `json:"event_id"`
	Time                  string `json:"time"`
	Phase                 string `json:"phase"`
	Action                string `json:"action"`
	Mode                  string `json:"mode,omitempty"`
	PolicyCategory        string `json:"policy_category,omitempty"`
	Decision              string `json:"decision,omitempty"`
	Success               *bool  `json:"success,omitempty"`
	ExitCode              int    `json:"exit_code,omitempty"`
	DurationMS            int64  `json:"duration_ms,omitempty"`
	ErrorClass            string `json:"error_class,omitempty"`
	ReasonCode            string `json:"reason_code,omitempty"`
	RequestID             string `json:"request_id,omitempty"`
	OperationID           string `json:"operation_id,omitempty"`
	ApprovalID            string `json:"approval_id,omitempty"`
	JobID                 string `json:"job_id,omitempty"`
	PrincipalID           string `json:"principal_id,omitempty"`
	PrincipalClass        string `json:"principal_class,omitempty"`
	PrincipalName         string `json:"principal_name,omitempty"`
	CommandFingerprint    string `json:"command_fingerprint,omitempty"`
	FingerprintKeyVersion int    `json:"fingerprint_key_version,omitempty"`
	FingerprintKeyID      string `json:"fingerprint_key_id,omitempty"`

	Command string `json:"-"`
}

func New(path string) *Logger {
	return &Logger{
		path:           path,
		keyPath:        path + ".key",
		allowKeyCreate: true,
		maxFileBytes:   defaultMaxFileBytes,
		maxFiles:       defaultRotatedFiles,
		safetyReserve:  defaultSafetyReserve,
	}
}

func NewWithKey(path, keyPath string) *Logger {
	return &Logger{
		path:          path,
		keyPath:       keyPath,
		maxFileBytes:  defaultMaxFileBytes,
		maxFiles:      defaultRotatedFiles,
		safetyReserve: defaultSafetyReserve,
	}
}

func NewWithKeyAndReserve(path, keyPath string, safetyReserve int64) *Logger {
	logger := NewWithKey(path, keyPath)
	if safetyReserve > logger.safetyReserve {
		logger.safetyReserve = safetyReserve
	}
	return logger
}

func (l *Logger) Write(e Entry) error {
	if l.beforeLockHook != nil {
		l.beforeLockHook(e)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.degraded {
		return ErrDegraded
	}
	return l.writeLocked(e)
}

// WriteCompletion durably appends a completion event. If any stage of the
// completion write fails, degraded state is latched before the logger mutex is
// released. This makes the failed completion and the safety latch one atomic
// admission boundary for concurrent action-start writes.
func (l *Logger) WriteCompletion(e Entry) error {
	if l.beforeLockHook != nil {
		l.beforeLockHook(e)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.degraded {
		return ErrDegraded
	}
	if err := l.writeLocked(e); err != nil {
		l.degraded = true
		return err
	}
	return nil
}

func (l *Logger) writeLocked(e Entry) error {
	if l.writeHook != nil {
		if err := l.writeHook(e); err != nil {
			return err
		}
	}
	if err := l.ensureKeyLocked(); err != nil {
		return err
	}
	if err := l.validateParentLocked(); err != nil {
		return err
	}
	if err := l.ensureDiskReserveLocked(); err != nil {
		return err
	}

	eventID, err := randomID()
	if err != nil {
		return err
	}
	e.SchemaVersion = SchemaVersion
	e.EventID = eventID
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	e.Phase = bounded(e.Phase)
	e.Action = bounded(e.Action)
	e.Mode = bounded(e.Mode)
	e.PolicyCategory = bounded(e.PolicyCategory)
	e.Decision = bounded(e.Decision)
	e.ErrorClass = bounded(e.ErrorClass)
	e.ReasonCode = bounded(e.ReasonCode)
	e.RequestID = bounded(e.RequestID)
	e.OperationID = bounded(e.OperationID)
	e.ApprovalID = bounded(e.ApprovalID)
	e.JobID = bounded(e.JobID)
	e.PrincipalID = bounded(e.PrincipalID)
	e.PrincipalClass = bounded(e.PrincipalClass)
	e.PrincipalName = bounded(e.PrincipalName)
	if e.Command != "" {
		e.CommandFingerprint = l.commandFingerprintLocked(e.Command)
		e.FingerprintKeyVersion = fingerprintKeyVersion
		e.FingerprintKeyID = l.fingerprintKeyIDLocked()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := l.rotateIfNeededLocked(int64(len(b))); err != nil {
		return err
	}
	return l.appendDurableLocked(b)
}

func (l *Logger) commandFingerprintLocked(command string) string {
	mac := hmac.New(sha256.New, l.key)
	_, _ = mac.Write([]byte(command))
	return hex.EncodeToString(mac.Sum(nil))
}

func (l *Logger) fingerprintKeyIDLocked() string {
	sum := sha256.Sum256(l.key)
	return hex.EncodeToString(sum[:8])
}

func (l *Logger) ensureKeyLocked() error {
	if l.keyPath == "" {
		return errors.New("audit fingerprint key path is empty")
	}
	if err := l.ensureTestKeyLocked(); err != nil {
		return err
	}
	fi, err := os.Lstat(l.keyPath)
	if err != nil {
		return fmt.Errorf("inspect audit fingerprint key: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return errors.New("audit fingerprint key is not a regular file")
	}
	if fi.Mode().Perm() != 0600 {
		return fmt.Errorf("audit fingerprint key mode is unsafe: %04o", fi.Mode().Perm())
	}
	if fi.Size() != int64(len("v1:")+64+1) {
		return fmt.Errorf("audit fingerprint key size is invalid: %d", fi.Size())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
		return errors.New("audit fingerprint key owner is unsafe")
	}
	f, err := os.OpenFile(l.keyPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open audit fingerprint key: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil {
		return fmt.Errorf("read audit fingerprint key: %w", err)
	}
	raw := strings.TrimSpace(string(b))
	const prefix = "v1:"
	if !strings.HasPrefix(raw, prefix) {
		return errors.New("audit fingerprint key has unsupported version")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil || len(decoded) != 32 {
		return errors.New("audit fingerprint key payload is invalid")
	}
	if len(l.key) != 0 && !hmac.Equal(l.key, decoded) {
		return errors.New("audit fingerprint key changed while executor is running; restart is required")
	}
	if len(l.key) == 0 {
		l.key = decoded
	}
	return nil
}

func (l *Logger) ensureTestKeyLocked() error {
	if !l.allowKeyCreate {
		return nil
	}
	if _, err := os.Lstat(l.keyPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.keyPath), 0700); err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	f, err := os.OpenFile(l.keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	payload := []byte("v1:" + hex.EncodeToString(key) + "\n")
	if _, err := f.Write(payload); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (l *Logger) validateParentLocked() error {
	dir := filepath.Dir(l.path)
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect audit directory: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return errors.New("audit directory is not a real directory")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
		return errors.New("audit directory owner is unsafe")
	}
	if fi.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("audit directory mode is unsafe: %04o", fi.Mode().Perm())
	}
	return nil
}

func (l *Logger) ensureDiskReserveLocked() error {
	if l.safetyReserve <= 0 {
		return nil
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(l.path), &stat); err != nil {
		return fmt.Errorf("inspect audit filesystem capacity: %w", err)
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free < l.safetyReserve {
		return fmt.Errorf("audit filesystem is below the %d-byte safety reserve (%d bytes available)", l.safetyReserve, free)
	}
	return nil
}

func (l *Logger) rotateIfNeededLocked(incoming int64) error {
	fi, err := os.Lstat(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateAuditFile(fi); err != nil {
		return err
	}
	if fi.Size()+incoming <= l.maxFileBytes {
		return nil
	}

	oldest := fmt.Sprintf("%s.%d", l.path, l.maxFiles)
	if err := removeSafeAuditFile(oldest); err != nil {
		return err
	}
	for i := l.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.path, i)
		dst := fmt.Sprintf("%s.%d", l.path, i+1)
		if _, err := os.Lstat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := renameSafeAuditFile(src, dst); err != nil {
			return err
		}
	}
	if err := renameSafeAuditFile(l.path, l.path+".1"); err != nil {
		return err
	}
	return syncDir(filepath.Dir(l.path))
}

func (l *Logger) appendDurableLocked(b []byte) error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0640)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	fi, statErr := f.Stat()
	if statErr != nil {
		f.Close()
		return statErr
	}
	if err := validateAuditFile(fi); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0640); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("append audit log: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync audit log: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(l.path))
}

func validateAuditFile(fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return errors.New("audit path is not a regular file")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
		return errors.New("audit file owner is unsafe")
	}
	if fi.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("audit file mode is unsafe: %04o", fi.Mode().Perm())
	}
	return nil
}

func removeSafeAuditFile(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateAuditFile(fi); err != nil {
		return err
	}
	return os.Remove(path)
}

func renameSafeAuditFile(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := validateAuditFile(fi); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("audit rotation destination already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(src, dst)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func bounded(s string) string {
	if len(s) <= maxMetadataStringBytes {
		return s
	}
	return s[:maxMetadataStringBytes]
}

func Bool(v bool) *bool { return &v }
