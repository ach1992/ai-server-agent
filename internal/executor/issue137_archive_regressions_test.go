package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The historical Issue #137 archive had a direct-client cancellation case
// missing from the current aggregate-deadline test. Exercise the actual
// subprocess inventory path, not an injected terminal verification hook.
func TestRuntimeListCanceledSystemdInventoryFailsClosed(t *testing.T) {
	s, state, _ := newSharedRuntimeTestServer(t)
	if err := os.Mkdir(filepath.Join(state, "jobs"), 0711); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	entered := filepath.Join(t.TempDir(), "inventory-entered")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = list-units ]; then\n" +
		"  : > " + shellQuote(entered) + "\n" +
		"  exec sleep 60\n" +
		"fi\nexit 67\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	resp := s.dispatchContext(ctx, Request{
		Token: s.token, Action: "runtime_list",
		PrincipalID: "owner", PrincipalClass: "direct",
	})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled inventory blocked the connected request: %s", elapsed)
	}
	if _, err := os.Stat(entered); err != nil {
		t.Fatalf("stalled systemctl inventory fixture was not invoked: %v", err)
	}
	if !resp.OK || resp.Status != "observed" || !resp.Truncated ||
		resp.RuntimeCapacity == nil || resp.RuntimeCapacity.StateReliable ||
		resp.RuntimeCapacity.TasksActive != 0 || resp.RuntimeCapacity.ServicesActive != 0 {
		t.Fatalf("canceled PID1 inventory fabricated complete/available capacity: %+v", resp)
	}
}

// Stale/malformed protected service classification must not be repaired by
// guessing a kind or launching the command under another quota.
func TestServiceReplayRejectsUntrustedStoredKindWithoutDispatch(t *testing.T) {
	for _, storedKind := range []string{"", "task", "unexpected"} {
		t.Run("stored_"+storedKind, func(t *testing.T) {
			s, state, workspace := newSharedRuntimeTestServer(t)
			jobs := filepath.Join(state, "jobs")
			claims := filepath.Join(jobs, "claims")
			if err := os.Mkdir(jobs, 0711); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(claims, 0700); err != nil {
				t.Fatal(err)
			}
			req := Request{
				Command: "printf not-executed", OperationID: "stored-kind-negative",
				Kind: "service", Label: "api", Workspace: workspace,
				PrincipalID: "owner", PrincipalClass: "direct",
			}
			claim := jobClaim{
				Version: 1, JobID: "99561234", Fingerprint: s.jobFingerprint(req),
				State: "claimed", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
				PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass,
				Workspace: workspace, Kind: storedKind, Label: req.Label,
			}
			claimPath := filepath.Join(claims, operationClaimName(req.OperationID, req.PrincipalID))
			if err := createJobClaim(claimPath, claim); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			marker := filepath.Join(t.TempDir(), "dispatched")
			ctl := "#!/bin/sh\ncase \"$1\" in\n list-units) exit 0 ;;\n show) printf 'not-found\\n' ;;\n *) exit 67 ;;\nesac\n"
			run := "#!/bin/sh\n: > " + shellQuote(marker) + "\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(ctl), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "systemd-run"), []byte(run), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

			resp := s.startJobBounded(req)
			if resp.OK || resp.ErrorCode != "job_state_unavailable" {
				t.Fatalf("untrusted kind %q was allowed to launch: %+v", storedKind, resp)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("untrusted kind executed systemd-run: %v", err)
			}
			if _, err := os.Stat(jobPathsFor(jobs, claim.JobID).command); !os.IsNotExist(err) {
				t.Fatalf("untrusted kind prepared protected handoff: %v", err)
			}
			saved, found, err := readJobClaim(claimPath)
			if err != nil || !found || saved.JobID != claim.JobID ||
				saved.State != "claimed" || saved.Kind != storedKind ||
				saved.PrincipalID != req.PrincipalID {
				t.Fatalf("untrusted replay changed claim: %+v found=%v err=%v", saved, found, err)
			}
		})
	}
}
