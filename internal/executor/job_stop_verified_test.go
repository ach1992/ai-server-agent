package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func TestVerifiedJobStopRetainsRecoverableStoppedStatus(t *testing.T) {
	state := t.TempDir()
	provisionSharedWorkerHome(t, state)
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// The fake systemctl exclusively manages this test's fixture: real
	// systemd units, services and Agent state must never be affected.
	systemctl := filepath.Join(bin, "systemctl")
	script := "#!/bin/sh\ncase \"$1\" in\n  stop|list-units) exit 0 ;;\n  show) printf 'not-found\\n'; exit 0 ;;\n  *) exit 88 ;;\nesac\n"
	if err := os.WriteFile(systemctl, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	for _, tc := range []struct {
		name     string
		id       string
		original string
		expected string
	}{
		{name: "fresh stop without exit code", id: "881", original: "", expected: "stopped"},
		{name: "previous unknown upgraded by verified stop", id: "882", original: jobStatusUnknown + "\n", expected: "stopped"},
		{name: "already completed retains numeric exit", id: "883", original: "7\n", expected: "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := jobPathsFor(jobs, tc.id)
			if err := os.WriteFile(paths.status, []byte(tc.original), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.started, []byte("started\n"), 0640); err != nil {
				t.Fatal(err)
			}
			fixtureRuntimeClaim(t, claims, tc.id, "owner", "service", "/srv/ai-workspace/test-owned-project", time.Now().UTC().Format(time.RFC3339Nano))
			s := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
			stop := s.jobStop(Request{JobID: tc.id})
			if !stop.OK || stop.Status != "stopped" {
				t.Fatalf("verified stop returned %+v", stop)
			}
			got, err := os.ReadFile(paths.status)
			if err != nil || strings.TrimSpace(string(got)) != tc.expected {
				t.Fatalf("durable terminal status: %q err=%v, want %s", got, err, tc.expected)
			}
			recovered := s.jobStatus(Request{JobID: tc.id})
			if !recovered.OK {
				t.Fatalf("verified stop cannot be recovered: %+v", recovered)
			}
			expectedStatus := "stopped"
			if tc.expected == "7" {
				expectedStatus = "completed"
			}
			if recovered.Status != expectedStatus {
				t.Fatalf("recovered status %+v, want %s", recovered, expectedStatus)
			}
			if tc.expected == "7" && recovered.ExitCode != 7 {
				t.Fatalf("original command exit code lost: %+v", recovered)
			}
			if tc.expected == jobStatusStopped && (recovered.ExitCode != -1 || !strings.Contains(recovered.Output, "unavailable")) {
				t.Fatalf("verified stop invented a successful command exit: %+v", recovered)
			}
			list := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct", Kind: "service"})
			if !list.OK || list.Truncated {
				t.Fatalf("passive inventory unusable after stop: %+v", list)
			}
			found := false
			for _, entry := range list.RuntimeEntries {
				if entry.ID == tc.id {
					found = true
					if entry.Status != expectedStatus {
						t.Fatalf("passive inventory lost stop evidence: %+v", entry)
					}
				}
			}
			if !found {
				t.Fatalf("verified stop missing from inventory: %+v", list.RuntimeEntries)
			}
			if err := s.cleanupCompletedJobArtifacts(jobs, claims); err != nil {
				t.Fatalf("retention path rejects verified stop marker: %v", err)
			}
		})
	}
}

// Failure of the systemd-active verification is not successful stop evidence.
// This fake unit remains active even after systemctl stop returned zero.
func TestJobStopDoesNotPersistStoppedIfUnitStillActive(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	id := "884"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.status, nil, 0640); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n stop) exit 0 ;;\n list-units) echo 'ai-job-884.service loaded active running'; exit 0 ;;\n *) exit 88 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	s := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	stopped := s.jobStop(Request{JobID: id})
	if stopped.OK || stopped.ErrorCode != "unknown_completion" || stopped.Status != "unknown" {
		t.Fatalf("active job was falsely reported stopped: %+v", stopped)
	}
	contents, err := os.ReadFile(paths.status)
	if err != nil || len(contents) != 0 {
		t.Fatalf("active job status was altered: %q %v", contents, err)
	}
}

// A verified systemd stop must never follow or modify an untrusted status
// symlink, even if the target is writeable by this test process.
func TestJobStopRejectsUnsafeStatusSymlink(t *testing.T) {
	state := t.TempDir()
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	id := "885"
	sentinel := filepath.Join(t.TempDir(), "unrelated-data")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := jobPathsFor(jobs, id)
	if err := os.Symlink(sentinel, paths.status); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n stop|list-units) exit 0 ;;\n *) exit 88 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	s := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	stopped := s.jobStop(Request{JobID: id})
	if stopped.OK || stopped.ErrorCode != "unknown_completion" {
		t.Fatalf("symlink status incorrectly accepted: %+v", stopped)
	}
	b, err := os.ReadFile(sentinel)
	if err != nil || string(b) != "preserve" {
		t.Fatalf("unrelated symlink target was modified: %q %v", b, err)
	}
}
