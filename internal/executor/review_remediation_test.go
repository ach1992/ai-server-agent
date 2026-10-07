package executor

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

func useInactiveSystemctl(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
}

func TestPersistentJobFailedSystemdRunLoadedInactiveIsUnknown(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	script := "#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'loaded\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(systemctl, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	systemdRun := filepath.Join(fakeBin, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nprintf 'launch failed after unit load\\n' >&2\nexit 1\n"), 0755); err != nil {
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
	resp := server.startJobBounded(Request{Command: "printf uncertain", OperationID: "loaded-inactive-launch"})
	if resp.OK || resp.ErrorCode != "unknown_completion" || resp.Status != "unknown" || resp.JobID == "" {
		t.Fatalf("loaded-inactive failed launch = %+v", resp)
	}
	paths := jobPathsFor(jobs, resp.JobID)
	b, err := os.ReadFile(paths.status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != jobStatusUnknown {
		t.Fatalf("loaded-inactive status = %q, want %q", b, jobStatusUnknown)
	}
	if _, err := os.Stat(paths.command); !os.IsNotExist(err) {
		t.Fatalf("terminal unknown command handoff still exists; err=%v", err)
	}
}

func TestWorkerTerminalMarkerDoesNotOverrideActiveUnit(t *testing.T) {
	for _, statusValue := range []string{"7\n", jobStatusUnknown + "\n"} {
		t.Run(strings.TrimSpace(statusValue), func(t *testing.T) {
			state := t.TempDir()
			jobs := filepath.Join(state, "jobs")
			claims := filepath.Join(jobs, "claims")
			if err := os.Mkdir(jobs, 0711); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(claims, 0700); err != nil {
				t.Fatal(err)
			}
			id := "812399"
			fakeBin := t.TempDir()
			systemctl := filepath.Join(fakeBin, "systemctl")
			script := "#!/bin/sh\ncase \"$1\" in\n  list-units) printf 'ai-job-" + id + ".service loaded active running test\\n'; exit 0 ;;\n  show) printf 'ActiveState=active\\nSubState=running\\nExecMainStatus=0\\nMainPID=123\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"
			if err := os.WriteFile(systemctl, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
			paths := jobPathsFor(jobs, id)
			if err := os.WriteFile(paths.status, []byte(statusValue), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.started, []byte("started\n"), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.log, nil, 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.command, []byte("printf still-running"), 0600); err != nil {
				t.Fatal(err)
			}
			server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
			resp := server.jobStatus(Request{JobID: id})
			if resp.Status == "completed" || resp.ErrorCode == "unknown_completion" {
				t.Fatalf("active unit was terminalized from worker marker %q: %+v", strings.TrimSpace(statusValue), resp)
			}
			if _, err := os.Stat(paths.command); err != nil {
				t.Fatalf("active job command handoff was retired from worker marker: %v", err)
			}
			if err := server.cleanupCompletedJobArtifacts(jobs, claims); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(paths.command); err != nil {
				t.Fatalf("retention evicted active job from worker marker: %v", err)
			}
			got, err := os.ReadFile(paths.status)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != statusValue {
				t.Fatalf("active status marker changed from %q to %q", statusValue, got)
			}
		})
	}
}

func TestRunJobHelperRetiresCommandOnLogSetupFailure(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "job.cmd")
	logPath := filepath.Join(dir, "job.log")
	statusPath := filepath.Join(dir, "job.status")
	startedPath := filepath.Join(dir, "job.started")
	if err := os.WriteFile(commandPath, []byte("printf secret-log-failure"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing-log-target"), logPath); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{statusPath, startedPath} {
		if err := os.WriteFile(path, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	if code := RunJobHelper([]string{
		"--command-file", commandPath,
		"--log-file", logPath,
		"--status-file", statusPath,
		"--started-file", startedPath,
	}); code != jobRunnerFailureExit {
		t.Fatalf("job helper exit = %d, want %d", code, jobRunnerFailureExit)
	}
	if _, err := os.Stat(commandPath); !os.IsNotExist(err) {
		t.Fatalf("command handoff survived log setup failure; err=%v", err)
	}
	b, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(jobRunnerFailureExit) {
		t.Fatalf("terminal runner status = %q", b)
	}
}

func TestRunJobHelperRetiresCommandOnStartedMarkerFailure(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "job.cmd")
	logPath := filepath.Join(dir, "job.log")
	statusPath := filepath.Join(dir, "job.status")
	startedPath := filepath.Join(dir, "job.started")
	if err := os.WriteFile(commandPath, []byte("printf secret-start-failure"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{logPath, statusPath} {
		if err := os.WriteFile(path, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "missing-start-target"), startedPath); err != nil {
		t.Fatal(err)
	}
	if code := RunJobHelper([]string{
		"--command-file", commandPath,
		"--log-file", logPath,
		"--status-file", statusPath,
		"--started-file", startedPath,
	}); code != jobRunnerFailureExit {
		t.Fatalf("job helper exit = %d, want %d", code, jobRunnerFailureExit)
	}
	if _, err := os.Stat(commandPath); !os.IsNotExist(err) {
		t.Fatalf("command handoff survived start-marker failure; err=%v", err)
	}
	b, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(jobRunnerFailureExit) {
		t.Fatalf("terminal runner status = %q", b)
	}
}

func TestRunJobHelperRetiresRejectedCommandHandoff(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	id := "812450"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.command, []byte("printf validation-secret"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.log, paths.status, paths.started} {
		if err := os.WriteFile(path, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	if code := RunJobHelper([]string{
		"--command-file", paths.command,
		"--log-file", paths.log,
		"--status-file", paths.status,
		"--started-file", paths.started,
	}); code != jobRunnerFailureExit {
		t.Fatalf("job helper exit = %d, want %d", code, jobRunnerFailureExit)
	}
	if _, err := os.Stat(paths.command); !os.IsNotExist(err) {
		t.Fatalf("validation-failed command handoff survived runner failure; err=%v", err)
	}
	b, err := os.ReadFile(paths.status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(jobRunnerFailureExit) {
		t.Fatalf("terminal runner status = %q", b)
	}
}

func TestRunnerCommandRetirementScrubsWhenUnlinkDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unlink-denial scrub test requires a non-root test user")
	}
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "job.cmd")
	secret := []byte("printf must-not-remain-on-disk")
	if err := os.WriteFile(commandPath, secret, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0511); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if err := retireRunnerCommandHandoff(commandPath); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(commandPath)
	if err != nil {
		t.Fatalf("expected scrubbed path to remain when unlink is denied: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("scrubbed command handoff size = %d, want 0", fi.Size())
	}
}

func TestJobStatusSurfacesTerminalCommandCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denial cleanup test requires a non-root test user")
	}
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	id := "812451"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.status, []byte("125\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.command, []byte("printf retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jobs, 0511); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(jobs, 0711)
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nif [ \"$1\" = list-units ]; then exit 0; fi\nexit 64\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	server := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	resp := server.jobStatus(Request{JobID: id})
	if resp.OK || resp.ErrorCode != "job_status_unavailable" {
		t.Fatalf("cleanup failure was not surfaced: %+v", resp)
	}
	if _, err := os.Stat(paths.command); err != nil {
		t.Fatalf("cleanup-failure fixture unexpectedly removed command: %v", err)
	}
}

func TestPersistentJobAdmissionUsesLifecycleLock(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "management.lock")
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)

	server := &Server{
		guard:             policy.New(nil),
		lifecycleLockPath: lockPath,
	}
	resp := server.startJobBounded(Request{Command: "printf blocked-by-lifecycle"})
	if resp.OK || resp.ErrorCode != "resource_limit" || resp.Status != "busy" || !resp.Retryable {
		t.Fatalf("lifecycle-lock admission response = %+v", resp)
	}
}

func TestPersistentJobHoldsLifecycleLockThroughLaunch(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "management.lock")
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	systemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\ncase \"$1\" in\n  list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 64 ;;\nesac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	exclusiveMarker := filepath.Join(t.TempDir(), "exclusive-acquired")
	systemdRun := filepath.Join(fakeBin, "systemd-run")
	probe := "#!/bin/sh\nif flock -n " + shellQuote(lockPath) + " -c true; then touch " + shellQuote(exclusiveMarker) + "; exit 55; fi\nexit 0\n"
	if err := os.WriteFile(systemdRun, []byte(probe), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:               config.Config{StateDir: state, WorkspaceDir: t.TempDir(), WorkerUser: current.Username},
		token:             "test-executor-token",
		guard:             policy.New(nil),
		audit:             audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID:         uint32(os.Geteuid()),
		workerGID:         uint32(os.Getegid()),
		lifecycleLockPath: lockPath,
	}
	resp := server.startJobBounded(Request{Command: "printf lifecycle-serialized"})
	if !resp.OK || resp.Status != "accepted" {
		t.Fatalf("start response = %+v", resp)
	}
	if _, err := os.Stat(exclusiveMarker); !os.IsNotExist(err) {
		t.Fatalf("exclusive lifecycle lock was acquired while start_job launch was in flight; err=%v", err)
	}
}
