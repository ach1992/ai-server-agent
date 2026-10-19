package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

const (
	maxActivePersistentJobs           = 64 // hard bound; configured default is four
	maxActivePersistentServices       = 64 // hard bound; configured default is two
	maxCompletedJobArtifacts          = 32
	maxFailedJobClaims                = maxCompletedJobArtifacts
	maxPersistentJobClaims            = 2*maxCompletedJobArtifacts + maxActivePersistentJobs + maxActivePersistentServices
	maxJobLogBytes              int64 = 8 << 20
	jobLogHeaderSize                  = 32
	maxOperationIDBytes               = 128
	systemdOutputLimit                = 64 << 10
	jobSystemdProbeTimeout            = 4 * time.Second
	jobSystemdLaunchTimeout           = 25 * time.Second
	jobSystemdStopTimeout             = 25 * time.Second
	jobStateLockTimeout               = 5 * time.Second
	jobSystemdWaitDelay               = 750 * time.Millisecond
	jobRunnerFailureExit              = 125
	jobStateOverheadBytes       int64 = 64 << 20
	lifecycleManagementLockPath       = "/run/lock/ai-server-agent/management.lock"
)

var jobLogMagic = [8]byte{'A', 'I', 'S', 'A', 'J', 'L', '0', '1'}

const jobStatusUnknown = "unknown"
const jobStatusStopped = "stopped"

// A stalled systemd operation must not hold jobsMu indefinitely. Keep
// the synchronization needed for protected claims and runner status, but
// give every caller a bounded, retryable path when another Job is busy.
func (s *Server) lockJobState(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, jobStateLockTimeout)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.jobsMu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func jobStateBusyResponse(operation, jobID string, err error) Response {
	return Response{
		Error:      fmt.Sprintf("%s could not acquire Job state lock within %s: %v; retry after the other operation finishes", operation, jobStateLockTimeout, err),
		ReasonCode: "job_state_unavailable", ErrorCode: "job_state_unavailable",
		ErrorClass: "state", Status: "busy", Retryable: true, JobID: jobID,
	}
}

// Systemd calls share a bounded collector and independent deadlines. A
// disconnected client does not cancel durable launch/stop reconciliation.
func runBoundedJobSystemd(parent context.Context, timeout time.Duration, program string, args ...string) (boundedOutputResult, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	out := newBoundedOutputCollector(systemdOutputLimit)
	cmd := exec.CommandContext(ctx, program, args...)
	// A child inheriting stdout must not keep Cmd.Wait alive after the
	// systemd process exits or is killed by the context deadline.
	cmd.WaitDelay = jobSystemdWaitDelay
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	result := out.Result()
	if ctx.Err() != nil {
		return result, fmt.Errorf("%s operation exceeded its deadline: %w", program, ctx.Err())
	}
	return result, err
}

type jobPaths struct {
	command string
	log     string
	status  string
	started string
}

type jobClaim struct {
	Version        int    `json:"version"`
	JobID          string `json:"job_id"`
	Fingerprint    string `json:"fingerprint"`
	State          string `json:"state"`
	CreatedAt      string `json:"created_at"`
	PrincipalID    string `json:"principal_id,omitempty"`
	PrincipalClass string `json:"principal_class,omitempty"`
	Root           bool   `json:"root,omitempty"`
	Workspace      string `json:"workspace,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Label          string `json:"label,omitempty"`
	// Root Executor proof; never sourced from a worker-writable .status file.
	VerifiedStopped bool `json:"verified_stopped,omitempty"`
}

type jobLogHeader struct {
	Capacity      int64
	AvailableFrom int64
	CurrentEnd    int64
}

type jobLogWriter struct {
	mu sync.Mutex
	f  *os.File
	h  jobLogHeader
}

type completedJobArtifact struct {
	id      string
	modTime time.Time
}

func (s *Server) startJobBounded(req Request) (resp Response) {
	if bad := commandInputError(req.Command); bad != nil {
		return *bad
	}
	if req.Root && req.Workspace != "" {
		return fileError("invalid_workspace", "validation", errors.New("worker workspace cannot be supplied for a root job"))
	}
	dec := s.guard.Evaluate(req.Command, req.Root)
	if !dec.Allowed {
		return Response{Error: dec.Reason, ReasonCode: "policy_denied", ErrorCode: "policy_denied", ErrorClass: "policy"}
	}
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", ReasonCode: "approval_required", ErrorCode: "approval_required", ErrorClass: "approval", Approval: dec}
	}
	if req.Kind == "" {
		req.Kind = "task"
	}
	if req.Kind != "task" && req.Kind != "service" {
		return fileError("invalid_job_kind", "validation", errors.New("kind must be task or service"))
	}
	if err := validateJobLabel(req.Kind, req.Label); err != nil {
		return fileError("invalid_job_label", "validation", err)
	}
	if req.Root {
		if req.Workspace != "" {
			return fileError("invalid_workspace", "validation", errors.New("worker workspace cannot be supplied for a root job"))
		}
		req.Workspace = "/root"
	} else {
		cwd, err := s.workerCommandWorkspace(req.Workspace)
		if err != nil {
			return fileError("invalid_workspace", "validation", err)
		}
		if _, err := s.workerSessionHome(); err != nil {
			return fileError("worker_home_unavailable", "state", err)
		}
		req.Workspace = cwd
	}
	if err := validateOperationID(req.OperationID); err != nil {
		return Response{Error: err.Error(), ReasonCode: "invalid_operation_id", ErrorCode: "invalid_operation_id", ErrorClass: "validation"}
	}

	lifecycleLock, blocked := s.acquirePersistentJobAdmissionLock()
	if blocked != nil {
		return *blocked
	}
	if lifecycleLock != nil {
		defer releasePersistentJobAdmissionLock(lifecycleLock)
	}

	if err := s.lockJobState(context.Background()); err != nil {
		return jobStateBusyResponse("start_job", "", err)
	}
	defer s.jobsMu.Unlock()

	if req.OperationID == "" {
		req.OperationID = "@internal-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	auditStarted := time.Now()
	if blocked := s.beginActionAudit(req, "start_job", auditMode(req.Root), req.Command, dec.Category); blocked != nil {
		return *blocked
	}
	defer func() {
		resp = s.finishActionAudit(req, "start_job", auditMode(req.Root), req.Command, dec.Category, auditStarted, resp)
	}()

	jobsDir, claimsDir, err := s.ensureJobState()
	if err != nil {
		return jobStateError("job_state_unavailable", err)
	}
	if err := s.cleanupCompletedJobArtifacts(jobsDir, claimsDir); err != nil {
		return jobStateError("job_state_unavailable", err)
	}
	if err := s.cleanupStalePrelaunchClaims(jobsDir, claimsDir, time.Now()); err != nil {
		return jobStateError("job_state_unavailable", err)
	}

	fingerprint := s.jobFingerprint(req)
	var claimPath string
	if req.OperationID != "" {
		claimPath = filepath.Join(claimsDir, operationClaimName(req.OperationID, req.PrincipalID))
		if claim, found, err := readJobClaim(claimPath); err != nil {
			return jobStateError("job_state_unavailable", err)
		} else if found {
			return s.resumeClaimedJob(req, jobsDir, claimPath, claim, fingerprint)
		}
		// Before named principals, direct/default idempotency claims were keyed
		// only by operation_id. Preserve those claims for the migrated direct
		// principal while keeping every newly created claim principal-scoped.
		if req.PrincipalID == "direct-default" {
			legacyClaimPath := filepath.Join(claimsDir, legacyOperationClaimName(req.OperationID))
			if claim, found, err := readJobClaim(legacyClaimPath); err != nil {
				return jobStateError("job_state_unavailable", err)
			} else if found {
				return s.resumeClaimedJob(req, jobsDir, legacyClaimPath, claim, fingerprint)
			}
		}
		if err := cleanupFailedJobClaims(jobsDir, claimsDir); err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		claimCount, err := countJobClaims(claimsDir)
		if err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		if claimCount >= maxPersistentJobClaims {
			return Response{
				Error:      "persistent-job recovery state is at capacity; reconcile retained jobs/operation_ids before creating another",
				ReasonCode: "resource_limit",
				ErrorCode:  "resource_limit",
				ErrorClass: "resource",
				Retryable:  true,
				Status:     "busy",
			}
		}
	}

	if blocked := persistentJobAdmissionResponseWithLimits(jobsDir, s.persistentTaskCapacity(), s.persistentServiceCapacity(), req.Kind); blocked != nil {
		return *blocked
	}

	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	paths := jobPathsFor(jobsDir, id)
	claim := jobClaim{
		Version:        1,
		JobID:          id,
		Fingerprint:    fingerprint,
		State:          "claimed",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		PrincipalID:    req.PrincipalID,
		PrincipalClass: req.PrincipalClass,
		Root:           req.Root,
		Workspace:      req.Workspace,
		Kind:           req.Kind,
		Label:          req.Label,
	}
	if claimPath != "" {
		if err := createJobClaim(claimPath, claim); err != nil {
			if errors.Is(err, os.ErrExist) {
				existing, found, readErr := readJobClaim(claimPath)
				if readErr != nil {
					return jobStateError("job_state_unavailable", readErr)
				}
				if found {
					return s.resumeClaimedJob(req, jobsDir, claimPath, existing, fingerprint)
				}
			}
			return jobStateError("job_state_unavailable", err)
		}
	}
	if err := s.prepareJobFiles(paths, req); err != nil {
		s.failJobClaim(claimPath, &claim)
		cleanupJobPaths(paths)
		return jobStateError("job_prepare_failed", err)
	}

	// Persist before calling systemd-run: from this point an external
	// dispatch may have happened, even if the unit has not appeared yet.
	// Only a still-claimed record proves that no launch was attempted.
	claim.State = "launching"
	if claimPath != "" {
		if err := updateJobClaim(claimPath, claim); err != nil {
			s.failJobClaim(claimPath, &claim)
			cleanupJobPaths(paths)
			return jobStateError("job_state_unavailable", err)
		}
	}

	resp = s.launchPreparedJob(req, paths, id)
	if resp.OK {
		claim.State = "started"
		if claimPath != "" {
			if err := updateJobClaim(claimPath, claim); err != nil {
				return Response{
					Error:      "job started but idempotency state could not be finalized: " + err.Error(),
					ReasonCode: "unknown_completion",
					ErrorCode:  "unknown_completion",
					ErrorClass: "state",
					JobID:      id,
				}
			}
		}
	} else if resp.ErrorCode != "unknown_completion" {
		cleanupJobPaths(paths)
		if claimPath != "" {
			s.failJobClaim(claimPath, &claim)
		}
	}

	return resp
}

func (s *Server) resumeClaimedJob(req Request, jobsDir, claimPath string, claim jobClaim, fingerprint string) Response {
	if claim.Fingerprint != fingerprint {
		return Response{
			Error:      "operation_id is already bound to a different persistent-job request",
			ReasonCode: "idempotency_conflict",
			ErrorCode:  "idempotency_conflict",
			ErrorClass: "conflict",
			JobID:      claim.JobID,
		}
	}
	kind := claim.Kind
	if kind == "" {
		// Claims created before task/service classification were tasks only.
		kind = "task"
	}
	reqKind := req.Kind
	if reqKind == "" {
		reqKind = "task"
	}
	if (kind != "task" && kind != "service") || kind != reqKind {
		return jobStateError("job_state_unavailable", errors.New("persistent job claim kind is invalid or does not match replay request"))
	}
	if _, err := safeID(claim.JobID); err != nil {
		return jobStateError("job_state_unavailable", fmt.Errorf("invalid claimed job id: %w", err))
	}
	paths := jobPathsFor(jobsDir, claim.JobID)
	if terminal, found := s.jobTerminalReplayResponse(paths.status, claim.JobID); found {
		terminal.IdempotentReplay = true
		return terminal
	}

	switch claim.State {
	case "started":
		active, err := jobUnitActive(claim.JobID)
		if err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		if active {
			return Response{OK: true, JobID: claim.JobID, Status: "accepted", IdempotentReplay: true}
		}
		if err := s.markJobStatusUnknownIfEmpty(paths.status); err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		if err := removeTerminalCommandHandoff(paths.command); err != nil {
			return jobStateError("job_status_unavailable", err)
		}
		resp, _ := s.jobTerminalReplayResponse(paths.status, claim.JobID)
		resp.IdempotentReplay = true
		return resp
	case "failed":
		return Response{
			Error:            "the prior start attempt for this operation_id failed before a durable job start could be proven",
			ReasonCode:       "job_start_failed",
			ErrorCode:        "job_start_failed",
			ErrorClass:       "state",
			JobID:            claim.JobID,
			IdempotentReplay: true,
		}
	case "claimed", "launching":
		// Reconciled below.
	default:
		return jobStateError("job_state_unavailable", fmt.Errorf("unsupported job claim state %q", claim.State))
	}

	evidence, active, err := s.jobExecutionEvidenceState(paths, claim.JobID)
	if err != nil {
		return jobStateError("job_state_unavailable", err)
	}
	if evidence {
		claim.State = "started"
		if err := updateJobClaim(claimPath, claim); err != nil {
			resp := jobClaimFinalizeError(claim.JobID, err)
			resp.IdempotentReplay = true
			return resp
		}
		if active {
			return Response{OK: true, JobID: claim.JobID, Status: "accepted", IdempotentReplay: true}
		}
		if err := s.markJobStatusUnknownIfEmpty(paths.status); err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		if err := removeTerminalCommandHandoff(paths.command); err != nil {
			return jobStateError("job_status_unavailable", err)
		}
		resp, _ := s.jobTerminalReplayResponse(paths.status, claim.JobID)
		resp.IdempotentReplay = true
		return resp
	}

	// "launching" was durably committed before systemd-run was invoked.
	// A lost/timed-out reply or Executor restart cannot establish non-delivery
	// merely from a missing transient unit or started marker. Never submit a
	// second launch for the same operation_id, including legacy launching
	// claims created before this no-blind-replay correction.
	if claim.State == "launching" {
		resp := unknownJobLaunchResponse(claim.JobID,
			"persistent job launch may have reached systemd but execution is not proven; inspect the original job_id; the original operation_id cannot be automatically re-dispatched")
		resp.IdempotentReplay = true
		return resp
	}

	// Only "claimed" proves systemd-run was never invoked. Safely rebuild
	// any partial handoff before rechecking resource admission.
	cleanupJobPaths(paths)
	if blocked := persistentJobAdmissionResponseWithLimits(jobsDir, s.persistentTaskCapacity(), s.persistentServiceCapacity(), kind); blocked != nil {
		blocked.JobID = claim.JobID
		blocked.IdempotentReplay = true
		return *blocked
	}

	if err := s.prepareJobFiles(paths, req); err != nil {
		claim.State = "failed"
		_ = updateJobClaim(claimPath, claim)
		return jobStateError("job_prepare_failed", err)
	}
	// The durable transition is the one-way external-dispatch boundary.
	// A crash before this write remains safely retryable; after it,
	// uncertain delivery is never blindly retried.
	claim.State = "launching"
	if err := updateJobClaim(claimPath, claim); err != nil {
		cleanupJobPaths(paths)
		return jobStateError("job_state_unavailable", err)
	}

	resp := s.launchPreparedJob(req, paths, claim.JobID)
	resp.IdempotentReplay = true
	if resp.OK {
		claim.State = "started"
		if err := updateJobClaim(claimPath, claim); err != nil {
			finalize := jobClaimFinalizeError(claim.JobID, err)
			finalize.IdempotentReplay = true
			return finalize
		}
	} else if resp.ErrorCode != "unknown_completion" {
		cleanupJobPaths(paths)
		claim.State = "failed"
		_ = updateJobClaim(claimPath, claim)
	}
	return resp
}

func (s *Server) launchPreparedJob(req Request, paths jobPaths, id string) Response {
	return s.launchPreparedJobWithTimeout(req, paths, id, jobSystemdLaunchTimeout)
}

func (s *Server) launchPreparedJobWithTimeout(req Request, paths jobPaths, id string, timeout time.Duration) Response {
	home := filepath.Join(s.cfg.StateDir, "worker-home")
	workDir := req.Workspace
	if req.Root {
		home = "/root"
		workDir = "/root"
	}
	runnerPath, err := os.Executable()
	if err != nil {
		return jobStateError("job_start_failed", fmt.Errorf("resolve executor binary: %w", err))
	}

	args := []string{"--unit", "ai-job-" + id, "--collect", "--property=WorkingDirectory=" + workDir}
	if !req.Root {
		args = append(args, "--uid="+s.cfg.WorkerUser)
	}
	args = append(args,
		"/usr/bin/env", "-i",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"PATH="+safeCommandPath,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"AI_SERVER_AGENT=1",
		runnerPath, "job-runner",
		"--command-file", paths.command,
		"--log-file", paths.log,
		"--status-file", paths.status,
		"--started-file", paths.started,
	)

	result, err := runBoundedJobSystemd(context.Background(), timeout, "systemd-run", args...)
	if err != nil {
		active, activeErr := jobUnitActive(id)
		if activeErr != nil {
			resp := unknownJobLaunchResponse(id, "systemd-run failed and authoritative active state could not be determined: "+activeErr.Error())
			applyOutputResult(&resp, result)
			return resp
		}
		if active {
			resp := Response{OK: true, JobID: id, Status: "accepted"}
			applyOutputResult(&resp, result)
			return resp
		}

		status, _, statusErr := s.readJobStatusValue(paths.status)
		if statusErr != nil && !os.IsNotExist(statusErr) {
			resp := unknownJobLaunchResponse(id, "systemd-run failed and durable job status could not be read: "+statusErr.Error())
			applyOutputResult(&resp, result)
			return resp
		}
		if status != "" {
			if cleanupErr := removeTerminalCommandHandoff(paths.command); cleanupErr != nil {
				resp := unknownJobLaunchResponse(id, "job reached terminal state but command handoff cleanup failed: "+cleanupErr.Error())
				applyOutputResult(&resp, result)
				return resp
			}
			return terminalJobResponse(status, id)
		}

		started, startedErr := s.jobStarted(paths.started)
		if startedErr != nil {
			resp := unknownJobLaunchResponse(id, "systemd-run failed and durable start state could not be read: "+startedErr.Error())
			applyOutputResult(&resp, result)
			return resp
		}
		exists, existsErr := jobUnitExists(id)
		if existsErr != nil {
			resp := unknownJobLaunchResponse(id, "systemd-run failed and unit load state could not be reconciled: "+existsErr.Error())
			applyOutputResult(&resp, result)
			return resp
		}
		if started || exists {
			if markErr := s.markJobStatusUnknownIfEmpty(paths.status); markErr != nil {
				resp := unknownJobLaunchResponse(id, "systemd-run failed and unknown completion could not be persisted: "+markErr.Error())
				applyOutputResult(&resp, result)
				return resp
			}
			if cleanupErr := removeTerminalCommandHandoff(paths.command); cleanupErr != nil {
				resp := unknownJobLaunchResponse(id, "systemd-run failed and terminal command handoff cleanup failed: "+cleanupErr.Error())
				applyOutputResult(&resp, result)
				return resp
			}
			resp := unknownJobLaunchResponse(id, "systemd-run failed after launch became possible; completion cannot be proven")
			applyOutputResult(&resp, result)
			return resp
		}

		// A timed-out systemd-run may have reached PID1 before its reply was
		// lost. Preserve the protected launching claim and handoff rather
		// than treating the absence of immediate unit evidence as failure.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrWaitDelay) {
			resp := unknownJobLaunchResponse(id, "systemd-run exceeded its process or output deadline; launch outcome remains uncertain; reconcile the original operation_id before retrying")
			applyOutputResult(&resp, result)
			return resp
		}
		if cleanupErr := removeTerminalCommandHandoff(paths.command); cleanupErr != nil {
			resp := jobStateError("job_state_unavailable", cleanupErr)
			resp.JobID = id
			applyOutputResult(&resp, result)
			return resp
		}
		resp := Response{
			Error:      "persistent job could not be started: " + err.Error(),
			ReasonCode: "job_start_failed",
			ErrorCode:  "job_start_failed",
			ErrorClass: "process",
			JobID:      id,
		}
		applyOutputResult(&resp, result)
		return resp
	}

	resp := Response{OK: true, JobID: id, Status: "accepted"}
	applyOutputResult(&resp, result)
	return resp
}

func (s *Server) ensureJobState() (string, string, error) {
	if err := trustedDir(s.cfg.StateDir); err != nil {
		return "", "", err
	}
	jobsDir := filepath.Join(s.cfg.StateDir, "jobs")
	if err := trustedDir(jobsDir); err != nil {
		return "", "", err
	}
	claimsDir := filepath.Join(jobsDir, "claims")
	if err := os.Mkdir(claimsDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	if err := os.Chmod(claimsDir, 0700); err != nil {
		return "", "", err
	}
	if err := trustedDir(claimsDir); err != nil {
		return "", "", err
	}
	return jobsDir, claimsDir, nil
}

func (s *Server) prepareJobFiles(paths jobPaths, req Request) error {
	fileUID, fileGID := uint32(0), uint32(0)
	if !req.Root {
		fileUID, fileGID = s.workerUID, s.workerGID
	}
	created := []string{}
	for _, path := range []string{paths.log, paths.status, paths.started, paths.command} {
		if err := createJobFile(path, fileUID, fileGID); err != nil {
			for _, p := range created {
				_ = os.Remove(p)
			}
			return err
		}
		created = append(created, path)
	}
	if err := os.Chmod(paths.command, 0600); err != nil {
		cleanupJobPaths(paths)
		return err
	}
	f, err := os.OpenFile(paths.command, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		cleanupJobPaths(paths)
		return err
	}
	if _, err = io.WriteString(f, req.Command); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanupJobPaths(paths)
		return err
	}
	return nil
}

func jobPathsFor(jobsDir, id string) jobPaths {
	return jobPaths{
		command: filepath.Join(jobsDir, id+".cmd"),
		log:     filepath.Join(jobsDir, id+".log"),
		status:  filepath.Join(jobsDir, id+".status"),
		started: filepath.Join(jobsDir, id+".started"),
	}
}

func removeJobPaths(paths jobPaths) error {
	var firstErr error
	for _, path := range []string{paths.command, paths.log, paths.status, paths.started} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("remove job artifact %s: %w", path, err)
		}
	}
	return firstErr
}

func cleanupJobPaths(paths jobPaths) {
	_ = removeJobPaths(paths)
}

// Labels are public, bounded operational hints, not commands or credentials.
func validateJobLabel(kind, label string) error {
	if kind == "service" && label == "" {
		return errors.New("service requires a non-secret label")
	}
	if len(label) > 48 {
		return errors.New("job label exceeds 48 bytes")
	}
	for i, c := range label {
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if alnum {
			continue
		}
		if i == 0 || (c != '_' && c != '-' && c != '.') {
			return errors.New("job label must start with a letter or digit and contain only ASCII letters, digits, dot, dash or underscore")
		}
	}
	return nil
}

func validateOperationID(id string) error {
	if id == "" {
		return nil
	}
	if len(id) > maxOperationIDBytes {
		return fmt.Errorf("operation_id exceeds %d bytes", maxOperationIDBytes)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-", r) {
			continue
		}
		return errors.New("operation_id contains unsupported characters")
	}
	return nil
}

func operationClaimName(operationID string, principalID ...string) string {
	principal := ""
	if len(principalID) > 0 {
		principal = principalID[0]
	}
	sum := sha256.Sum256([]byte(principal + "\x00" + operationID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func legacyOperationClaimName(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func (s *Server) jobFingerprint(req Request) string {
	mac := hmac.New(sha256.New, []byte(s.token))
	_, _ = io.WriteString(mac, "persistent-job-v1\x00")
	if req.Root {
		_, _ = io.WriteString(mac, "root\x00")
	} else {
		_, _ = io.WriteString(mac, "worker\x00")
	}
	kind := req.Kind
	if kind == "" {
		kind = "task"
	}
	cwd := req.Workspace
	if cwd == "" {
		if req.Root {
			cwd = "/root"
		} else if root, err := s.workspaceRoot(); err == nil {
			cwd = root
		} else {
			cwd = s.cfg.WorkspaceDir
		}
	}
	_, _ = io.WriteString(mac, cwd+"\x00"+kind+"\x00"+req.Label+"\x00")
	_, _ = mac.Write([]byte(req.Command))
	return hex.EncodeToString(mac.Sum(nil))
}

func createJobClaim(path string, claim jobClaim) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	b, err := json.Marshal(claim)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func updateJobClaim(path string, claim jobClaim) error {
	dir := filepath.Dir(path)
	if err := trustedDir(dir); err != nil {
		return err
	}
	b, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claim-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Locate root-owned recovery proof using the existing protected claim store.
// A worker-controlled status-file string is never affirmative stop evidence.
func findJobClaimForID(dir, id string) (jobClaim, string, bool, error) {
	if err := trustedDir(dir); err != nil {
		if os.IsNotExist(err) {
			return jobClaim{}, "", false, nil
		}
		return jobClaim{}, "", false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return jobClaim{}, "", false, err
	}
	if len(entries) > maxPersistentJobClaims+maxFailedJobClaims {
		return jobClaim{}, "", false, errors.New("job claim directory exceeds bounded recovery capacity")
	}
	var match jobClaim
	var matchPath string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		claim, found, err := readJobClaim(path)
		if err != nil {
			return jobClaim{}, "", false, err
		}
		if !found || claim.JobID != id {
			continue
		}
		if matchPath != "" {
			return jobClaim{}, "", false, errors.New("ambiguous duplicate protected claims for job")
		}
		match, matchPath = claim, path
	}
	return match, matchPath, matchPath != "", nil
}

// Only the root Executor calls this after verifying exact-unit inactivity.
// Atomic claim replacement keeps the proof recoverable across Executor restarts.
func (s *Server) recordVerifiedJobStop(id string, hasStatusRecord bool) error {
	claim, path, found, err := findJobClaimForID(filepath.Join(s.cfg.StateDir, "jobs", "claims"), id)
	if err != nil {
		return err
	}
	if !found {
		// Legacy jobs without any state record retain their old stop
		// acknowledgement. Never silently lose the mandatory protected proof
		// for a newly-recorded job.
		if hasStatusRecord {
			return errors.New("protected job claim missing for existing status record")
		}
		return nil
	}
	if claim.VerifiedStopped {
		return nil
	}
	claim.VerifiedStopped = true
	return updateJobClaim(path, claim)
}

func readJobClaim(path string) (jobClaim, bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return jobClaim{}, false, nil
		}
		return jobClaim{}, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return jobClaim{}, false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0077 != 0 {
		return jobClaim{}, false, errors.New("job claim has unsafe ownership or mode")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return jobClaim{}, false, err
	}
	if len(b) > 4096 {
		return jobClaim{}, false, errors.New("job claim exceeds size limit")
	}
	var claim jobClaim
	if err := json.Unmarshal(b, &claim); err != nil {
		return jobClaim{}, false, err
	}
	if claim.Version != 1 || claim.JobID == "" || claim.Fingerprint == "" {
		return jobClaim{}, false, errors.New("invalid job claim")
	}
	return claim, true, nil
}

func jobClaimFinalizeError(jobID string, err error) Response {
	return Response{
		Error:      "job started but idempotency state could not be finalized: " + err.Error(),
		ReasonCode: "unknown_completion",
		ErrorCode:  "unknown_completion",
		ErrorClass: "state",
		JobID:      jobID,
	}
}

func (s *Server) failJobClaim(path string, claim *jobClaim) {
	if path == "" || claim == nil {
		return
	}
	claim.State = "failed"
	_ = updateJobClaim(path, *claim)
}

func activeAgentJobCount() (int, error) {
	result, err := runBoundedJobSystemd(context.Background(), jobSystemdProbeTimeout, "systemctl", "list-units", "--type=service", "--state=activating,active,deactivating,reloading", "--no-legend", "--no-pager", "ai-job-*.service")
	if err != nil {
		return 0, fmt.Errorf("list persistent jobs: %w: %s", err, result.Output)
	}
	if result.Truncated {
		return 0, errors.New("persistent job listing exceeded its bounded response")
	}
	count := 0
	for _, line := range strings.Split(result.Output, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}

func jobUnitExists(id string) (bool, error) {
	result, err := runBoundedJobSystemd(context.Background(), jobSystemdProbeTimeout, "systemctl", "show", "ai-job-"+id, "--property=LoadState", "--value", "--no-pager")
	value := strings.TrimSpace(result.Output)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, exec.ErrWaitDelay) &&
			(strings.Contains(value, "not-found") || strings.Contains(value, "could not be found")) {
			return false, nil
		}
		return false, fmt.Errorf("inspect persistent job unit: %w: %s", err, value)
	}
	return value != "" && value != "not-found", nil
}

func jobUnitActive(id string) (bool, error) {
	return jobUnitActiveContext(context.Background(), id)
}

func jobUnitActiveContext(parent context.Context, id string) (bool, error) {
	result, err := runBoundedJobSystemd(parent, jobSystemdProbeTimeout, "systemctl", "list-units", "--type=service", "--state=activating,active,deactivating,reloading", "--no-legend", "--no-pager", "ai-job-"+id+".service")
	if err != nil {
		return false, fmt.Errorf("inspect persistent job activity: %w: %s", err, result.Output)
	}
	if result.Truncated {
		return false, errors.New("persistent job activity response exceeded its bounded limit")
	}
	return strings.TrimSpace(result.Output) != "", nil
}

func (s *Server) jobExecutionEvidenceState(paths jobPaths, id string) (bool, bool, error) {
	started, err := s.jobStarted(paths.started)
	if err != nil {
		return false, false, err
	}
	if started {
		active, err := jobUnitActive(id)
		return true, active, err
	}
	exists, err := jobUnitExists(id)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, false, nil
	}
	active, err := jobUnitActive(id)
	return true, active, err
}

func (s *Server) acquirePersistentJobAdmissionLock() (*os.File, *Response) {
	if s.lifecycleLockPath == "" {
		return nil, nil
	}
	if err := trustedDir(filepath.Dir(s.lifecycleLockPath)); err != nil {
		resp := jobStateError("job_state_unavailable", fmt.Errorf("validate lifecycle lock directory: %w", err))
		return nil, &resp
	}
	f, err := os.OpenFile(s.lifecycleLockPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		resp := jobStateError("job_state_unavailable", fmt.Errorf("open lifecycle lock: %w", err))
		return nil, &resp
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		resp := jobStateError("job_state_unavailable", fmt.Errorf("inspect lifecycle lock: %w", err))
		return nil, &resp
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		resp := jobStateError("job_state_unavailable", errors.New("lifecycle lock has unsafe ownership or mode"))
		return nil, &resp
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, &Response{
				Error:      "persistent job admission is unavailable while lifecycle management is active",
				ReasonCode: "resource_limit",
				ErrorCode:  "resource_limit",
				ErrorClass: "resource",
				Retryable:  true,
				Status:     "busy",
			}
		}
		resp := jobStateError("job_state_unavailable", fmt.Errorf("acquire lifecycle admission lock: %w", err))
		return nil, &resp
	}
	return f, nil
}

func releasePersistentJobAdmissionLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func unknownJobLaunchResponse(jobID, message string) Response {
	return Response{
		Error:      message,
		ReasonCode: "unknown_completion",
		ErrorCode:  "unknown_completion",
		ErrorClass: "state",
		JobID:      jobID,
		Status:     "unknown",
	}
}

func removeTerminalCommandHandoff(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove terminal command handoff %s: %w", path, err)
	}
	return nil
}

func verifiedJobStopResponse(jobID string) Response {
	const detail = "stop verified; command exit code unavailable"
	return Response{OK: true, Status: "stopped", JobID: jobID, ExitCode: -1,
		Output: detail, OutputEncoding: "utf-8",
		BytesSeen: int64(len(detail)), BytesReturned: int64(len(detail))}
}

func terminalJobResponse(status, jobID string) Response {
	if status == jobStatusUnknown || status == jobStatusStopped {
		return unknownJobLaunchResponse(jobID, "persistent job ended without a trusted terminal exit status; completion cannot be proven")
	}
	code, err := strconv.Atoi(status)
	if err != nil {
		return jobStateError("job_status_unavailable", fmt.Errorf("invalid terminal job status for %s", jobID))
	}
	return Response{
		OK:             true,
		Status:         "completed",
		Output:         status,
		OutputEncoding: "utf-8",
		BytesSeen:      int64(len(status)),
		BytesReturned:  int64(len(status)),
		ExitCode:       code,
		JobID:          jobID,
	}
}

func (s *Server) persistentTaskCapacity() int {
	n := s.cfg.EffectiveRuntime().PersistentTaskSlots
	if n < 1 || n > maxActivePersistentJobs {
		return 4 // same safe default as a config with no runtime section
	}
	return n
}

func (s *Server) persistentServiceCapacity() int {
	n := s.cfg.EffectiveRuntime().PersistentServiceSlots
	if n < 1 || n > maxActivePersistentServices {
		return 2 // legacy configs and incomplete internal test fixtures
	}
	return n
}

// Kept for fixture-level checks of the maximum independently protected cap.
func persistentJobAdmissionResponse(jobsDir string, kinds ...string) *Response {
	kind := "task"
	if len(kinds) > 0 {
		kind = kinds[0]
	}
	return persistentJobAdmissionResponseWithLimit(jobsDir, maxActivePersistentJobs, kind)
}

// Legacy fixture wrapper exercises the default independent service quota.
func persistentJobAdmissionResponseWithLimit(jobsDir string, taskLimit int, kind string) *Response {
	return persistentJobAdmissionResponseWithLimits(jobsDir, taskLimit, 2, kind)
}

func persistentJobAdmissionResponseWithLimits(jobsDir string, taskLimit, serviceLimit int, kind string) *Response {
	return persistentJobAdmissionResponseWithReserveCheckAndLimits(jobsDir, taskLimit, serviceLimit, kind, jobStateHasReserve)
}

// Legacy fixture wrapper keeps the original default service quota.
func persistentJobAdmissionResponseWithReserveCheck(jobsDir string, taskLimit int, kind string, reserveCheck func(string, int64) (bool, int64, error)) *Response {
	return persistentJobAdmissionResponseWithReserveCheckAndLimits(jobsDir, taskLimit, 2, kind, reserveCheck)
}

// Production and deterministic tests share admission logic with independently
// configured task/service quotas. The filesystem check may be injected only
// in tests; production always examines real disk space.
func persistentJobAdmissionResponseWithReserveCheckAndLimits(jobsDir string, taskLimit, serviceLimit int, kind string, reserveCheck func(string, int64) (bool, int64, error)) *Response {
	if taskLimit < 1 || taskLimit > maxActivePersistentJobs {
		taskLimit = 4
	}
	if serviceLimit < 1 || serviceLimit > maxActivePersistentServices {
		serviceLimit = 2
	}
	counts, err := activeAgentJobKindCounts(jobsDir)
	if err != nil {
		resp := jobStateError("job_state_unavailable", err)
		return &resp
	}
	// After a supported capacity downscale, existing transient jobs remain
	// alive. Enforce hard per-kind bounds but preserve independent quotas.
	if counts.tasks > maxActivePersistentJobs || counts.services > maxActivePersistentServices {
		resp := jobStateError("job_state_unavailable", errors.New("active persistent jobs exceed the hard per-kind bounds"))
		return &resp
	}
	if (kind == "task" && counts.tasks >= taskLimit) ||
		(kind == "service" && counts.services >= serviceLimit) {
		return &Response{
			Error:      "persistent task/service execution capacity is busy",
			ReasonCode: "resource_limit",
			ErrorCode:  "resource_limit",
			ErrorClass: "resource",
			Retryable:  true,
			Status:     "busy",
		}
	}
	if kind != "task" && kind != "service" {
		resp := jobStateError("job_state_unavailable", errors.New("unrecognized persistent job kind"))
		return &resp
	}
	// Keep space for all trusted active task AND service logs after downscale.
	reserve := jobStateSafetyReserveBytesForLimits(max(taskLimit, counts.tasks), max(serviceLimit, counts.services))
	ok, free, err := reserveCheck(jobsDir, reserve)
	if err != nil {
		resp := jobStateError("job_state_unavailable", err)
		return &resp
	}
	if !ok {
		return &Response{
			Error:      fmt.Sprintf("job state filesystem is below the %d-byte safety reserve (%d bytes available)", reserve, free),
			ReasonCode: "resource_limit",
			ErrorCode:  "resource_limit",
			ErrorClass: "resource",
			Retryable:  true,
			Status:     "disk_pressure",
		}
	}
	return nil
}

// Legacy default-service wrappers retain existing fixture semantics.
func jobStateSafetyReserveBytesForActive(taskLimit, activeTasks int) int64 {
	return jobStateSafetyReserveBytesForLimits(max(taskLimit, activeTasks), 2)
}

func jobStateSafetyReserveBytesFor(taskLimit int) int64 {
	return jobStateSafetyReserveBytesForLimits(taskLimit, 2)
}

// Keep space for bounded retained logs plus every configured or still-active
// task/service. Neither service downscaling nor task downscaling deletes units.
func jobStateSafetyReserveBytesForLimits(taskLimit, serviceLimit int) int64 {
	return int64(2*maxCompletedJobArtifacts+taskLimit+serviceLimit)*maxJobLogBytes + jobStateOverheadBytes
}

// Audit is a separate admission boundary: after an Executor restart it must
// preserve the same log reserve for still-active jobs as ordinary job admission.
// If PID1/claim inventory is untrustworthy, reserve the hard ceiling rather
// than silently trusting a possibly downscaled configuration.
func jobAuditSafetyReserveAtStartup(cfg config.Config) int64 {
	runtime := cfg.EffectiveRuntime()
	tasks, services := runtime.PersistentTaskSlots, runtime.PersistentServiceSlots
	if tasks < 1 || tasks > maxActivePersistentJobs {
		tasks = 4
	}
	if services < 1 || services > maxActivePersistentServices {
		services = 2
	}
	counts, err := activeAgentJobKindCounts(filepath.Join(cfg.StateDir, "jobs"))
	if err != nil {
		return jobStateSafetyReserveBytesForLimits(maxActivePersistentJobs, maxActivePersistentServices)
	}
	return jobStateSafetyReserveBytesForLimits(max(tasks, counts.tasks), max(services, counts.services))
}

func jobStateHasReserve(path string, reserve int64) (bool, int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false, 0, err
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return free >= reserve, free, nil
}

func (s *Server) cleanupCompletedJobArtifacts(jobsDir, claimsDir string) error {
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return err
	}
	completed := []completedJobArtifact{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".status") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".status")
		if _, err := safeID(id); err != nil {
			continue
		}
		paths := jobPathsFor(jobsDir, id)
		status, modTime, err := s.readJobStatusValue(paths.status)
		if err != nil {
			return err
		}
		active, err := jobUnitActive(id)
		if err != nil {
			return err
		}
		if active {
			continue
		}
		if status == "" {
			started, err := s.jobStarted(paths.started)
			if err != nil {
				return err
			}
			accepted := started
			if !accepted {
				accepted, err = jobHasStartedClaim(claimsDir, id)
				if err != nil {
					return err
				}
			}
			if !accepted {
				continue
			}
			// A started/accepted job whose transient unit is no longer
			// active/pending and has no durable numeric status ended with an
			// unknown completion state. Persist that state before retention.
			if err := s.markJobStatusUnknownIfEmpty(paths.status); err != nil {
				return err
			}
			status, modTime, err = s.readJobStatusValue(paths.status)
			if err != nil {
				return err
			}
		}
		if status != jobStatusUnknown && status != jobStatusStopped {
			if _, err := strconv.Atoi(status); err != nil {
				return fmt.Errorf("invalid job status file: %s", paths.status)
			}
		}
		if err := removeTerminalCommandHandoff(paths.command); err != nil {
			return err
		}
		completed = append(completed, completedJobArtifact{id: id, modTime: modTime})
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].modTime.After(completed[j].modTime) })
	if len(completed) > maxCompletedJobArtifacts {
		for _, old := range completed[maxCompletedJobArtifacts:] {
			paths := jobPathsFor(jobsDir, old.id)
			if err := removeJobPaths(paths); err != nil {
				return err
			}
			if err := removeClaimsForJob(claimsDir, old.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Terminal status is numeric runner exit evidence. The affirmative stop is
// separately recorded in the protected, atomically replaced Executor claim.
func (s *Server) jobTerminalReplayResponse(statusPath, jobID string) (Response, bool) {
	status, _, err := s.readJobStatusValue(statusPath)
	if err != nil && !os.IsNotExist(err) {
		return jobStateError("job_status_unavailable", err), true
	}
	verified := false
	if status == "" || status == jobStatusUnknown || status == jobStatusStopped {
		claim, _, found, claimErr := findJobClaimForID(filepath.Join(s.cfg.StateDir, "jobs", "claims"), jobID)
		if claimErr != nil {
			return jobStateError("job_status_unavailable", claimErr), true
		}
		verified = found && claim.VerifiedStopped
	}
	if status == "" && !verified {
		return Response{}, false
	}
	active, err := jobUnitActive(jobID)
	if err != nil {
		return jobStateError("job_status_unavailable", err), true
	}
	if active {
		return Response{}, false
	}
	commandPath := strings.TrimSuffix(statusPath, ".status") + ".cmd"
	if err := removeTerminalCommandHandoff(commandPath); err != nil {
		return jobStateError("job_status_unavailable", err), true
	}
	if status != "" && status != jobStatusUnknown && status != jobStatusStopped {
		return terminalJobResponse(status, jobID), true
	}
	if verified {
		return verifiedJobStopResponse(jobID), true
	}
	if status == jobStatusStopped {
		return unknownJobLaunchResponse(jobID, "worker job status cannot prove a verified stop"), true
	}
	return terminalJobResponse(status, jobID), true
}

func (s *Server) readJobStatusValue(path string) (string, time.Time, error) {
	f, err := s.openJobFile(path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer f.Close()
	// Coordinate readers with the runner's numeric exit writer and the
	// unknown-status reconciler on the same status inode.
	if err := lockJobStatusFile(f, syscall.LOCK_SH); err != nil {
		return "", time.Time{}, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	b, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", time.Time{}, err
	}
	if len(b) > 64 {
		return "", time.Time{}, fmt.Errorf("job status file exceeds limit: %s", path)
	}
	fi, err := f.Stat()
	if err != nil {
		return "", time.Time{}, err
	}
	status := strings.TrimSpace(string(b))
	if status != "" && status != jobStatusUnknown && status != jobStatusStopped {
		if _, err := strconv.Atoi(status); err != nil {
			return "", time.Time{}, fmt.Errorf("invalid job status file: %s", path)
		}
	}
	return status, fi.ModTime(), nil
}

func (s *Server) jobStarted(path string) (bool, error) {
	f, err := s.openJobFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 17))
	if err != nil {
		return false, err
	}
	if len(b) > 16 {
		return false, fmt.Errorf("job started marker exceeds limit: %s", path)
	}
	marker := strings.TrimSpace(string(b))
	if marker == "" {
		return false, nil
	}
	if marker != "started" {
		return false, fmt.Errorf("invalid job started marker: %s", path)
	}
	return true, nil
}

// Unknown reconciliation never overwrites numeric runner exit evidence.
// Verified stop proof resides in the root claim, not this worker-owned file.
// A worker can retain a file lock. Bound each privilege-crossing wait so
// status inspection and reconciliation cannot hang the root Executor.
func lockJobStatusFile(f *os.File, mode int) error {
	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		err := syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("job status file is busy; retry after the writer finishes")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Server) markJobStatusUnknownIfEmpty(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || (st.Uid != 0 && st.Uid != s.workerUID) || fi.Mode().Perm()&0002 != 0 {
		return fmt.Errorf("job status file has unsafe ownership or mode: %s", path)
	}
	if err := lockJobStatusFile(f, syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	b, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return err
	}
	if len(b) > 64 {
		return fmt.Errorf("job status file exceeds limit: %s", path)
	}
	if strings.TrimSpace(string(b)) != "" {
		return nil
	}
	if s.jobUnknownBeforeWriteTestHook != nil {
		s.jobUnknownBeforeWriteTestHook()
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.WriteString(f, jobStatusUnknown+"\n"); err != nil {
		return err
	}
	return f.Sync()
}

func countJobClaims(claimsDir string) (int, error) {
	entries, err := os.ReadDir(claimsDir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count, nil
}

func cleanupFailedJobClaims(jobsDir, claimsDir string) error {
	entries, err := os.ReadDir(claimsDir)
	if err != nil {
		return err
	}
	type failedClaim struct {
		path    string
		modTime time.Time
	}
	failed := []failedClaim{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(claimsDir, entry.Name())
		claim, found, err := readJobClaim(path)
		if err != nil {
			return err
		}
		if !found || claim.State != "failed" {
			continue
		}
		id, err := safeID(claim.JobID)
		if err != nil {
			return fmt.Errorf("invalid failed claim job id: %w", err)
		}
		if err := removeJobPaths(jobPathsFor(jobsDir, id)); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		failed = append(failed, failedClaim{path: path, modTime: info.ModTime()})
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].modTime.After(failed[j].modTime) })
	if len(failed) <= maxFailedJobClaims {
		return nil
	}
	for _, old := range failed[maxFailedJobClaims:] {
		if err := os.Remove(old.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func jobHasStartedClaim(claimsDir, jobID string) (bool, error) {
	entries, err := os.ReadDir(claimsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		claim, found, err := readJobClaim(filepath.Join(claimsDir, entry.Name()))
		if err != nil {
			return false, err
		}
		if found && claim.JobID == jobID && claim.State == "started" {
			return true, nil
		}
	}
	return false, nil
}

func removeClaimsForJob(claimsDir, jobID string) error {
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
		if found && claim.JobID == jobID {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func jobStateError(code string, err error) Response {
	return Response{
		Error:      err.Error(),
		ReasonCode: code,
		ErrorCode:  code,
		ErrorClass: "state",
	}
}

func (s *Server) jobOutputBounded(req Request) Response {
	id, err := safeID(req.JobID)
	if err != nil {
		return Response{Error: err.Error(), ReasonCode: "invalid_job_id", ErrorCode: "invalid_job_id", ErrorClass: "validation"}
	}
	path := filepath.Join(s.cfg.StateDir, "jobs", id+".log")
	f, err := s.openJobFile(path)
	if err != nil {
		code := "job_output_unavailable"
		if os.IsNotExist(err) {
			code = "job_not_found"
		}
		return Response{Error: err.Error(), ReasonCode: code, ErrorCode: code, ErrorClass: "state"}
	}
	defer f.Close()

	requested := req.Offset
	if requested < 0 {
		requested = 0
	}
	limit := req.Limit
	if limit <= 0 || limit > maxJobOutputReadBytes {
		limit = maxJobOutputReadBytes
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return jobStateError("job_output_unavailable", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	fi, err := f.Stat()
	if err != nil {
		return jobStateError("job_output_unavailable", err)
	}
	header, ring, err := readJobLogHeader(f, fi.Size())
	if err != nil {
		return jobStateError("job_output_unavailable", err)
	}
	if !ring {
		return readLegacyJobOutput(f, fi.Size(), requested, limit)
	}

	effective := requested
	retentionTruncated := false
	if effective < header.AvailableFrom {
		effective = header.AvailableFrom
		retentionTruncated = true
	}
	if effective > header.CurrentEnd {
		effective = header.CurrentEnd
	}
	remaining := header.CurrentEnd - effective
	want := int64(limit)
	if remaining < want {
		want = remaining
	}
	raw := make([]byte, int(want))
	if want > 0 {
		if err := readRingBytes(f, header, effective, raw); err != nil {
			return jobStateError("job_output_unavailable", err)
		}
	}
	output, encoding := encodeOutputBytes(raw)
	next := effective + int64(len(raw))
	return Response{
		OK:                  true,
		Output:              output,
		OutputEncoding:      encoding,
		BytesReturned:       int64(len(raw)),
		JobID:               id,
		RequestedOffset:     requested,
		Offset:              int64Ptr(requested),
		AvailableFromOffset: header.AvailableFrom,
		NextOffset:          int64Ptr(next),
		CurrentEnd:          header.CurrentEnd,
		EOF:                 boolPtr(next >= header.CurrentEnd),
		RetentionTruncated:  retentionTruncated,
	}
}

func readLegacyJobOutput(f *os.File, size, requested int64, limit int) Response {
	effective := requested
	if effective > size {
		effective = size
	}
	raw := make([]byte, minInt64(int64(limit), size-effective))
	if len(raw) > 0 {
		if _, err := f.ReadAt(raw, effective); err != nil && !errors.Is(err, io.EOF) {
			return jobStateError("job_output_unavailable", err)
		}
	}
	output, encoding := encodeOutputBytes(raw)
	next := effective + int64(len(raw))
	return Response{
		OK:                  true,
		Output:              output,
		OutputEncoding:      encoding,
		BytesReturned:       int64(len(raw)),
		RequestedOffset:     requested,
		AvailableFromOffset: 0,
		Offset:              int64Ptr(requested),
		NextOffset:          int64Ptr(next),
		CurrentEnd:          size,
		EOF:                 boolPtr(next >= size),
	}
}

func minInt64(a, b int64) int {
	if a < b {
		return int(a)
	}
	return int(b)
}

func RunJobHelper(args []string) int {
	fs := flag.NewFlagSet("job-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	commandPath := fs.String("command-file", "", "")
	logPath := fs.String("log-file", "", "")
	statusPath := fs.String("status-file", "", "")
	startedPath := fs.String("started-file", "", "")
	if err := fs.Parse(args); err != nil || *commandPath == "" || *logPath == "" || *statusPath == "" || *startedPath == "" {
		return jobRunnerFailureExit
	}

	logWriter, err := newJobLogWriter(*logPath)
	if err != nil {
		_ = retireRunnerCommandHandoff(*commandPath)
		_ = writeJobStatus(*statusPath, jobRunnerFailureExit)
		return jobRunnerFailureExit
	}
	defer logWriter.Close()

	exitCode := jobRunnerFailureExit
	defer func() {
		if err := writeJobStatus(*statusPath, exitCode); err != nil {
			_, _ = logWriter.Write([]byte("\n[agent job runner could not persist exit status]\n"))
		}
	}()

	if err := writeJobMarker(*startedPath); err != nil {
		if cleanupErr := retireRunnerCommandHandoff(*commandPath); cleanupErr != nil {
			_, _ = logWriter.Write([]byte("\n[agent job runner could not retire protected command handoff after start-marker failure]\n"))
		}
		_, _ = logWriter.Write([]byte("\n[agent job runner could not persist start marker]\n"))
		return exitCode
	}
	command, err := readAndRemoveJobCommand(*commandPath)
	if err != nil {
		_, _ = logWriter.Write([]byte("\n[agent job runner rejected protected command handoff]\n"))
		return exitCode
	}

	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-s")
	cmd.Stdin = bytes.NewReader(command)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	cmd.Env = os.Environ()
	err = cmd.Run()
	exitCode = commandExitCode(err)
	return exitCode
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return jobRunnerFailureExit
}

func readAndRemoveJobCommand(path string) (command []byte, retErr error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	validated := false
	defer func() {
		if validated {
			if err := retireOpenedRunnerCommandHandoff(f, path); retErr == nil && err != nil {
				retErr = err
			}
			return
		}
		_ = f.Close()
	}()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("protected command handoff has unsafe ownership or type")
	}
	if fi.Mode().Perm()&0077 != 0 {
		validated = true
		return nil, errors.New("protected command handoff has unsafe mode")
	}
	validated = true
	command, err = io.ReadAll(io.LimitReader(f, maxCommandBytes+1))
	if err != nil {
		return nil, err
	}
	if len(command) > maxCommandBytes {
		return nil, errors.New("protected command handoff exceeds command size limit")
	}
	return command, nil
}

func retireRunnerCommandHandoff(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return errors.New("protected command handoff has unsafe ownership or mode")
	}
	return retireOpenedRunnerCommandHandoff(f, path)
}

func retireOpenedRunnerCommandHandoff(f *os.File, path string) error {
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) && !errors.Is(err, os.ErrPermission) && !errors.Is(err, syscall.EPERM) {
		return err
	}
	return nil
}

func writeJobMarker(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, "started\n"); err != nil {
		return err
	}
	return f.Sync()
}

func writeJobStatus(path string, code int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockJobStatusFile(f, syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%d\n", code); err != nil {
		return err
	}
	return f.Sync()
}

func newJobLogWriter(path string) (*jobLogWriter, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0002 != 0 {
		f.Close()
		return nil, errors.New("job log has unsafe ownership or mode")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	header, ring, err := readJobLogHeader(f, fi.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ring {
		if fi.Size() != 0 {
			f.Close()
			return nil, errors.New("job log already contains legacy data")
		}
		header = jobLogHeader{Capacity: maxJobLogBytes}
		if err := writeJobLogHeader(f, header); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &jobLogWriter{f: f, h: header}, nil
}

func (w *jobLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if err := syscall.Flock(int(w.f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(w.f.Fd()), syscall.LOCK_UN)

	n := len(p)
	oldEnd := w.h.CurrentEnd
	newEnd := oldEnd + int64(n)
	start := oldEnd
	data := p
	if int64(len(data)) > w.h.Capacity {
		skip := int64(len(data)) - w.h.Capacity
		data = data[skip:]
		start = newEnd - int64(len(data))
	}
	if err := writeRingBytes(w.f, w.h.Capacity, start, data); err != nil {
		return 0, err
	}
	available := newEnd - w.h.Capacity
	if available < 0 {
		available = 0
	}
	w.h.AvailableFrom = available
	w.h.CurrentEnd = newEnd
	if err := writeJobLogHeader(w.f, w.h); err != nil {
		return 0, err
	}
	return n, nil
}

func (w *jobLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Sync()
	closeErr := w.f.Close()
	w.f = nil
	if err != nil {
		return err
	}
	return closeErr
}

func writeJobLogHeader(f *os.File, h jobLogHeader) error {
	var raw [jobLogHeaderSize]byte
	copy(raw[:8], jobLogMagic[:])
	binary.LittleEndian.PutUint64(raw[8:16], uint64(h.Capacity))
	binary.LittleEndian.PutUint64(raw[16:24], uint64(h.AvailableFrom))
	binary.LittleEndian.PutUint64(raw[24:32], uint64(h.CurrentEnd))
	_, err := f.WriteAt(raw[:], 0)
	return err
}

func readJobLogHeader(f *os.File, size int64) (jobLogHeader, bool, error) {
	if size == 0 {
		return jobLogHeader{}, false, nil
	}
	if size < jobLogHeaderSize {
		return jobLogHeader{}, false, nil
	}
	var raw [jobLogHeaderSize]byte
	if _, err := f.ReadAt(raw[:], 0); err != nil {
		return jobLogHeader{}, false, err
	}
	if !bytes.Equal(raw[:8], jobLogMagic[:]) {
		return jobLogHeader{}, false, nil
	}
	h := jobLogHeader{
		Capacity:      int64(binary.LittleEndian.Uint64(raw[8:16])),
		AvailableFrom: int64(binary.LittleEndian.Uint64(raw[16:24])),
		CurrentEnd:    int64(binary.LittleEndian.Uint64(raw[24:32])),
	}
	expectedAvailable := h.CurrentEnd - h.Capacity
	if expectedAvailable < 0 {
		expectedAvailable = 0
	}
	if h.Capacity != maxJobLogBytes || h.AvailableFrom < 0 || h.CurrentEnd < h.AvailableFrom || h.CurrentEnd-h.AvailableFrom > h.Capacity || h.AvailableFrom != expectedAvailable {
		return jobLogHeader{}, false, errors.New("invalid bounded job log header")
	}
	if size > int64(jobLogHeaderSize)+h.Capacity {
		return jobLogHeader{}, false, errors.New("bounded job log exceeds physical size limit")
	}
	return h, true, nil
}

func writeRingBytes(f *os.File, capacity, logicalStart int64, data []byte) error {
	for len(data) > 0 {
		pos := logicalStart % capacity
		room := capacity - pos
		take := int64(len(data))
		if take > room {
			take = room
		}
		if _, err := f.WriteAt(data[:int(take)], int64(jobLogHeaderSize)+pos); err != nil {
			return err
		}
		data = data[int(take):]
		logicalStart += take
	}
	return nil
}

func readRingBytes(f *os.File, h jobLogHeader, logicalStart int64, dst []byte) error {
	for len(dst) > 0 {
		pos := logicalStart % h.Capacity
		room := h.Capacity - pos
		take := int64(len(dst))
		if take > room {
			take = room
		}
		if _, err := f.ReadAt(dst[:int(take)], int64(jobLogHeaderSize)+pos); err != nil {
			return err
		}
		dst = dst[int(take):]
		logicalStart += take
	}
	return nil
}
