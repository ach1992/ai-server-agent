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
	systemctlScript := "#!/bin/sh\ncase \"$1\" in\n  list-units) printf 'ai-job-test.service loaded active running test\\n'; exit 0 ;;\n  show) printf 'loaded\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"
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

func TestPersistentJobDefinitiveLaunchFailureRemovesPreparedArtifacts(t *testing.T) {
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
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nprintf 'definitive launch failure\\n' >&2\nexit 1\n"), 0755); err != nil {
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
	req := Request{Command: "printf never-ran", OperationID: "definitive-launch-failure"}
	resp := server.startJobBounded(req)
	if resp.OK || resp.ErrorCode != "job_start_failed" || resp.JobID == "" {
		t.Fatalf("definitive launch failure = %+v", resp)
	}
	paths := jobPathsFor(jobs, resp.JobID)
	for _, path := range []string{paths.command, paths.log, paths.status, paths.started} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("definitive launch artifact still exists: %s (err=%v)", path, err)
		}
	}
	claim, found, err := readJobClaim(filepath.Join(jobs, "claims", operationClaimName(req.OperationID)))
	if err != nil || !found {
		t.Fatalf("read failed launch claim: found=%v err=%v", found, err)
	}
	if claim.State != "failed" {
		t.Fatalf("failed launch claim state = %q, want failed", claim.State)
	}
}

func TestPersistentJobUnknownLaunchOutcomeKeepsRecoverableClaim(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	systemctlScript := "#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'systemd state unavailable\\n' >&2; exit 2 ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(systemctl, []byte(systemctlScript), 0755); err != nil {
		t.Fatal(err)
	}
	systemdRun := filepath.Join(fakeBin, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nprintf 'launch transport failed\\n' >&2\nexit 1\n"), 0755); err != nil {
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
	req := Request{Command: "printf uncertain", OperationID: "unknown-launch"}
	resp := server.startJobBounded(req)
	if resp.OK || resp.ErrorCode != "unknown_completion" || resp.JobID == "" {
		t.Fatalf("unknown launch response = %+v", resp)
	}
	claimPath := filepath.Join(jobs, "claims", operationClaimName(req.OperationID))
	claim, found, err := readJobClaim(claimPath)
	if err != nil || !found {
		t.Fatalf("read unknown launch claim: found=%v err=%v", found, err)
	}
	if claim.State != "launching" || claim.JobID != resp.JobID {
		t.Fatalf("unknown launch claim = %+v, response=%+v", claim, resp)
	}
	if _, err := os.Stat(filepath.Join(jobs, resp.JobID+".cmd")); err != nil {
		t.Fatalf("unknown launch must preserve protected handoff for safe reconciliation: %v", err)
	}
}

func TestPersistentJobClaimReplayCannotBypassActiveCapacity(t *testing.T) {
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
	activeLines := strings.Repeat("ai-job-active.service loaded active running test\n", maxActivePersistentJobs)
	systemctlScript := "#!/bin/sh\ncase \"$1\" in\n  list-units) printf '%s' '" + activeLines + "'; exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(systemctl, []byte(systemctlScript), 0755); err != nil {
		t.Fatal(err)
	}
	launchMarker := filepath.Join(t.TempDir(), "launched")
	systemdRun := filepath.Join(fakeBin, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\ntouch "+shellQuote(launchMarker)+"\nexit 0\n"), 0755); err != nil {
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
	req := Request{Command: "printf recovered", OperationID: "capacity-replay"}
	claim := jobClaim{
		Version:     1,
		JobID:       "123456788",
		Fingerprint: server.jobFingerprint(req),
		State:       "claimed",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	claimPath := filepath.Join(claims, operationClaimName(req.OperationID))
	if err := createJobClaim(claimPath, claim); err != nil {
		t.Fatal(err)
	}

	resp := server.startJobBounded(req)
	if resp.OK || resp.ErrorCode != "resource_limit" || resp.Status != "busy" || !resp.Retryable || !resp.IdempotentReplay || resp.JobID != claim.JobID {
		t.Fatalf("capacity replay response = %+v", resp)
	}
	if _, err := os.Stat(launchMarker); !os.IsNotExist(err) {
		t.Fatalf("capacity-limited replay invoked systemd-run; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(jobs, claim.JobID+".cmd")); !os.IsNotExist(err) {
		t.Fatalf("claimed replay left a protected handoff while waiting for capacity; err=%v", err)
	}
	recovered, found, err := readJobClaim(claimPath)
	if err != nil || !found {
		t.Fatalf("read capacity-limited claim: found=%v err=%v", found, err)
	}
	if recovered.State != "claimed" {
		t.Fatalf("capacity-limited claim state = %q, want claimed", recovered.State)
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

func TestPersistentJobReplayPreservesUnknownTerminalState(t *testing.T) {
	useInactiveSystemctl(t)
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       config.Config{StateDir: state},
		token:     "test-executor-token",
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
	}
	req := Request{Command: "printf maybe", OperationID: "unknown-terminal-replay"}
	claim := jobClaim{
		Version:     1,
		JobID:       "123456787",
		Fingerprint: server.jobFingerprint(req),
		State:       "started",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := createJobClaim(filepath.Join(claims, operationClaimName(req.OperationID)), claim); err != nil {
		t.Fatal(err)
	}
	paths := jobPathsFor(jobs, claim.JobID)
	if err := os.WriteFile(paths.status, []byte(jobStatusUnknown+"\n"), 0640); err != nil {
		t.Fatal(err)
	}

	resp := server.startJobBounded(req)
	if resp.OK || resp.ErrorCode != "unknown_completion" || resp.Status != "unknown" || resp.JobID != claim.JobID || !resp.IdempotentReplay {
		t.Fatalf("unknown terminal replay = %+v", resp)
	}
}

func TestPersistentJobReplayReportsCompletedStatus(t *testing.T) {
	useInactiveSystemctl(t)
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       config.Config{StateDir: state},
		token:     "test-executor-token",
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
	}
	req := Request{Command: "exit 7", OperationID: "completed-terminal-replay"}
	claim := jobClaim{
		Version:     1,
		JobID:       "123456786",
		Fingerprint: server.jobFingerprint(req),
		State:       "started",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := createJobClaim(filepath.Join(claims, operationClaimName(req.OperationID)), claim); err != nil {
		t.Fatal(err)
	}
	paths := jobPathsFor(jobs, claim.JobID)
	if err := os.WriteFile(paths.status, []byte("7\n"), 0640); err != nil {
		t.Fatal(err)
	}

	resp := server.startJobBounded(req)
	if !resp.OK || resp.Status != "completed" || resp.ExitCode != 7 || resp.JobID != claim.JobID || !resp.IdempotentReplay {
		t.Fatalf("completed terminal replay = %+v", resp)
	}
}

func TestFailedJobClaimCleanupRemovesPreparedArtifacts(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	id := "123450001"
	paths := jobPathsFor(jobs, id)
	for _, path := range []string{paths.log, paths.status, paths.started} {
		if err := os.WriteFile(path, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.command, []byte("printf stale-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(claims, operationClaimName("failed-cleanup"))
	if err := createJobClaim(claimPath, jobClaim{
		Version:     1,
		JobID:       id,
		Fingerprint: strings.Repeat("e", 64),
		State:       "failed",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := cleanupFailedJobClaims(jobs, claims); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.command, paths.log, paths.status, paths.started} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed-claim artifact still exists: %s (err=%v)", path, err)
		}
	}
	if _, err := os.Stat(claimPath); err != nil {
		t.Fatalf("bounded failed claim should remain for idempotent replay: %v", err)
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

func TestInterruptedPersistentJobsEnterBoundedRetention(t *testing.T) {
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
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	base := time.Now().Add(-time.Duration(maxCompletedJobArtifacts+2) * time.Second)
	allIDs := make([]string, 0, maxCompletedJobArtifacts+1)
	for i := 0; i < maxCompletedJobArtifacts+1; i++ {
		id := fmt.Sprintf("%d", 700000+i)
		allIDs = append(allIDs, id)
		paths := jobPathsFor(jobs, id)
		if err := os.WriteFile(paths.log, nil, 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.status, nil, 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.started, []byte("started\n"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.command, []byte("printf interrupted"), 0600); err != nil {
			t.Fatal(err)
		}
		stamp := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(paths.status, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		claimPath := filepath.Join(claims, operationClaimName("interrupted-"+id))
		if err := createJobClaim(claimPath, jobClaim{
			Version:     1,
			JobID:       id,
			Fingerprint: strings.Repeat("c", 64),
			State:       "started",
			CreatedAt:   stamp.UTC().Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := server.cleanupCompletedJobArtifacts(jobs, claims); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(jobs)
	if err != nil {
		t.Fatal(err)
	}
	statusCount := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".status") {
			statusCount++
			b, err := os.ReadFile(filepath.Join(jobs, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(b)) != jobStatusUnknown {
				t.Fatalf("retained interrupted status %s = %q, want %q", entry.Name(), b, jobStatusUnknown)
			}
		}
	}
	if statusCount != maxCompletedJobArtifacts {
		t.Fatalf("retained terminal status files = %d, want %d", statusCount, maxCompletedJobArtifacts)
	}
	removedID := ""
	for _, id := range allIDs {
		if _, err := os.Stat(filepath.Join(jobs, id+".status")); os.IsNotExist(err) {
			if removedID != "" {
				t.Fatalf("more than one terminal job was evicted: %s and %s", removedID, id)
			}
			removedID = id
		}
	}
	if removedID == "" {
		t.Fatal("expected one interrupted terminal job to be evicted")
	}
	for _, path := range []string{
		filepath.Join(jobs, removedID+".cmd"),
		filepath.Join(jobs, removedID+".log"),
		filepath.Join(jobs, removedID+".status"),
		filepath.Join(jobs, removedID+".started"),
		filepath.Join(claims, operationClaimName("interrupted-"+removedID)),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("evicted interrupted artifact still exists: %s (err=%v)", path, err)
		}
	}
}

func TestActivePersistentJobIsNotRetiredAsUnknown(t *testing.T) {
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
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\ncase \"$1\" in\n  list-units) printf 'ai-job-812345.service loaded active running test\\n'; exit 0 ;;\n  show) printf 'loaded\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	id := "812345"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.log, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.status, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.started, []byte("started\n"), 0640); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	if err := server.cleanupCompletedJobArtifacts(jobs, claims); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(paths.status)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Fatalf("active job status was rewritten: %q", b)
	}
}

func TestJobStatusReconcilesInterruptedJobWithoutExitStatus(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'loaded\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	id := "798"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.status, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.started, []byte("started\n"), 0640); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	resp := server.jobStatus(Request{JobID: id})
	if resp.OK || resp.ErrorCode != "unknown_completion" || resp.Status != "unknown" || resp.JobID != id {
		t.Fatalf("reconciled interrupted status = %+v", resp)
	}
	b, err := os.ReadFile(paths.status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != jobStatusUnknown {
		t.Fatalf("reconciled status marker = %q, want %q", b, jobStatusUnknown)
	}
}

func TestJobStatusReconcilesAcceptedJobBeforeRunnerStart(t *testing.T) {
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
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	id := "797"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.status, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.started, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.command, []byte("printf secret"), 0600); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(claims, operationClaimName("accepted-before-runner"))
	if err := createJobClaim(claimPath, jobClaim{
		Version:     1,
		JobID:       id,
		Fingerprint: strings.Repeat("d", 64),
		State:       "started",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	resp := server.jobStatus(Request{JobID: id})
	if resp.OK || resp.ErrorCode != "unknown_completion" || resp.Status != "unknown" || resp.JobID != id {
		t.Fatalf("accepted-before-runner status = %+v", resp)
	}
	if _, err := os.Stat(paths.command); !os.IsNotExist(err) {
		t.Fatalf("accepted-before-runner command handoff still exists; err=%v", err)
	}
	b, err := os.ReadFile(paths.status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != jobStatusUnknown {
		t.Fatalf("accepted-before-runner status marker = %q, want %q", b, jobStatusUnknown)
	}
}

func TestJobStatusReportsUnknownCompletionMarker(t *testing.T) {
	useInactiveSystemctl(t)
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(jobs, "799.status")
	if err := os.WriteFile(statusPath, []byte(jobStatusUnknown+"\n"), 0640); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	resp := server.jobStatus(Request{JobID: "799"})
	if resp.OK || resp.ErrorCode != "unknown_completion" || resp.ReasonCode != "unknown_completion" || resp.Status != "unknown" || resp.JobID != "799" {
		t.Fatalf("unknown job status = %+v", resp)
	}
}

func TestJobStatusReturnsStructuredExitCode(t *testing.T) {
	useInactiveSystemctl(t)
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

	server := &Server{workerUID: uint32(os.Geteuid())}
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
