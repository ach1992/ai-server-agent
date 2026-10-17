package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Simulate a unit-name replacement specifically AFTER the second live-unit
// check, not before it. Real kernel pidfds pin the original test-owned child.
// No real systemd unit is created or stopped by this non-root regression.
func TestPHPHTTPFixtureFallbackTargetsOriginalAfterNameReplacement(t *testing.T) {
	oldRunner := exec.Command("/bin/sleep", "30")
	replacement := exec.Command("/bin/sleep", "30")
	for _, cmd := range []*exec.Cmd{oldRunner, replacement} {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	original := phpHTTPServiceUnit{
		id: "12345", invocation: strings.Repeat("a", 32),
		cgroup: "/system.slice/ai-job-12345.service", mainPID: oldRunner.Process.Pid,
	}
	replaced := original
	replaced.invocation = strings.Repeat("b", 32)
	replaced.mainPID = replacement.Process.Pid
	mappedUnit := original
	observations := 0
	pinOriginal := func(actual phpHTTPServiceUnit, _ uint32) (*os.File, error) {
		if actual != original {
			return nil, errors.New("attempted to pin replacement")
		}
		fd, err := unix.PidfdOpen(oldRunner.Process.Pid, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), "test-original-job-runner"), nil
	}
	err := stopOriginalPHPHTTPFixtureUnitWith(
		context.Background(), original, "test-worker", 1234,
		phpHTTPFixtureStopOps{
			read: func(_ context.Context, id, worker string) (phpHTTPServiceUnit, error) {
				if id != original.id || worker != "test-worker" {
					return phpHTTPServiceUnit{}, errors.New("wrong unit")
				}
				observations++
				return mappedUnit, nil
			},
			pin: pinOriginal,
			active: func(context.Context, string) (bool, error) {
				return true, nil // the replacement now owns this name
			},
			afterFinalCheck: func() {
				if observations != 2 {
					t.Errorf("expected two original identity checks before substitution, observed %d", observations)
				}
				mappedUnit = replaced
			},
		})
	if err == nil || !strings.Contains(err.Error(), "replacement_unit_active_after_original_runner_stop") {
		t.Fatalf("replacement did not produce safe post-signal reconciliation: %v", err)
	}
	// Only the pinned original process was signaled. The now-named replacement
	// is still live, even though the second identity check occurred before
	// the replacement mapping changed.
	if err := oldRunner.Wait(); err == nil {
		t.Fatal("original runner did not exit from targeted SIGTERM")
	}
	if err := syscall.Kill(replacement.Process.Pid, 0); err != nil {
		t.Fatalf("new owner of reusable unit name was signaled: %v", err)
	}
}

func TestPHPHTTPFixtureFallbackVerifiedOriginalRunnerExit(t *testing.T) {
	oldRunner := exec.Command("/bin/sleep", "30")
	if err := oldRunner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = oldRunner.Process.Kill()
		_ = oldRunner.Wait()
	})
	original := phpHTTPServiceUnit{
		id: "54321", invocation: strings.Repeat("a", 32),
		cgroup: "/system.slice/ai-job-54321.service", mainPID: oldRunner.Process.Pid,
	}
	checked := 0
	err := stopOriginalPHPHTTPFixtureUnitWith(
		context.Background(), original, "test-worker", 1234,
		phpHTTPFixtureStopOps{
			read: func(_ context.Context, _, _ string) (phpHTTPServiceUnit, error) {
				checked++
				return original, nil
			},
			pin: func(_ phpHTTPServiceUnit, _ uint32) (*os.File, error) {
				fd, err := unix.PidfdOpen(oldRunner.Process.Pid, 0)
				if err != nil {
					return nil, err
				}
				return os.NewFile(uintptr(fd), "test-original-job-runner"), nil
			},
			active: func(context.Context, string) (bool, error) { return false, nil },
		})
	if err != nil || checked != 2 {
		t.Fatalf("original exact-pidfd stop unverified, checks=%d err=%v", checked, err)
	}
	if err := oldRunner.Wait(); err == nil {
		t.Fatal("original runner was not terminated by a targeted signal")
	}
}
