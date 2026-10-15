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
	script := "#!/bin/sh\ncase \"$1\" in\n list-units) printf '%s' " + shellQuote(lines.String()) + " ;;\n *) exit 64 ;;\nesac\n"
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
	if v := persistentJobAdmissionResponse(jobs, "service"); v == nil || v.Status != "busy" || v.ErrorCode != "resource_limit" {
		t.Fatalf("service quota not enforced: %+v", v)
	}
	if v := persistentJobAdmissionResponse(jobs, "task"); v != nil {
		t.Fatalf("services incorrectly exhausted task cap: %+v", v)
	}
	if err := os.Remove(filepath.Join(claims, "job-11.json")); err != nil {
		t.Fatal(err)
	}
	if v := persistentJobAdmissionResponse(jobs, "task"); v == nil || v.ErrorCode != "job_state_unavailable" {
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
	for n := 1; n <= 4; n++ {
		fixtureRuntimeClaim(t, claims, fmt.Sprint(n), "owner", "task", root, now)
	}
	mockActiveUnits(t, "1", "2", "3", "4")
	if s.persistentTaskCapacity() != 4 {
		t.Fatal("default task limit changed")
	}
	cap4 := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !cap4.OK || cap4.RuntimeCapacity == nil || cap4.RuntimeCapacity.TaskLimit != 4 || cap4.RuntimeCapacity.TasksActive != 4 {
		t.Fatalf("default capacity not exposed: %+v", cap4)
	}
	if r := persistentJobAdmissionResponseWithLimit(jobs, 4, "task"); r == nil || r.Status != "busy" {
		t.Fatalf("four active tasks over-admitted at default: %+v", r)
	}
	runtime := config.DefaultRuntimeSettings()
	runtime.PersistentTaskSlots = 8
	s.cfg.Runtime = &runtime
	if s.persistentTaskCapacity() != 8 {
		t.Fatal("override not effective")
	}
	cap8 := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct"})
	if !cap8.OK || cap8.RuntimeCapacity == nil || cap8.RuntimeCapacity.TaskLimit != 8 || cap8.RuntimeCapacity.TasksActive != 4 {
		t.Fatalf("override capacity not exposed: %+v", cap8)
	}
	if r := persistentJobAdmissionResponseWithLimit(jobs, 8, "task"); r != nil {
		t.Fatalf("eight-slot task admission rejected 4 running: %+v", r)
	}
	for n := 5; n <= 8; n++ {
		fixtureRuntimeClaim(t, claims, fmt.Sprint(n), "owner", "task", root, now)
	}
	mockActiveUnits(t, "1", "2", "3", "4", "5", "6", "7", "8")
	if r := persistentJobAdmissionResponseWithLimit(jobs, 8, "task"); r == nil || r.Status != "busy" {
		t.Fatalf("eight active tasks over-admitted: %+v", r)
	}
	if r := persistentJobAdmissionResponseWithLimit(jobs, 8, "service"); r != nil {
		t.Fatalf("service admission incorrectly coupled to full task slots: %+v", r)
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
