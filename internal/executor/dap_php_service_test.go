package executor

import (
	"strings"
	"testing"
)

func TestPHPHTTPServiceClaimRequiresExactIndependentOwner(t *testing.T) {
	const workspace = "/srv/ai-workspace/verified-project"
	owner := Request{
		Action: "debug_launch", Language: "php", DebugMode: "http",
		PrincipalID: "authenticated-client-1", PrincipalClass: "direct",
		Workspace: workspace, JobID: "1234567890123456789",
	}
	claim := jobClaim{
		Version: 1, State: "started", Kind: "service", JobID: owner.JobID,
		PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass,
		Workspace: workspace, Fingerprint: strings.Repeat("a", 64),
	}
	if err := authorizePHPHTTPServiceClaim(owner, workspace, claim); err != nil {
		t.Fatalf("matching protected worker Service Job rejected: %v", err)
	}

	cases := []struct {
		name string
		edit func(*Request, *jobClaim)
	}{
		{"missing_principal", func(r *Request, _ *jobClaim) { r.PrincipalID = "" }},
		{"different_principal", func(r *Request, _ *jobClaim) { r.PrincipalID = "other" }},
		{"different_principal_class", func(r *Request, _ *jobClaim) { r.PrincipalClass = "other" }},
		{"missing_principal_class", func(r *Request, _ *jobClaim) { r.PrincipalClass = "" }},
		{"root_debug_request", func(r *Request, _ *jobClaim) { r.Root = true }},
		{"approval_debug_request", func(r *Request, _ *jobClaim) { r.Approval = true }},
		{"missing_job_id", func(r *Request, _ *jobClaim) { r.JobID = "" }},
		{"bad_job_id", func(r *Request, _ *jobClaim) { r.JobID = "../other" }},
		{"different_job_id", func(r *Request, _ *jobClaim) { r.JobID = "9876543210123456789" }},
		{"claim_root_job", func(_ *Request, c *jobClaim) { c.Root = true }},
		{"claim_task_not_service", func(_ *Request, c *jobClaim) { c.Kind = "task" }},
		{"claim_stopped", func(_ *Request, c *jobClaim) { c.VerifiedStopped = true }},
		{"claim_not_started", func(_ *Request, c *jobClaim) { c.State = "launching" }},
		{"claim_completed", func(_ *Request, c *jobClaim) { c.State = "failed" }},
		{"claim_wrong_version", func(_ *Request, c *jobClaim) { c.Version = 2 }},
		{"claim_wrong_owner", func(_ *Request, c *jobClaim) { c.PrincipalID = "other" }},
		{"claim_wrong_owner_class", func(_ *Request, c *jobClaim) { c.PrincipalClass = "other" }},
		{"claim_workspace_drift", func(_ *Request, c *jobClaim) { c.Workspace = "/srv/ai-workspace/another" }},
		{"request_workspace_drift", func(r *Request, _ *jobClaim) { r.Workspace = "/srv/ai-workspace/another" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := owner, claim
			tc.edit(&r, &c)
			if err := authorizePHPHTTPServiceClaim(r, workspace, c); err == nil {
				t.Fatalf("unsafe PHP HTTP service claim accepted: request=%+v claim=%+v", r, c)
			}
		})
	}
}
