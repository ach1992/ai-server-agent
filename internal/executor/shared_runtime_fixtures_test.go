package executor

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A first-time Agent install always provisions this worker-only directory.
// Test the real fail-closed check rather than falling back to workspace HOME.
func provisionSharedWorkerHome(t *testing.T, state string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(state, "worker-home"), 0700); err != nil {
		t.Fatalf("provision fresh Agent worker HOME: %v", err)
	}
}

// Active systemd fixtures require matching trusted job records. Unknown kinds
// must fail closed rather than counting as capacity available to another task.
func provisionActiveTaskClaims(t *testing.T, claimsDir, workspace string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		id := strconv.Itoa(i)
		claim := jobClaim{
			Version: 1, JobID: id, Fingerprint: "fixture-fingerprint",
			State: "started", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			PrincipalID: "fixture-owner", PrincipalClass: "direct",
			Workspace: workspace, Kind: "task",
		}
		if err := createJobClaim(filepath.Join(claimsDir, "job-"+id+".json"), claim); err != nil {
			t.Fatalf("create protected capacity fixture: %v", err)
		}
	}
}
