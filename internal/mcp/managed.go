package mcp

import (
	"context"
	"encoding/json"

	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/managedincus"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ManagedEnvironmentInput struct {
	Operation     string   `json:"operation" jsonschema:"status, pending_creates, list, inspect, create, start, stop, exec, snapshots, snapshot, restore or delete"`
	Project       string   `json:"project,omitempty" jsonschema:"Actual Incus project. Required for per-instance operations; list without project searches all projects"`
	Instance      string   `json:"instance,omitempty" jsonschema:"Exact native Incus instance name"`
	UUID          string   `json:"uuid,omitempty" jsonschema:"Native volatile.uuid obtained from inspect; required before mutating an existing guest"`
	Image         string   `json:"image,omitempty" jsonschema:"Native Incus image alias or fingerprint for create (e.g. images:debian/13)"`
	Type          string   `json:"type,omitempty" jsonschema:"container or virtual-machine for create"`
	WorkID        string   `json:"work_id,omitempty" jsonschema:"Stable Issue/project-owned work identifier for create, not a chat/session name"`
	OperationID   string   `json:"operation_id,omitempty" jsonschema:"Stable create idempotency key; reuse unchanged when the outcome is uncertain"`
	CPU           int      `json:"cpu,omitempty" jsonschema:"Explicit CPU count at create; operator capacity also applies"`
	MemoryMiB     int      `json:"memory_mib,omitempty" jsonschema:"Explicit guest memory size in MiB at create; checked against host capacity"`
	DiskGiB       int      `json:"disk_gib,omitempty" jsonschema:"Explicit guest root disk size in GiB at create; checked against host capacity"`
	StoragePool   string   `json:"storage_pool,omitempty" jsonschema:"Actual Incus storage pool for root disk; required on create to verify native free capacity"`
	Command       []string `json:"command,omitempty" jsonschema:"Native guest exec argv, no shell interpretation and never include secret values"`
	CredentialRef string   `json:"credential_ref,omitempty" jsonschema:"Optional service reference for confidential guest-exec stdin, bound to this exact host/project/guest/UUID; suppresses all guest stdout/stderr"`
	Snapshot      string   `json:"snapshot,omitempty" jsonschema:"Exact snapshot name for snapshot/restore"`
	Approval      bool     `json:"approval,omitempty" jsonschema:"Set only after explicit Owner approval for destructive delete/restore; no automatic privilege escalation"`
}

type ServiceAccessInput struct {
	Operation    string `json:"operation" jsonschema:"list, inspect, register, update, generate, renew, revoke, delete or cleanup"`
	Ref          string `json:"ref,omitempty" jsonschema:"Opaque service reference returned by register; never contains credentials"`
	Project      string `json:"project,omitempty" jsonschema:"Incus project for a managed guest; omit for an independently authorized external target"`
	Guest        string `json:"guest,omitempty" jsonschema:"Native instance name, requires verified UUID and project; omit for external service"`
	GuestUUID    string `json:"guest_uuid,omitempty" jsonschema:"Native volatile.uuid from current managed_environment inspect"`
	Service      string `json:"service,omitempty" jsonschema:"Descriptive service name, not a fixed allowlist; e.g. WordPress or aaPanel"`
	Purpose      string `json:"purpose,omitempty" jsonschema:"Non-secret work/purpose label"`
	Endpoint     string `json:"endpoint,omitempty" jsonschema:"Current URL including scheme/host/port/path; must not contain passwords, query tokens or fragments"`
	Username     string `json:"username,omitempty" jsonschema:"Optional non-secret username for registration or generated credentials"`
	Kind         string `json:"kind,omitempty" jsonschema:"Credential kind for generate: password or token; generated value never leaves root Executor"`
	TTLDays      int    `json:"ttl_days,omitempty" jsonschema:"Expiration from generation/renewal in days; 0 defaults to 30; set never_expires instead for explicit no expiry"`
	NeverExpires bool   `json:"never_expires,omitempty" jsonschema:"Explicit no-expiry option; false defaults to 30-day TTL"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Required only for irreversible metadata/credential reference deletion after explicit approval"`
}

func (s *Server) registerManagedTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "managed_environment",
		Description: "Optional host-local native Incus system containers and KVM VMs. status reports readiness without changing the host; list/inspect reconcile actual provider state and Agent instance identity before create or resume. Create uses stable operation_id and real capacity checks, refuses collisions, never installs/initializes Incus. Guest exec uses native Incus agent and argv (no shell unless explicitly requested). Snapshot/restore/delete are identity-bound; restore/delete require explicit approval. Missing Incus degrades only this tool; ordinary root/worker operations remain available.",
		Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ManagedEnvironmentInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		native := managedincus.Request{Operation: in.Operation, Project: in.Project, Instance: in.Instance, UUID: in.UUID, Image: in.Image, Type: in.Type, WorkID: in.WorkID, OperationID: in.OperationID, CPU: in.CPU, MemoryMiB: in.MemoryMiB, DiskGiB: in.DiskGiB, StoragePool: in.StoragePool, Command: in.Command, Snapshot: in.Snapshot}
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "managed_environment", ManagedEnv: native, CredentialRef: in.CredentialRef, Approval: in.Approval})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "service_access",
		Description: "Agent-local root-protected target-service records. Never returns secret values. Register verified guest-bound or independently authorized external endpoint; generate a strong local password/token without exposing it; list/inspect metadata; renew (default 30 days/no-expiry), revoke, delete or clean expired values. Third-party credentials can be enrolled/rotated with the operator-local no-echo CLI, never through MCP parameters, chat or logs. Credential refs are scoped to the authenticated Agent instance. No mandatory Vault/DB/new daemon.",
		Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ServiceAccessInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		reg := serviceaccess.Registration{Project: in.Project, Guest: in.Guest, GuestUUID: in.GuestUUID, Service: in.Service, Purpose: in.Purpose, Endpoint: in.Endpoint, Username: in.Username}
		op := serviceaccess.Input{Operation: in.Operation, Ref: in.Ref, Registration: reg, Kind: in.Kind, Username: in.Username, TTLDays: in.TTLDays, NeverExpires: in.NeverExpires}
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "service_access", ServiceAccess: op, Approval: in.Approval})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
}

// BrowserCredentialLoginInput contains only non-secret page locators and an
// opaque reference. The root Executor sends the credential directly to its
// isolated pinned Chromium process over a private stdin frame.
type BrowserCredentialLoginInput struct {
	Workspace          string `json:"workspace" jsonschema:"Exact existing Browser session workspace"`
	SessionID          string `json:"session_id" jsonschema:"Principal-bound isolated Browser session ID; never a durable credential"`
	CredentialRef      string `json:"credential_ref" jsonschema:"Opaque service_access registration reference; credential value is never returned"`
	UsernameSelector   string `json:"username_selector,omitempty" jsonschema:"CSS login user field locator; optional for token-only forms"`
	SecretSelector     string `json:"secret_selector" jsonschema:"CSS login password/token field locator"`
	SubmitSelector     string `json:"submit_selector" jsonschema:"CSS submit control locator"`
	SuccessSelector    string `json:"success_selector,omitempty" jsonschema:"CSS element that becomes visible only after successful authenticated login"`
	SuccessURL         string `json:"success_url,omitempty" jsonschema:"Alternative absolute same-origin URL prefix that is reached after a successful login"`
	ChallengeSelector  string `json:"challenge_selector,omitempty" jsonschema:"Optional CSS MFA/CAPTCHA/manual-intervention element locator to return an actionable challenge outcome"`
	LoginErrorSelector string `json:"login_error_selector,omitempty" jsonschema:"Optional CSS element visible only on failed sign-in; report invalid credentials only when this evidence is observed"`
}

func (s *Server) registerBrowserCredentialTool() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_credential_login",
		Description: "Use a protected target-service credential reference inside an existing isolated managed Playwright session; first navigate via browser_session_flow to the actual registered origin and inspect page locators. No secret argument, response or trace. Checks native guest UUID, exact page/form origin, credential expiry/revocation and a concrete success selector or URL; reports MFA/manual intervention without bypass. Do not assume preexisting Browser sessions survive chat changes. Credential-derived sessions deny further actions after expiry/revocation.",
		Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserCredentialLoginInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		opts := map[string]string{"username_selector": in.UsernameSelector, "secret_selector": in.SecretSelector, "submit_selector": in.SubmitSelector, "success_selector": in.SuccessSelector, "success_url": in.SuccessURL, "challenge_selector": in.ChallengeSelector, "login_error_selector": in.LoginErrorSelector}
		data, err := json.Marshal(opts)
		if err != nil {
			return executorTransportErrorResult(err)
		}
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "browser_session_credential_login", Workspace: in.Workspace, SessionID: in.SessionID, CredentialRef: in.CredentialRef, Content: string(data)})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
}
