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
	Operation     string   `json:"operation" jsonschema:"status, pending_creates, list, inspect, create, abandon_create, start, stop, exec, snapshots, snapshot, restore, operation_list, operation_status, resolve_uncertain or delete"`
	Project       string   `json:"project,omitempty" jsonschema:"Actual Incus project. Required for per-instance operations; list without project searches all projects"`
	Instance      string   `json:"instance,omitempty" jsonschema:"Exact native Incus instance name"`
	UUID          string   `json:"uuid,omitempty" jsonschema:"Currently observed Incus volatile.uuid from inspect, required for operations on both Agent-created and pre-existing native guests"`
	Image         string   `json:"image,omitempty" jsonschema:"Native Incus image alias or fingerprint for create (e.g. images:debian/13)"`
	Type          string   `json:"type,omitempty" jsonschema:"container or virtual-machine for create"`
	WorkID        string   `json:"work_id,omitempty" jsonschema:"Stable Issue/project-owned work identifier for create, not a chat/session name"`
	OperationID   string   `json:"operation_id,omitempty" jsonschema:"Stable idempotency key for create and nonblocking snapshot/restore. operation_status reads this same ID; never resubmit uncertain work"`
	CPU           int      `json:"cpu,omitempty" jsonschema:"Explicit CPU count at create; operator capacity also applies"`
	MemoryMiB     int      `json:"memory_mib,omitempty" jsonschema:"Explicit guest memory size in MiB at create; checked against host capacity"`
	DiskGiB       int      `json:"disk_gib,omitempty" jsonschema:"Explicit guest root disk size in GiB at create; checked against host capacity"`
	StoragePool   string   `json:"storage_pool,omitempty" jsonschema:"Actual Incus storage pool for root disk; required on create to verify native free capacity"`
	Command       []string `json:"command,omitempty" jsonschema:"Native guest exec argv, no shell interpretation and never include secret values"`
	CredentialRef string   `json:"credential_ref,omitempty" jsonschema:"Guest-bound service_access ref; host/project/guest/UUID must match. Secret reaches guest exec stdin (not argv); command must read stdin. Redacted diagnostics remain available"`
	Snapshot      string   `json:"snapshot,omitempty" jsonschema:"Exact snapshot name for snapshot/restore"`
	Approval      bool     `json:"approval,omitempty" jsonschema:"Set only after explicit Owner approval for delete/restore, uncertain create abandonment or uncertain native operation retirement; no automatic privilege escalation"`
}

type ServiceAccessInput struct {
	Operation    string `json:"operation" jsonschema:"list, inspect, register, update, generate, renew, revoke, delete or cleanup"`
	Ref          string `json:"ref,omitempty" jsonschema:"Opaque service reference returned by register; never contains credentials"`
	Project      string `json:"project,omitempty" jsonschema:"Incus project for a managed guest; omit for an independently authorized external target"`
	Guest        string `json:"guest,omitempty" jsonschema:"Native instance name, requires verified UUID and project; omit for external service"`
	GuestUUID    string `json:"guest_uuid,omitempty" jsonschema:"Native volatile.uuid from current managed_environment inspect"`
	Service      string `json:"service,omitempty" jsonschema:"Descriptive service name, not a fixed allowlist; e.g. WordPress or aaPanel"`
	Purpose      string `json:"purpose,omitempty" jsonschema:"Non-secret work/purpose label"`
	Endpoint     string `json:"endpoint,omitempty" jsonschema:"Optional non-secret absolute network-service URI (https, ssh, postgres, etc.). Omit for host-local app keys/codes without any network endpoint; no userinfo/query tokens/fragments. Storage alone does not grant an authentication adapter"`
	Username     string `json:"username,omitempty" jsonschema:"Optional non-secret username for registration or generated credentials"`
	Kind         string `json:"kind,omitempty" jsonschema:"Random generation supports only password or token; other kinds (api_key, private_key, access_code) are imported with operator-local service-access add/enroll; values never leave protected Executor"`
	TTLDays      int    `json:"ttl_days,omitempty" jsonschema:"Expiration from generation/renewal in days; 0 defaults to 30; set never_expires instead for explicit no expiry"`
	NeverExpires bool   `json:"never_expires,omitempty" jsonschema:"Explicit no-expiry option; false defaults to 30-day TTL"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Required only for irreversible metadata/credential reference deletion after explicit approval"`
}

func (s *Server) registerManagedTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "managed_environment",
		Description: "Manage optional native Incus guests via root Executor. status reports daemon readiness separately from VM-only KVM/UEFI observations; containers may still work without VM firmware. list/inspect are passive. Create uses stable operation_id; inspect operation_status before resubmitting uncertain operations. Existing guests require exact project/name/UUID. Restore/delete require approval; abandon_create also requires approval and native absence proof. Recover/inspect before repeating uncertain operations.",
		Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ManagedEnvironmentInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		native := managedincus.Request{Operation: in.Operation, Project: in.Project, Instance: in.Instance, UUID: in.UUID, Image: in.Image, Type: in.Type, WorkID: in.WorkID, OperationID: in.OperationID, CPU: in.CPU, MemoryMiB: in.MemoryMiB, DiskGiB: in.DiskGiB, StoragePool: in.StoragePool, Command: in.Command, Snapshot: in.Snapshot}
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "managed_environment", ManagedEnv: native, CredentialRef: in.CredentialRef, Approval: in.Approval})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "service_access",
		Description: "Manage protected service credentials for host/Incus/HTTP/SSH/SQL/Browser. register then generate stores a private password/token; local add/enroll imports existing secrets. list/inspect expose metadata only; update/renew/revoke/delete/cleanup manage lifecycle. Use an authorized target-specific consumer and verify real login/role. Manual secret retrieval requires an authorized root-capable local terminal with service-access reveal; never expose values via MCP or run_root_command.",
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

type ServiceUseInput struct {
	CredentialRef         string            `json:"credential_ref" jsonschema:"Opaque protected service_access reference; never paste the secret"`
	Method                string            `json:"method,omitempty" jsonschema:"HTTP method; defaults to GET"`
	Path                  string            `json:"path,omitempty" jsonschema:"Optional absolute HTTP path on the registered origin, with query; cannot change host"`
	Body                  string            `json:"body,omitempty" jsonschema:"Optional HTTP request body; bounded by existing Executor 8 MiB serialized request frame"`
	BodySecretRef         string            `json:"body_secret_ref,omitempty" jsonschema:"Optional second service_access reference for confidential JSON request-body field injection; must match authenticated service, origin, host and guest"`
	BodySecretJSONPointer string            `json:"body_secret_json_pointer,omitempty" jsonschema:"Existing JSON string field to fill from body_secret_ref, e.g. /password or /user/password; never include the secret in body"`
	ContentType           string            `json:"content_type,omitempty" jsonschema:"Optional HTTP request content type; defaults to application/json when body is given"`
	AuthHeader            string            `json:"auth_header,omitempty" jsonschema:"Optional HTTP header name for auth_mode=header, default X-API-Key"`
	AuthMode              string            `json:"auth_mode,omitempty" jsonschema:"HTTP auth mode: bearer, basic, or header; defaults from credential kind but does not constrain it"`
	Headers               map[string]string `json:"headers,omitempty" jsonschema:"Optional HTTP request headers on registered origin (except reserved Host/Authorization and transport fields); never include enrolled secret"`
	Command               string            `json:"command,omitempty" jsonschema:"SSH remote command: native enrolled private key, or optional sshpass for password/token; strict known-host verification"`
	Query                 string            `json:"query,omitempty" jsonschema:"SQL via native PostgreSQL SQL-only --command or MySQL/MariaDB --binary-mode stdin; registered credential used privately independent of kind. PostgreSQL SQL text is exposed in native process argv; avoid unrelated secrets in SQL literals"`
}

func (s *Server) registerServiceUseTool() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "service_use",
		Description: "Use protected service_access credentials for HTTP(S), SSH, PostgreSQL or MySQL without revealing values. Enforces registered endpoint, guest identity and no HTTP redirects. For authorized HTTP APIs, body_secret_ref plus body_secret_json_pointer privately fills an existing JSON string field from a second same-service, same-origin and same-guest credential. Never put secrets in arguments. Keep useful diagnostics with targeted secret redaction; recover large output through job_output. Optional native clients may be required. Production changes need fresh Owner consent.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ServiceUseInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action: "service_use", CredentialRef: in.CredentialRef, TimeoutMS: s.commandTimeoutMS(),
			ServiceUse: executor.ServiceUseSpec{Method: in.Method, Path: in.Path, Body: in.Body, BodySecretRef: in.BodySecretRef, BodySecretJSONPointer: in.BodySecretJSONPointer, ContentType: in.ContentType, AuthHeader: in.AuthHeader, AuthMode: in.AuthMode, Headers: in.Headers, Command: in.Command, Query: in.Query},
		})
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
	SuccessURL         string `json:"success_url,omitempty" jsonschema:"Alternative HTTP(S) URL prefix reached after successful login; may be a different post-auth SSO origin but credential submission stays bound to registered form origin"`
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
