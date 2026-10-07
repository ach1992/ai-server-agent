package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cleanupStalePrelaunchClaims retires crash-window launch state after the
// executor's broad connection deadline has elapsed. Active/completed execution
// evidence always wins; only claims for which execution cannot have progressed
// are failed and their protected handoff files removed.
func (s *Server) cleanupStalePrelaunchClaims(jobsDir, claimsDir string, now time.Time) error {
	entries, err := os.ReadDir(claimsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(claimsDir, entry.Name())
		claim, found, err := readJobClaim(path)
		if err != nil {
			return err
		}
		if !found || (claim.State != "claimed" && claim.State != "launching") {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, claim.CreatedAt)
		if err != nil {
			return fmt.Errorf("invalid job claim creation time: %w", err)
		}
		if now.Before(createdAt) || now.Sub(createdAt) < executorConnectionTimeout {
			continue
		}
		id, err := safeID(claim.JobID)
		if err != nil {
			return fmt.Errorf("invalid stale claim job id: %w", err)
		}
		paths := jobPathsFor(jobsDir, id)
		evidence, active, err := s.jobExecutionEvidenceState(paths, id)
		if err != nil {
			return err
		}
		if evidence {
			claim.State = "started"
			if err := updateJobClaim(path, claim); err != nil {
				return err
			}
			if !active {
				if err := s.markJobStatusUnknownIfEmpty(paths.status); err != nil {
					return err
				}
				if err := os.Remove(paths.command); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			continue
		}
		if err := removeJobPaths(paths); err != nil {
			return err
		}
		claim.State = "failed"
		if err := updateJobClaim(path, claim); err != nil {
			return err
		}
	}
	return nil
}
