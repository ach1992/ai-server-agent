package executor

import (
	"context"
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

func newSharedRuntimeTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	state, workspace := t.TempDir(), t.TempDir()
	provisionSharedWorkerHome(t, state)
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:   config.Config{StateDir: state, WorkspaceDir: workspace, WorkerUser: current.Username},
		token: "test-executor-token", guard: policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()), workerGID: uint32(os.Getegid()),
		runs: newRunLimiterWith(2, 1),
	}
	return s, state, workspace
}

func TestSharedRuntimeWorkerCommandCWDAndPrivateHome(t *testing.T) {
	s, state, root := newSharedRuntimeTestServer(t)
	scratch := filepath.Join(root, "scratch-no-git")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	// The unprivileged Go test process cannot set SysProcAttr.Credential
	// even to its own UID on this host. Inspect exact command construction;
	// first-installed root-executor execution is a separate acceptance gate.
	for _, tc := range []struct{ name, workspace, wantCWD string }{
		{"default", "", root}, {"scratch", scratch, scratch}, {"relative", "scratch-no-git", scratch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, err := s.workerCommandWorkspace(tc.workspace)
			if err != nil {
				t.Fatal(err)
			}
			cmd, _ := s.commandContext(context.Background(), Request{Command: "printf safe", Workspace: cwd})
			if cmd.Dir != tc.wantCWD {
				t.Fatalf("CWD %q want %q", cmd.Dir, tc.wantCWD)
			}
			home := filepath.Join(state, "worker-home")
			expected := []string{
				"HOME=" + home,
				"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
				"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
				"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
			}
			joined := strings.Join(cmd.Env, "\n")
			for _, want := range expected {
				if !strings.Contains(joined, want) {
					t.Fatalf("missing sanitized environment %q in %q", want, joined)
				}
			}
		})
	}
	invalid := s.runContext(context.Background(), Request{Command: "printf sentinel", Workspace: filepath.Dir(root)})
	if invalid.OK || invalid.ErrorCode != "invalid_workspace" {
		t.Fatalf("accepted outside workspace: %+v", invalid)
	}
	missing := s.runContext(context.Background(), Request{Command: "printf sentinel", Workspace: "missing"})
	if missing.OK || missing.ErrorCode != "invalid_workspace" {
		t.Fatalf("accepted missing workspace: %+v", missing)
	}
	badHome := filepath.Join(state, "worker-home")
	if err := os.Chmod(badHome, 0777); err != nil {
		t.Fatal(err)
	}
	rejected := s.runContext(context.Background(), Request{Command: "printf sentinel"})
	if rejected.OK || rejected.ErrorCode != "worker_home_unavailable" {
		t.Fatalf("untrusted HOME accepted: %+v", rejected)
	}
}

func TestSharedRuntimeMaterialFingerprintAndRootWorkspaceMismatch(t *testing.T) {
	s, _, root := newSharedRuntimeTestServer(t)
	scratch := filepath.Join(root, "scratch")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	base := Request{Command: "printf safe", Workspace: root, Kind: "task"}
	changed := []Request{
		{Command: base.Command, Workspace: scratch, Kind: "task"},
		{Command: base.Command, Workspace: root, Kind: "service", Label: "http"},
		{Command: base.Command, Workspace: root, Kind: "task", Root: true},
		{Command: "printf other", Workspace: root, Kind: "task"},
	}
	fp := s.jobFingerprint(base)
	if fp != s.jobFingerprint(Request{Command: base.Command, Workspace: root}) {
		t.Fatal("omitted task kind did not normalize")
	}
	for _, different := range changed {
		if fp == s.jobFingerprint(different) {
			t.Fatalf("material conflict not detected: %+v", different)
		}
	}
	denied := s.startJobBounded(Request{Command: "printf safe", Root: true, Approval: true, Workspace: scratch})
	if denied.OK || denied.ErrorCode != "invalid_workspace" {
		t.Fatalf("root+worker CWD accepted: %+v", denied)
	}
	if err := validateJobLabel("service", ""); err == nil {
		t.Fatal("unlabeled service accepted")
	}
	for _, bad := range []string{" secret", "secret with spaces", "a/b", strings.Repeat("a", 49)} {
		if err := validateJobLabel("service", bad); err == nil {
			t.Fatalf("untrusted service label %q", bad)
		}
	}
	if err := validateJobLabel("service", "dev-http_1"); err != nil {
		t.Fatal(err)
	}
}

func fixtureRuntimeClaim(t *testing.T, dir, id, owner, kind, workspace, created string) {
	t.Helper()
	claim := jobClaim{
		Version: 1, JobID: id, Fingerprint: "fixture-test", State: "started", CreatedAt: created,
		PrincipalID: owner, PrincipalClass: "direct", Workspace: workspace, Kind: kind, Label: "dev-api",
	}
	if err := createJobClaim(filepath.Join(dir, "job-"+id+".json"), claim); err != nil {
		t.Fatal(err)
	}
}

func mockActiveUnits(t *testing.T, ids ...string) {
	t.Helper()
	bin := t.TempDir()
	var lines strings.Builder
	for _, id := range ids {
		lines.WriteString(fmt.Sprintf("ai-job-%s.service loaded active running SECRET_COMMAND_SHOULD_NOT_LEAK\n", id))
	}
	script := "#!/bin/sh\ncase \"$1\" in\n list-units) printf '%s' " + shellQuote(lines.String()) + " ;;\n show) printf 'not-found\\n' ;;\n *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestSharedRuntimePassiveInventoryFiltersAndDoesNotMutate(t *testing.T) {
	s, state, root := newSharedRuntimeTestServer(t)
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	older := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	newer := time.Now().UTC().Format(time.RFC3339Nano)
	fixtureRuntimeClaim(t, claims, "101", "owner", "task", workspace, newer)
	fixtureRuntimeClaim(t, claims, "102", "owner", "service", workspace, newer)
	fixtureRuntimeClaim(t, claims, "103", "owner", "task", workspace, older)
	fixtureRuntimeClaim(t, claims, "999", "other", "service", workspace, newer)
	if err := os.WriteFile(filepath.Join(jobs, "103.status"), []byte("0\n"), 0640); err != nil {
		t.Fatal(err)
	}
	// Passive inventory must not consume or erase pending command handoffs.
	handoff := filepath.Join(jobs, "103.cmd")
	if err := os.WriteFile(handoff, []byte("SENSITIVE_PENDING_JOB_COMMAND"), 0600); err != nil {
		t.Fatal(err)
	}
	mockActiveUnits(t, "101", "102", "999")
	req := Request{PrincipalID: "owner", PrincipalClass: "direct", Workspace: workspace}
	result := s.runtimeList(req)
	if !result.OK || result.Truncated || result.RuntimeCapacity == nil || !result.RuntimeCapacity.StateReliable {
		t.Fatalf("runtime inventory failed: %+v", result)
	}
	if result.RuntimeCapacity.TasksActive != 1 || result.RuntimeCapacity.ServicesActive != 2 {
		t.Fatalf("global active capacity incorrect: %+v", result.RuntimeCapacity)
	}
	if len(result.RuntimeEntries) != 3 {
		t.Fatalf("principal-isolated inventory: %+v", result.RuntimeEntries)
	}
	if result.RuntimeEntries[0].Status != "active" || result.RuntimeEntries[1].Status != "active" ||
		result.RuntimeEntries[2].Status != "completed" {
		t.Fatalf("not deterministic active-first order: %+v", result.RuntimeEntries)
	}
	for _, item := range result.RuntimeEntries {
		if item.ID == "999" || strings.Contains(fmt.Sprintf("%+v", item), "SENSITIVE") {
			t.Fatalf("inventory leaked other principal/command: %+v", item)
		}
	}
	filtered := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct", Workspace: workspace, Kind: "service"})
	if !filtered.OK || len(filtered.RuntimeEntries) != 1 || filtered.RuntimeEntries[0].ID != "102" {
		t.Fatalf("service filtering mismatch: %+v", filtered)
	}
	absent := s.runtimeList(Request{PrincipalID: "other", PrincipalClass: "direct", Workspace: root})
	if !absent.OK || len(absent.RuntimeEntries) != 0 {
		t.Fatalf("workspace filtering leak: %+v", absent)
	}
	if body, err := os.ReadFile(handoff); err != nil || string(body) != "SENSITIVE_PENDING_JOB_COMMAND" {
		t.Fatalf("passive inventory mutated protected handoff: %q %v", body, err)
	}
}

func persistentAdmissionWithTestDisk(jobsDir string, taskLimit int, kind string, reserved *int64) *Response {
	return persistentJobAdmissionResponseWithReserveCheck(jobsDir, taskLimit, kind, func(_ string, required int64) (bool, int64, error) {
		if reserved != nil {
			*reserved = required
		}
		return true, 1 << 40, nil
	})
}

func TestSharedRuntimeServiceQuotaIndependentAndUnknownFailsClosed(t *testing.T) {
	s, state, root := newSharedRuntimeTestServer(t)
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	fixtureRuntimeClaim(t, claims, "11", "owner", "service", root, now)
	fixtureRuntimeClaim(t, claims, "12", "owner", "service", root, now)
	mockActiveUnits(t, "11", "12")
	if v := persistentAdmissionWithTestDisk(jobs, 8, "service", nil); v == nil || v.Status != "busy" || v.ErrorCode != "resource_limit" {
		t.Fatalf("service quota not enforced: %+v", v)
	}
	if v := persistentAdmissionWithTestDisk(jobs, 8, "task", nil); v != nil {
		t.Fatalf("services incorrectly exhausted task cap: %+v", v)
	}
	if err := os.Remove(filepath.Join(claims, "job-11.json")); err != nil {
		t.Fatal(err)
	}
	if v := persistentAdmissionWithTestDisk(jobs, 8, "task", nil); v == nil || v.ErrorCode != "job_state_unavailable" {
		t.Fatalf("unclassified active unit allowed admission: %+v", v)
	}
	unknown := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !unknown.OK || unknown.RuntimeCapacity == nil || unknown.RuntimeCapacity.StateReliable ||
		unknown.RuntimeCapacity.UnclassifiedActive != 1 {
		t.Fatalf("unknown active state fabricated reliable capacity: %+v", unknown)
	}
}

func TestRuntimeConfiguredTaskCapacityAndAdmission(t *testing.T) {
	s, state, root := newSharedRuntimeTestServer(t)
	jobs, claims := filepath.Join(state, "jobs"), filepath.Join(state, "jobs", "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for n := 1; n <= 6; n++ {
		fixtureRuntimeClaim(t, claims, fmt.Sprint(n), "owner", "task", root, now)
	}
	mockActiveUnits(t, "1", "2", "3", "4", "5", "6")
	if s.persistentTaskCapacity() != 6 {
		t.Fatal("default task limit changed")
	}
	cap6 := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !cap6.OK || cap6.RuntimeCapacity == nil || cap6.RuntimeCapacity.TaskLimit != 6 || cap6.RuntimeCapacity.ServiceLimit != 3 || cap6.RuntimeCapacity.SessionLimit != 8 || cap6.RuntimeCapacity.TasksActive != 6 ||
		cap6.RuntimeCapacity.WorkerLimit != 2 || cap6.RuntimeCapacity.WorkersActive != 0 || cap6.RuntimeCapacity.RootLimit != 1 || cap6.RuntimeCapacity.RootActive != 0 {
		t.Fatalf("default capacity not exposed: %+v", cap6)
	}
	workerRelease, admitted := s.runs.acquire(false)
	if !admitted {
		t.Fatal("test worker slot not available")
	}
	busyWorkers := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	workerRelease()
	if !busyWorkers.OK || busyWorkers.RuntimeCapacity == nil || busyWorkers.RuntimeCapacity.WorkersActive != 1 {
		t.Fatalf("active worker admission not reflected by runtime_list: %+v", busyWorkers)
	}
	if r := persistentAdmissionWithTestDisk(jobs, 6, "task", nil); r == nil || r.Status != "busy" {
		t.Fatalf("six active tasks over-admitted at default: %+v", r)
	}
	runtime := config.DefaultRuntimeSettings()
	runtime.PersistentTaskSlots = 8
	s.cfg.Runtime = &runtime
	if s.persistentTaskCapacity() != 8 {
		t.Fatal("override not effective")
	}
	cap8 := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !cap8.OK || cap8.RuntimeCapacity == nil || cap8.RuntimeCapacity.TaskLimit != 8 || cap8.RuntimeCapacity.TasksActive != 6 {
		t.Fatalf("override capacity not exposed: %+v", cap8)
	}
	if r := persistentAdmissionWithTestDisk(jobs, 8, "task", nil); r != nil {
		t.Fatalf("eight-slot task admission rejected 6 running: %+v", r)
	}
	for n := 7; n <= 8; n++ {
		fixtureRuntimeClaim(t, claims, fmt.Sprint(n), "owner", "task", root, now)
	}
	mockActiveUnits(t, "1", "2", "3", "4", "5", "6", "7", "8")
	if r := persistentAdmissionWithTestDisk(jobs, 8, "task", nil); r == nil || r.Status != "busy" {
		t.Fatalf("eight active tasks over-admitted: %+v", r)
	}
	if r := persistentAdmissionWithTestDisk(jobs, 8, "service", nil); r != nil {
		t.Fatalf("service admission incorrectly coupled to full task slots: %+v", r)
	}
}

// At the upper supported settings, 64 tasks and 64 services must remain
// independently admitted, inventoried and recoverable after a downscale.
func TestPersistentConfigured64TaskServiceInventoryAndDownscale(t *testing.T) {
	s, state, workspace := newSharedRuntimeTestServer(t)
	claims := filepath.Join(state, "jobs", "claims")
	if err := os.MkdirAll(claims, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(state, "jobs"), 0711); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ids := make([]string, 0, maxActivePersistentJobs+maxActivePersistentServices)
	for i := 1; i <= maxActivePersistentJobs; i++ {
		id := fmt.Sprint(i)
		fixtureRuntimeClaim(t, claims, id, "owner", "task", workspace, now)
		ids = append(ids, id)
	}
	for i := 1; i < maxActivePersistentServices; i++ {
		id := fmt.Sprint(maxActivePersistentJobs + i)
		fixtureRuntimeClaim(t, claims, id, "owner", "service", workspace, now)
		ids = append(ids, id)
	}
	runtime := config.DefaultRuntimeSettings()
	runtime.PersistentTaskSlots = 64
	runtime.PersistentServiceSlots = 64
	s.cfg.Runtime = &runtime
	mockActiveUnits(t, ids...)
	jobs := filepath.Join(state, "jobs")
	checkDisk := func(_ string, _ int64) (bool, int64, error) { return true, 1 << 40, nil }
	if got := persistentJobAdmissionResponseWithReserveCheckAndLimits(jobs, 64, 64, "service", checkDisk); got != nil {
		t.Fatalf("64th service refused when tasks full but services still have room: %+v", got)
	}
	if got := persistentJobAdmissionResponseWithReserveCheckAndLimits(jobs, 64, 64, "task", checkDisk); got == nil || got.ErrorCode != "resource_limit" {
		t.Fatalf("65th task not blocked while service has room: %+v", got)
	}
	id := fmt.Sprint(maxActivePersistentJobs + maxActivePersistentServices)
	fixtureRuntimeClaim(t, claims, id, "owner", "service", workspace, now)
	ids = append(ids, id)
	mockActiveUnits(t, ids...)
	got := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !got.OK || got.Truncated || got.RuntimeCapacity == nil || !got.RuntimeCapacity.StateReliable ||
		got.RuntimeCapacity.TaskLimit != 64 || got.RuntimeCapacity.ServiceLimit != 64 ||
		got.RuntimeCapacity.TasksActive != 64 || got.RuntimeCapacity.ServicesActive != 64 ||
		len(got.RuntimeEntries) != 128 {
		t.Fatalf("complete 128-job inventory lost or truncated: %+v", got)
	}
	for _, kind := range []string{"task", "service"} {
		if blocked := persistentJobAdmissionResponseWithReserveCheckAndLimits(jobs, 64, 64, kind, checkDisk); blocked == nil || blocked.ErrorCode != "resource_limit" || blocked.Status != "busy" {
			t.Fatalf("%s 65th overadmission: %+v", kind, blocked)
		}
	}
	runtime.PersistentTaskSlots, runtime.PersistentServiceSlots = 1, 1
	after := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !after.OK || after.Truncated || !after.RuntimeCapacity.StateReliable ||
		after.RuntimeCapacity.TaskLimit != 1 || after.RuntimeCapacity.ServiceLimit != 1 ||
		after.RuntimeCapacity.TasksActive != 64 || after.RuntimeCapacity.ServicesActive != 64 {
		t.Fatalf("downscale hid surviving jobs: %+v", after)
	}
	want := jobStateSafetyReserveBytesForLimits(64, 64)
	if want != 1600<<20 {
		t.Fatalf("unexpected max-slot disk reserve: %d", want)
	}
	for _, kind := range []string{"task", "service"} {
		var reserve int64
		blocked := persistentJobAdmissionResponseWithReserveCheckAndLimits(jobs, 1, 1, kind,
			func(_ string, needed int64) (bool, int64, error) { reserve = needed; return false, needed - 1, nil })
		// The admission limit is reached before disk inspection, and independently
		// reports resource_limit rather than pretending a smaller live count.
		if blocked == nil || blocked.Status != "busy" || reserve != 0 {
			t.Fatalf("downscaled %s admission did not fail early: %+v reserve=%d", kind, blocked, reserve)
		}
	}
	if audit := jobAuditSafetyReserveAtStartup(s.cfg); audit != want {
		t.Fatalf("downscale audit reserve %d must include surviving 128 jobs (%d)", audit, want)
	}
}

func TestPersistentServiceConfiguredAdmissionDiskAndAuditFallback(t *testing.T) {
	s, state, workspace := newSharedRuntimeTestServer(t)
	claims := filepath.Join(state, "jobs", "claims")
	if err := os.MkdirAll(claims, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(state, "jobs"), 0711); err != nil {
		t.Fatal(err)
	}
	runtime := config.DefaultRuntimeSettings()
	s.cfg.Runtime = &runtime
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mockActiveUnits(t)
	if got := jobAuditSafetyReserveAtStartup(s.cfg); got != jobStateSafetyReserveBytesForLimits(6, 3) {
		t.Fatalf("healthy default audit reserve %d ignored low configured capacity", got)
	}
	ids := []string{"1", "2"}
	for _, id := range ids {
		fixtureRuntimeClaim(t, claims, id, "owner", "service", workspace, now)
	}
	mockActiveUnits(t, ids...)
	jobs := filepath.Join(state, "jobs")
	var gotReserve int64
	canAdmit := persistentJobAdmissionResponseWithReserveCheckAndLimits(jobs, 6, 3, "service",
		func(_ string, required int64) (bool, int64, error) { gotReserve = required; return true, required, nil })
	if canAdmit != nil || gotReserve != jobStateSafetyReserveBytesForLimits(6, 3) {
		t.Fatalf("third service refused or reservation wrong: %+v reserve=%d", canAdmit, gotReserve)
	}
	fixtureRuntimeClaim(t, claims, "3", "owner", "service", workspace, now)
	mockActiveUnits(t, "1", "2", "3")
	if blocked := persistentJobAdmissionResponseWithReserveCheckAndLimits(jobs, 6, 3, "service",
		func(_ string, required int64) (bool, int64, error) { return true, required, nil }); blocked == nil || blocked.Status != "busy" {
		t.Fatalf("fourth service admitted: %+v", blocked)
	}
	runtime.PersistentServiceSlots = 1
	if got := jobAuditSafetyReserveAtStartup(s.cfg); got != jobStateSafetyReserveBytesForLimits(6, 3) {
		t.Fatalf("audit reserve lost service high-water after downscale: %d", got)
	}
	mockBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(mockBin, "systemctl"), []byte(`#!/bin/sh
exit 9
`), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", mockBin+":"+os.Getenv("PATH"))
	if got := jobAuditSafetyReserveAtStartup(s.cfg); got != jobStateSafetyReserveBytesForLimits(64, 64) {
		t.Fatalf("unknown systemd inventory lowered audit safety reserve: %d", got)
	}
}

// Runtime setting changes restart the Executor, not the existing transient
// systemd units. Old tasks must keep their log reserve, but cannot steal the
// default independent persistent-service admissions after a downscale.
func TestPersistentTaskDownscalePreservesIndependentServicesAndReserve(t *testing.T) {
	s, state, workspace := newSharedRuntimeTestServer(t)
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.MkdirAll(claims, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ids := make([]string, 0, maxActivePersistentJobs+maxActivePersistentServices)
	for i := 1; i <= maxActivePersistentJobs; i++ {
		id := fmt.Sprint(i)
		fixtureRuntimeClaim(t, claims, id, "owner", "task", workspace, now)
		ids = append(ids, id)
	}
	mockActiveUnits(t, ids...)
	runtime := config.DefaultRuntimeSettings()
	runtime.PersistentTaskSlots = maxActivePersistentJobs
	s.cfg.Runtime = &runtime
	before := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !before.OK || before.RuntimeCapacity == nil || !before.RuntimeCapacity.StateReliable ||
		before.RuntimeCapacity.TaskLimit != maxActivePersistentJobs || before.RuntimeCapacity.TasksActive != maxActivePersistentJobs {
		t.Fatalf("baseline capacity not reliably observed: %+v", before)
	}
	runtime.PersistentTaskSlots = 1
	after := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !after.OK || after.RuntimeCapacity == nil || !after.RuntimeCapacity.StateReliable ||
		after.RuntimeCapacity.TaskLimit != 1 || after.RuntimeCapacity.TasksActive != maxActivePersistentJobs ||
		after.RuntimeCapacity.ServicesActive != 0 || after.RuntimeCapacity.ServiceLimit != 3 {
		t.Fatalf("runtime_list lost overcommitted tasks: %+v", after)
	}
	if got := persistentJobAdmissionResponseWithLimit(jobs, 1, "task"); got == nil ||
		got.ErrorCode != "resource_limit" || got.Status != "busy" {
		t.Fatalf("new task admitted after downscale: %+v", got)
	}
	// Sixty-four bounded logs can survive a downscale, but a small host with
	// the default limits must not reserve storage for 128 unrequested jobs.
	full := jobStateSafetyReserveBytesFor(maxActivePersistentJobs)
	if full != 1112<<20 || jobStateSafetyReserveBytesFor(1) != 608<<20 ||
		jobStateSafetyReserveBytesForActive(1, maxActivePersistentJobs) != full ||
		jobStateSafetyReserveBytesForActive(maxActivePersistentJobs, 1) != full {
		t.Fatalf("reserve fell below old running task exposure: %d", jobStateSafetyReserveBytesForActive(1, maxActivePersistentJobs))
	}
	var pressureReserve int64
	pressure := persistentJobAdmissionResponseWithReserveCheck(jobs, 1, "service", func(_ string, required int64) (bool, int64, error) {
		pressureReserve = required
		return false, required - 1, nil
	})
	if pressureReserve != full || pressure == nil || pressure.Status != "disk_pressure" ||
		pressure.ErrorCode != "resource_limit" {
		t.Fatalf("overcommitted task logs bypassed disk pressure reserve: reserve=%d resp=%+v", pressureReserve, pressure)
	}
	for service := 1; service <= runtime.PersistentServiceSlots; service++ {
		var required int64
		if got := persistentAdmissionWithTestDisk(jobs, 1, "service", &required); got != nil {
			t.Fatalf("service %d blocked by old active tasks: %+v", service, got)
		}
		if required != full {
			t.Fatalf("service %d reserve %d ignores 64 active tasks, want %d", service, required, full)
		}
		id := fmt.Sprint(maxActivePersistentJobs + service)
		fixtureRuntimeClaim(t, claims, id, "owner", "service", workspace, now)
		ids = append(ids, id)
		mockActiveUnits(t, ids...)
		cap := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
		if !cap.OK || cap.RuntimeCapacity == nil || !cap.RuntimeCapacity.StateReliable ||
			cap.RuntimeCapacity.TasksActive != maxActivePersistentJobs || cap.RuntimeCapacity.ServicesActive != service ||
			cap.RuntimeCapacity.TaskLimit != 1 {
			t.Fatalf("service %d inventory wrong after downscale: %+v", service, cap)
		}
	}
	if got := persistentJobAdmissionResponseWithLimit(jobs, 1, "service"); got == nil ||
		got.ErrorCode != "resource_limit" || got.Status != "busy" {
		t.Fatalf("fourth service admitted: %+v", got)
	}
	startedPath := filepath.Join(claims, "job-1.json")
	started, found, err := readJobClaim(startedPath)
	if err != nil || !found {
		t.Fatalf("read started claim: %t %v", found, err)
	}
	existing := s.resumeClaimedJob(Request{Kind: "task"}, jobs, startedPath, started, started.Fingerprint)
	if !existing.OK || !existing.IdempotentReplay || existing.Status != "accepted" || existing.JobID != started.JobID {
		t.Fatalf("already active task replay not idempotent: %+v", existing)
	}
	pendingID := fmt.Sprint(maxActivePersistentJobs + maxActivePersistentServices + 11)
	pending := jobClaim{Version: 1, JobID: pendingID, Fingerprint: "fixture-test", State: "claimed",
		CreatedAt: now, PrincipalID: "owner", PrincipalClass: "direct", Workspace: workspace, Kind: "task"}
	pendingPath := filepath.Join(claims, "job-"+pendingID+".json")
	if err := createJobClaim(pendingPath, pending); err != nil {
		t.Fatal(err)
	}
	blocked := s.resumeClaimedJob(Request{Kind: "task"}, jobs, pendingPath, pending, pending.Fingerprint)
	if blocked.OK || blocked.ErrorCode != "resource_limit" || blocked.Status != "busy" || !blocked.IdempotentReplay {
		t.Fatalf("pending task replay bypassed new limit: %+v", blocked)
	}
	retained, found, err := readJobClaim(pendingPath)
	if err != nil || !found || retained.State != "claimed" || retained.Fingerprint != pending.Fingerprint {
		t.Fatalf("blocked replay corrupted durable claim: %+v found=%t err=%v", retained, found, err)
	}
}

// A successful systemctl stop is only a request; state must prove that the
// exact unit is no longer active before advertising terminal completion.
func TestSharedRuntimeStopRequiresVerifiedUnitTermination(t *testing.T) {
	s, _, _ := newSharedRuntimeTestServer(t)
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{"still_running", true}, {"confirmed_stopped", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			script := "#!/bin/sh\ncase \"$1\" in\n stop) exit 0 ;;\n list-units) "
			if tc.active {
				script += "printf 'ai-job-123.service loaded active running fake\\n' "
			}
			script += ";;\n *) exit 67 ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			got := s.jobStop(Request{JobID: "123"})
			if tc.active {
				if got.OK || got.ErrorCode != "unknown_completion" || got.Status != "unknown" {
					t.Fatalf("systemctl accepted stop with unit still active: %+v", got)
				}
			} else if !got.OK || got.Status != "stopped" {
				t.Fatalf("stopped unit not positively verified: %+v", got)
			}
		})
	}
}

func TestSharedRuntimePassiveDirectoryListingIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("fixture-%d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	names, overflow, err := readDirBounded(dir, 3)
	if err != nil || !overflow || len(names) != 3 {
		t.Fatalf("bounded directory enumeration: len=%d overflow=%t err=%v", len(names), overflow, err)
	}
	// Enumeration does not modify any file, even when the directory exceeds
	// the contract's memory/capacity bound.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 6 {
		t.Fatalf("inventory mutated entries: %d %v", len(entries), err)
	}
}
