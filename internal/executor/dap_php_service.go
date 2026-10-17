package executor

import (
	"context"
	"errors"
	"path/filepath"
)

// authorizePHPHTTPServiceClaim binds the debugger's authenticated principal
// and exact workspace to the separately owned persistent worker Service Job.
// Sharing a Unix UID or knowing a job_id is never authorization to debug it.
//
// This is only the protected-claim gate. A caller must ALSO prove a live,
// unchanged systemd service unit/cgroup and exact PHP/DBGp request provenance
// before accepting an HTTP debug connection. In particular it must never
// transfer job_stop authority or service lifetime ownership to DAP.
func authorizePHPHTTPServiceClaim(req Request, workspace string, claim jobClaim) error {
	if req.Root || req.Approval || req.PrincipalID == "" || req.PrincipalClass == "" {
		return errors.New("PHP HTTP debugging requires an authenticated worker principal")
	}
	id, err := safeID(req.JobID)
	if err != nil {
		return errors.New("PHP HTTP debugging requires an exact service Job ID")
	}
	if workspace == "" || req.Workspace != workspace {
		return errors.New("PHP HTTP debugging requires an exact authorized workspace")
	}
	if claim.JobID != id || claim.Version != 1 || claim.Kind != "service" ||
		claim.Root || claim.VerifiedStopped || claim.State != "started" {
		return errors.New("PHP HTTP target is not a verified separately owned active worker Service Job claim")
	}
	if claim.PrincipalID != req.PrincipalID || claim.PrincipalClass != req.PrincipalClass ||
		claim.Workspace != workspace {
		return errors.New("PHP HTTP Service Job ownership does not match the debugger principal and workspace")
	}
	return nil
}

// loadAuthorizedPHPHTTPServiceClaim reads only Executor-protected Job claims
// under the existing Job-state synchronization. Claim validation is necessary
// but insufficient; never treat this routine as live service or process proof.
func (s *Server) loadAuthorizedPHPHTTPServiceClaim(
	ctx context.Context, req Request, workspace string,
) (jobClaim, error) {
	if _, err := safeID(req.JobID); err != nil {
		return jobClaim{}, errors.New("invalid PHP HTTP Service Job identity")
	}
	if err := s.lockJobState(ctx); err != nil {
		return jobClaim{}, err
	}
	defer s.jobsMu.Unlock()

	claim, _, found, err := findJobClaimForID(filepath.Join(s.cfg.StateDir, "jobs", "claims"), req.JobID)
	if err != nil {
		return jobClaim{}, err
	}
	if !found {
		return jobClaim{}, errors.New("PHP HTTP Service Job protected claim not found")
	}
	if err := authorizePHPHTTPServiceClaim(req, workspace, claim); err != nil {
		return jobClaim{}, err
	}
	return claim, nil
}
