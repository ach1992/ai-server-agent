package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteIncludesStructuredPrincipalAndFingerprintWithoutCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	secrets := []string{
		"Authorization: Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"AI_SERVER_AGENT_CF_TOKEN=cf-sensitive-token",
		"API_KEY=api-sensitive-token",
		"password=password-sensitive-value",
	}
	if err := logger.Write(Entry{
		Phase:          "start",
		Action:         "run",
		Mode:           "worker",
		Command:        "printf %s " + strings.Join(secrets, " ") + " https://example.invalid",
		RequestID:      "req-1",
		OperationID:    "op-1",
		PrincipalID:    "mcp-gateway",
		PrincipalClass: "gateway",
		PrincipalName:  "mcp-gateway",
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(secrets, "example.invalid") {
		if strings.Contains(string(b), secret) {
			t.Fatalf("secret-bearing command leaked into audit: %s", b)
		}
	}
	if strings.Contains(string(b), `"success"`) {
		t.Fatalf("start-only audit event must not claim an operation outcome: %s", b)
	}
	var got Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion || got.Phase != "start" || got.RequestID != "req-1" || got.OperationID != "op-1" {
		t.Fatalf("unexpected structured event: %+v", got)
	}
	if got.PrincipalID != "mcp-gateway" || got.PrincipalClass != "gateway" || got.PrincipalName != "mcp-gateway" {
		t.Fatalf("unexpected principal context: %+v", got)
	}
	if got.CommandFingerprint == "" || got.FingerprintKeyVersion != 1 || got.FingerprintKeyID == "" {
		t.Fatalf("missing keyed command fingerprint: %+v", got)
	}
}

func TestSameCommandHasStableKeyedFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	for i := 0; i < 2; i++ {
		if err := logger.Write(Entry{Phase: "start", Action: "run", Command: "printf stable"}); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(string(mustRead(t, path))), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d", len(lines))
	}
	var first, second Entry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.CommandFingerprint == "" || first.CommandFingerprint != second.CommandFingerprint {
		t.Fatalf("fingerprints differ: %q %q", first.CommandFingerprint, second.CommandFingerprint)
	}
}

func TestRotationIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	logger.maxFileBytes = 256
	logger.maxFiles = 3
	logger.safetyReserve = 0
	for i := 0; i < 20; i++ {
		if err := logger.Write(Entry{Phase: "complete", Action: "run", RequestID: strings.Repeat("x", 100)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("retention exceeded configured rotated-file bound: %v", err)
	}
	for _, name := range []string{path, path + ".1", path + ".2", path + ".3"} {
		if fi, err := os.Stat(name); err == nil && fi.Mode().Perm()&0022 != 0 {
			t.Fatalf("unsafe audit mode for %s: %04o", name, fi.Mode().Perm())
		}
	}
}

func TestRejectsSymlinkedAuditPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	logger := New(path)
	logger.safetyReserve = 0
	if err := logger.Write(Entry{Phase: "start", Action: "run"}); err == nil {
		t.Fatal("symlinked audit path was accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSameCommandDiffersAcrossIndependentInstallKeys(t *testing.T) {
	fingerprint := func(path string) string {
		logger := New(path)
		if err := logger.Write(Entry{Phase: "start", Action: "run", Command: "printf same-command"}); err != nil {
			t.Fatal(err)
		}
		var got Entry
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(mustRead(t, path)))), &got); err != nil {
			t.Fatal(err)
		}
		return got.CommandFingerprint
	}
	first := fingerprint(filepath.Join(t.TempDir(), "audit.jsonl"))
	second := fingerprint(filepath.Join(t.TempDir(), "audit.jsonl"))
	if first == "" || second == "" || first == second {
		t.Fatalf("per-install keyed fingerprints must differ: %q %q", first, second)
	}
}

func TestKeyChangeRequiresExecutorRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	if err := logger.Write(Entry{Phase: "start", Action: "run", Command: "printf one"}); err != nil {
		t.Fatal(err)
	}
	keyPath := path + ".key"
	if err := os.WriteFile(keyPath, []byte("v1:"+strings.Repeat("a", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Entry{Phase: "start", Action: "run", Command: "printf two"}); err == nil || !strings.Contains(err.Error(), "key changed") {
		t.Fatalf("live key replacement was not rejected: %v", err)
	}
}

func TestConcurrentRotationPreservesEventIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	logger.maxFileBytes = 2048
	logger.maxFiles = 64
	logger.safetyReserve = 0

	const workers = 16
	const perWorker = 10
	errCh := make(chan error, workers*perWorker)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if err := logger.Write(Entry{Phase: "complete", Action: "run", Command: "printf concurrent", RequestID: strings.Repeat("x", 32)}); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent audit write failed: %v", err)
	}

	seen := map[string]struct{}{}
	count := 0
	for i := 0; i <= logger.maxFiles; i++ {
		name := path
		if i > 0 {
			name = path + "." + fmt.Sprint(i)
		}
		b, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var event Entry
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("corrupt retained audit event in %s: %v", name, err)
			}
			if event.EventID == "" {
				t.Fatalf("retained audit event is missing event_id: %s", line)
			}
			if _, duplicate := seen[event.EventID]; duplicate {
				t.Fatalf("duplicate retained event_id %q", event.EventID)
			}
			seen[event.EventID] = struct{}{}
			count++
		}
	}
	if count != workers*perWorker {
		t.Fatalf("retained event count = %d, want %d", count, workers*perWorker)
	}
}

func TestProductionLoggerRequiresProvisionedFingerprintKey(t *testing.T) {
	dir := t.TempDir()
	logger := NewWithKey(filepath.Join(dir, "audit.jsonl"), filepath.Join(dir, "missing.key"))
	logger.safetyReserve = 0
	if err := logger.Write(Entry{Phase: "start", Action: "run", Command: "true"}); err == nil || !strings.Contains(err.Error(), "fingerprint key") {
		t.Fatalf("missing production fingerprint key did not fail closed: %v", err)
	}
}

func TestRejectsSymlinkedFingerprintKey(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.key")
	if err := os.WriteFile(target, []byte("v1:"+strings.Repeat("a", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "audit.key")
	if err := os.Symlink(target, key); err != nil {
		t.Fatal(err)
	}
	logger := NewWithKey(filepath.Join(dir, "audit.jsonl"), key)
	logger.safetyReserve = 0
	if err := logger.Write(Entry{Phase: "start", Action: "run", Command: "true"}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlinked production fingerprint key was accepted: %v", err)
	}
}

func TestDiskReserveFailsBeforeAuditAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	logger.safetyReserve = int64(^uint64(0) >> 1)
	if err := logger.Write(Entry{Phase: "start", Action: "run", Command: "true"}); err == nil || !strings.Contains(err.Error(), "safety reserve") {
		t.Fatalf("audit disk-pressure reserve did not fail closed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("audit file was created despite disk-pressure rejection; err=%v", err)
	}
}

func TestCompletionFailureAtomicallyLatchesBeforeConcurrentStartAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	logger.safetyReserve = 0

	if err := logger.Write(Entry{Phase: "start", Action: "write_file", RequestID: "req-a"}); err != nil {
		t.Fatal(err)
	}

	completionEntered := make(chan struct{})
	releaseCompletion := make(chan struct{})
	forced := errors.New("forced completion persistence failure")
	logger.writeHook = func(e Entry) error {
		if e.Phase == "complete" {
			close(completionEntered)
			<-releaseCompletion
			return forced
		}
		return nil
	}

	completionDone := make(chan error, 1)
	go func() {
		completionDone <- logger.WriteCompletion(Entry{Phase: "complete", Action: "write_file", RequestID: "req-a"})
	}()
	<-completionEntered

	startAttempted := make(chan struct{})
	logger.beforeLockHook = func(e Entry) {
		if e.RequestID == "req-b" {
			close(startAttempted)
		}
	}
	startDone := make(chan error, 1)
	go func() {
		startDone <- logger.Write(Entry{Phase: "start", Action: "write_file", RequestID: "req-b"})
	}()
	// This signal is emitted by Write itself immediately before it attempts the
	// logger mutex, while Action A is still holding that mutex inside the
	// forced completion failure boundary.
	<-startAttempted

	select {
	case err := <-startDone:
		t.Fatalf("concurrent start escaped completion critical section before failure latched: %v", err)
	default:
	}

	close(releaseCompletion)
	if err := <-completionDone; !errors.Is(err, forced) {
		t.Fatalf("completion error = %v, want forced failure", err)
	}
	if err := <-startDone; !errors.Is(err, ErrDegraded) {
		t.Fatalf("concurrent start error = %v, want ErrDegraded", err)
	}

	// Repairing the underlying write path/hook alone must not clear the safety
	// latch in the running logger instance.
	logger.beforeLockHook = nil
	logger.writeHook = nil
	if err := logger.Write(Entry{Phase: "start", Action: "write_file", RequestID: "req-c"}); !errors.Is(err, ErrDegraded) {
		t.Fatalf("same logger resumed after repair without restart: %v", err)
	}

	// Reconstructing the logger models executor restart and is the only way to
	// clear the in-memory degraded latch after the audit path is healthy.
	restarted := New(path)
	restarted.safetyReserve = 0
	if err := restarted.Write(Entry{Phase: "start", Action: "write_file", RequestID: "req-after-restart"}); err != nil {
		t.Fatalf("fresh logger did not recover after restart: %v", err)
	}
}

func TestPreActionWriteFailureDoesNotLatchDegraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	logger.safetyReserve = 0
	forced := errors.New("forced start persistence failure")
	logger.writeHook = func(e Entry) error {
		return forced
	}
	if err := logger.Write(Entry{Phase: "start", Action: "run"}); !errors.Is(err, forced) {
		t.Fatalf("start error = %v, want forced failure", err)
	}
	logger.writeHook = nil
	if err := logger.Write(Entry{Phase: "start", Action: "run"}); err != nil {
		t.Fatalf("pre-action failure incorrectly latched degraded state: %v", err)
	}
}
