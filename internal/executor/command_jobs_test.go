package executor

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

func TestBoundedOutputCollectorKeepsHeadTail(t *testing.T) {
	w := newBoundedOutputCollector(10)
	if _, err := w.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	got := w.Result()
	if !got.Truncated || got.BytesSeen != 16 || got.BytesReturned != 10 || got.OmittedBytes != 6 {
		t.Fatalf("unexpected bounded output metadata: %+v", got)
	}
	if got.Encoding != "utf-8" {
		t.Fatalf("encoding = %q, want utf-8", got.Encoding)
	}
	if !strings.Contains(got.Output, "01234") || !strings.Contains(got.Output, "BCDEF") || !strings.Contains(got.Output, "6 bytes omitted") {
		t.Fatalf("bounded output lost head/tail evidence: %q", got.Output)
	}
}

func TestBoundedOutputCollectorUsesBase64ForBinary(t *testing.T) {
	w := newBoundedOutputCollector(8)
	raw := []byte{0xff, 0x00, 0x01, 0x02}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	got := w.Result()
	if got.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64", got.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Output)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(raw) {
		t.Fatalf("decoded output = %v, want %v", decoded, raw)
	}
}

func TestBoundedOutputCollectorDoesNotEmitBrokenUTF8Boundary(t *testing.T) {
	w := newBoundedOutputCollector(6)
	raw := []byte("A€BC€D")
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	got := w.Result()
	if !got.Truncated {
		t.Fatalf("expected truncation: %+v", got)
	}
	if got.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64 when head/tail split a UTF-8 rune", got.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Output)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(decoded)) != got.BytesReturned {
		t.Fatalf("decoded bytes = %d, metadata = %d", len(decoded), got.BytesReturned)
	}
}

func TestBoundedJobLogRetainsLogicalTail(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
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
	payload := []byte(strings.Repeat("a", int(maxJobLogBytes+128)))
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > int64(jobLogHeaderSize)+maxJobLogBytes {
		t.Fatalf("physical log grew to %d bytes", fi.Size())
	}

	s := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	resp := s.jobOutputBounded(Request{JobID: "123", Offset: 0, Limit: 16})
	if !resp.OK {
		t.Fatalf("job output failed: %+v", resp)
	}
	if !resp.RetentionTruncated || resp.AvailableFromOffset != 128 || resp.CurrentEnd != maxJobLogBytes+128 {
		t.Fatalf("unexpected logical retention metadata: %+v", resp)
	}
	if resp.NextOffset != 144 || resp.BytesReturned != 16 || resp.Output != strings.Repeat("a", 16) {
		t.Fatalf("unexpected bounded range response: %+v", resp)
	}
}

func TestJobOutputIsBinarySafe(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(jobs, "456.log")
	if err := os.WriteFile(logPath, nil, 0640); err != nil {
		t.Fatal(err)
	}
	writer, err := newJobLogWriter(logPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{0xff, 0x00, 'A', 0xfe}
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	resp := s.jobOutputBounded(Request{JobID: "456"})
	if !resp.OK || resp.OutputEncoding != "base64" {
		t.Fatalf("binary job output response = %+v", resp)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Output)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(raw) {
		t.Fatalf("decoded output = %v, want %v", decoded, raw)
	}
}

func TestRunJobHelperConsumesProtectedCommandHandoff(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "job.cmd")
	logPath := filepath.Join(dir, "job.log")
	statusPath := filepath.Join(dir, "job.status")
	startedPath := filepath.Join(dir, "job.started")
	if err := os.WriteFile(commandPath, []byte("printf safe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{logPath, statusPath, startedPath} {
		if err := os.WriteFile(path, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}

	code := RunJobHelper([]string{
		"--command-file", commandPath,
		"--log-file", logPath,
		"--status-file", statusPath,
		"--started-file", startedPath,
	})
	if code != 0 {
		t.Fatalf("job helper exit = %d, want 0", code)
	}
	if _, err := os.Stat(commandPath); !os.IsNotExist(err) {
		t.Fatalf("protected command handoff was not removed; stat err=%v", err)
	}
	if b, err := os.ReadFile(startedPath); err != nil || strings.TrimSpace(string(b)) != "started" {
		t.Fatalf("started marker = %q, err=%v", b, err)
	}
	if b, err := os.ReadFile(statusPath); err != nil || strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("status = %q, err=%v", b, err)
	}

	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	header, ring, err := readJobLogHeader(f, fi.Size())
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if !ring || header.CurrentEnd != 4 {
		f.Close()
		t.Fatalf("unexpected job log header: ring=%v header=%+v", ring, header)
	}
	raw := make([]byte, 4)
	if err := readRingBytes(f, header, 0, raw); err != nil {
		f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	if string(raw) != "safe" {
		t.Fatalf("job output = %q, want safe", raw)
	}
}

func TestPersistentJobIdempotencyAndCommandPrivacy(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	fakeBin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "systemd-run.args")
	count := filepath.Join(t.TempDir(), "systemd-run.count")

	systemctl := filepath.Join(fakeBin, "systemctl")
	systemctlScript := "#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(systemctl, []byte(systemctlScript), 0755); err != nil {
		t.Fatal(err)
	}
	systemdRun := filepath.Join(fakeBin, "systemd-run")
	systemdScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(capture) + "\nprintf x >> " + shellQuote(count) + "\nprintf 'accepted\\n'\n"
	if err := os.WriteFile(systemdRun, []byte(systemdScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	s := &Server{
		cfg: config.Config{
			StateDir:     state,
			WorkspaceDir: workspace,
			WorkerUser:   current.Username,
		},
		token:     "test-executor-token",
		guard:     policy.New(nil),
		audit:     audit.New(auditPath),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
	}
	secretCommand := "printf super-sensitive-command"
	first := s.startJobBounded(Request{Command: secretCommand, OperationID: "retry-key-1"})
	if !first.OK || first.JobID == "" {
		t.Fatalf("first start failed: %+v", first)
	}
	second := s.startJobBounded(Request{Command: secretCommand, OperationID: "retry-key-1"})
	if !second.OK || !second.IdempotentReplay || second.JobID != first.JobID {
		t.Fatalf("idempotent replay = %+v, first=%+v", second, first)
	}
	conflict := s.startJobBounded(Request{Command: "printf different", OperationID: "retry-key-1"})
	if conflict.OK || conflict.ErrorCode != "idempotency_conflict" {
		t.Fatalf("operation conflict = %+v", conflict)
	}
	if b, err := os.ReadFile(count); err != nil || len(b) != 1 {
		t.Fatalf("systemd-run invocation count = %q, err=%v", b, err)
	}
	if b, err := os.ReadFile(capture); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(b), secretCommand) || strings.Contains(string(b), "super-sensitive-command") {
		t.Fatalf("raw command leaked into systemd-run argv: %q", b)
	}
	if b, err := os.ReadFile(auditPath); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(b), secretCommand) || strings.Contains(string(b), "super-sensitive-command") {
		t.Fatalf("raw command leaked into audit: %q", b)
	}
	claims, err := os.ReadDir(filepath.Join(jobs, "claims"))
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claim count = %d, want 1", len(claims))
	}
	claimBytes, err := os.ReadFile(filepath.Join(jobs, "claims", claims[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(claimBytes), secretCommand) || strings.Contains(string(claimBytes), "super-sensitive-command") {
		t.Fatalf("raw command leaked into idempotency claim: %q", claimBytes)
	}
}

func TestPersistentJobCapacityFailsWithoutQueue(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	systemctlScript := "#!/bin/sh\nif [ \"$1\" = list-units ]; then\n  printf 'ai-job-1.service loaded active running x\\nai-job-2.service loaded active running x\\nai-job-3.service loaded active running x\\nai-job-4.service loaded active running x\\n'\n  exit 0\nfi\nexit 64\n"
	if err := os.WriteFile(systemctl, []byte(systemctlScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	s := &Server{
		cfg:       config.Config{StateDir: state, WorkspaceDir: t.TempDir(), WorkerUser: "aiworker"},
		token:     "test-executor-token",
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
	}
	resp := s.startJobBounded(Request{Command: "printf safe"})
	if resp.OK || resp.ErrorCode != "resource_limit" || resp.Status != "busy" || !resp.Retryable {
		t.Fatalf("capacity response = %+v", resp)
	}
}

func TestPersistentJobClaimRecoversBeforeLaunch(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	systemctlScript := "#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(systemctl, []byte(systemctlScript), 0755); err != nil {
		t.Fatal(err)
	}
	systemdRun := filepath.Join(fakeBin, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nprintf 'accepted\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg: config.Config{
			StateDir:     state,
			WorkspaceDir: t.TempDir(),
			WorkerUser:   current.Username,
		},
		token:     "test-executor-token",
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
	}
	req := Request{Command: "printf recovered", OperationID: "claim-crash-window"}
	claim := jobClaim{
		Version:     1,
		JobID:       "123456789",
		Fingerprint: server.jobFingerprint(req),
		State:       "claimed",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	claimPath := filepath.Join(claims, operationClaimName(req.OperationID))
	if err := createJobClaim(claimPath, claim); err != nil {
		t.Fatal(err)
	}

	resp := server.startJobBounded(req)
	if !resp.OK || !resp.IdempotentReplay || resp.JobID != claim.JobID {
		t.Fatalf("claim recovery response = %+v", resp)
	}
	recovered, found, err := readJobClaim(claimPath)
	if err != nil || !found {
		t.Fatalf("read recovered claim: found=%v err=%v", found, err)
	}
	if recovered.State != "started" {
		t.Fatalf("recovered claim state = %q, want started", recovered.State)
	}
	commandBytes, err := os.ReadFile(filepath.Join(jobs, claim.JobID+".cmd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(commandBytes) != req.Command {
		t.Fatalf("rebuilt command handoff = %q, want %q", commandBytes, req.Command)
	}
}

func TestPersistentJobIdempotencyStateIsBounded(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPersistentJobClaims; i++ {
		operationID := fmt.Sprintf("claim-%d", i)
		claim := jobClaim{
			Version:     1,
			JobID:       fmt.Sprintf("%d", 100000+i),
			Fingerprint: strings.Repeat("a", 64),
			State:       "claimed",
			CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := createJobClaim(filepath.Join(claims, operationClaimName(operationID)), claim); err != nil {
			t.Fatal(err)
		}
	}

	server := &Server{
		cfg:       config.Config{StateDir: state, WorkspaceDir: t.TempDir(), WorkerUser: "aiworker"},
		token:     "test-executor-token",
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
	}
	resp := server.startJobBounded(Request{Command: "printf safe", OperationID: "new-operation"})
	if resp.OK || resp.ErrorCode != "resource_limit" || resp.Status != "busy" || !resp.Retryable {
		t.Fatalf("idempotency capacity response = %+v", resp)
	}
}

func TestJobStatusReturnsStructuredExitCode(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(jobs, "789.status")
	if err := os.WriteFile(statusPath, []byte("7\n"), 0640); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	resp := server.jobStatus(Request{JobID: "789"})
	if !resp.OK || resp.Status != "completed" || resp.ExitCode != 7 {
		t.Fatalf("completed job status = %+v", resp)
	}
	if resp.Output != "7" || resp.OutputEncoding != "utf-8" || resp.BytesReturned != 1 {
		t.Fatalf("completed job output metadata = %+v", resp)
	}
}

func TestStartJobWithoutCallerOperationIDStillGetsRecoveryClaim(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	systemctlScript := "#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(systemctl, []byte(systemctlScript), 0755); err != nil {
		t.Fatal(err)
	}
	systemdRun := filepath.Join(fakeBin, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nprintf 'accepted\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       config.Config{StateDir: state, WorkspaceDir: t.TempDir(), WorkerUser: current.Username},
		token:     "test-executor-token",
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
	}
	resp := server.startJobBounded(Request{Command: "printf safe"})
	if !resp.OK || resp.JobID == "" {
		t.Fatalf("start response = %+v", resp)
	}
	claims, err := os.ReadDir(filepath.Join(jobs, "claims"))
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claim count = %d, want 1", len(claims))
	}
	claim, found, err := readJobClaim(filepath.Join(jobs, "claims", claims[0].Name()))
	if err != nil || !found {
		t.Fatalf("read recovery claim: found=%v err=%v", found, err)
	}
	if claim.JobID != resp.JobID || claim.State != "started" {
		t.Fatalf("recovery claim = %+v, response=%+v", claim, resp)
	}
}

func TestStalePrelaunchClaimRetiresProtectedHandoff(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nif [ \"$1\" = show ]; then printf 'not-found\\n'; exit 0; fi\nexit 64\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	id := "987654321"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.command, []byte("printf secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.log, paths.status, paths.started} {
		if err := os.WriteFile(path, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	claimPath := filepath.Join(claims, operationClaimName("stale-operation"))
	created := time.Now().Add(-2 * executorConnectionTimeout)
	claim := jobClaim{
		Version:     1,
		JobID:       id,
		Fingerprint: strings.Repeat("b", 64),
		State:       "launching",
		CreatedAt:   created.UTC().Format(time.RFC3339Nano),
	}
	if err := createJobClaim(claimPath, claim); err != nil {
		t.Fatal(err)
	}

	server := &Server{}
	if err := server.cleanupStalePrelaunchClaims(jobs, claims, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.command); !os.IsNotExist(err) {
		t.Fatalf("stale protected command handoff still exists; stat err=%v", err)
	}
	got, found, err := readJobClaim(claimPath)
	if err != nil || !found {
		t.Fatalf("read stale claim: found=%v err=%v", found, err)
	}
	if got.State != "failed" {
		t.Fatalf("stale claim state = %q, want failed", got.State)
	}
}
