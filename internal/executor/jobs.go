package executor

import (
	"bytes"
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

	"github.com/ach1992/ai-server-agent/internal/audit"
)

const (
	maxActivePersistentJobs   = 4
	maxCompletedJobArtifacts  = 32
	maxFailedJobClaims         = maxCompletedJobArtifacts
	maxIdempotencyClaims       = 2*maxCompletedJobArtifacts + maxActivePersistentJobs
	maxJobLogBytes      int64 = 8 << 20
	jobLogHeaderSize           = 32
	maxOperationIDBytes        = 128
	systemdOutputLimit         = 64 << 10
	jobRunnerFailureExit       = 125
	jobStateOverheadBytes int64 = 64 << 20
	jobStateSafetyReserveBytes = int64(maxActivePersistentJobs+maxCompletedJobArtifacts)*maxJobLogBytes + jobStateOverheadBytes
)

var jobLogMagic = [8]byte{'A', 'I', 'S', 'A', 'J', 'L', '0', '1'}

type jobPaths struct {
	command string
	log     string
	status  string
	started string
}

type jobClaim struct {
	Version     int    `json:"version"`
	JobID       string `json:"job_id"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
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

func (s *Server) startJobBounded(req Request) Response {
	if bad := commandInputError(req.Command); bad != nil {
		return *bad
	}
	dec := s.guard.Evaluate(req.Command, req.Root)
	if !dec.Allowed {
		return Response{Error: dec.Reason, ReasonCode: "policy_denied", ErrorCode: "policy_denied", ErrorClass: "policy"}
	}
	if dec.RequiresApproval && !req.Approval {
		return Response{Error: "approval_required", ReasonCode: "approval_required", ErrorCode: "approval_required", ErrorClass: "approval", Approval: dec}
	}
	if err := validateOperationID(req.OperationID); err != nil {
		return Response{Error: err.Error(), ReasonCode: "invalid_operation_id", ErrorCode: "invalid_operation_id", ErrorClass: "validation"}
	}

	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()

	if req.OperationID == "" {
		req.OperationID = "internal-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}

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
		claimPath = filepath.Join(claimsDir, operationClaimName(req.OperationID))
		if claim, found, err := readJobClaim(claimPath); err != nil {
			return jobStateError("job_state_unavailable", err)
		} else if found {
			return s.resumeClaimedJob(req, jobsDir, claimPath, claim, fingerprint)
		}
		if err := cleanupFailedJobClaims(claimsDir); err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		claimCount, err := countJobClaims(claimsDir)
		if err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		if claimCount >= maxIdempotencyClaims {
			return Response{
				Error:      "persistent-job idempotency state is at capacity; reconcile existing operation_ids before creating another",
				ReasonCode: "resource_limit",
				ErrorCode:  "resource_limit",
				ErrorClass: "resource",
				Retryable:  true,
				Status:     "busy",
			}
		}
	}

	active, err := activeAgentJobCount()
	if err != nil {
		return jobStateError("job_state_unavailable", err)
	}
	if active >= maxActivePersistentJobs {
		return Response{
			Error:      "persistent job execution capacity is busy",
			ReasonCode: "resource_limit",
			ErrorCode:  "resource_limit",
			ErrorClass: "resource",
			Retryable:  true,
			Status:     "busy",
		}
	}
	ok, free, err := jobStateHasReserve(jobsDir)
	if err != nil {
		return jobStateError("job_state_unavailable", err)
	}
	if !ok {
		return Response{
			Error:      fmt.Sprintf("job state filesystem is below the %d-byte safety reserve (%d bytes available)", jobStateSafetyReserveBytes, free),
			ReasonCode: "resource_limit",
			ErrorCode:  "resource_limit",
			ErrorClass: "resource",
			Retryable:  true,
			Status:     "disk_pressure",
		}
	}

	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	paths := jobPathsFor(jobsDir, id)
	claim := jobClaim{
		Version:     1,
		JobID:       id,
		Fingerprint: fingerprint,
		State:       "claimed",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
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

	claim.State = "launching"
	if claimPath != "" {
		if err := updateJobClaim(claimPath, claim); err != nil {
			s.failJobClaim(claimPath, &claim)
			cleanupJobPaths(paths)
			return jobStateError("job_state_unavailable", err)
		}
	}

	resp := s.launchPreparedJob(req, paths, id)
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
	} else if claimPath != "" {
		s.failJobClaim(claimPath, &claim)
	}

	_ = s.audit.Write(audit.Entry{
		Action:  "start_job",
		Mode:    map[bool]string{true: "root", false: "worker"}[req.Root],
		Success: resp.OK,
		Detail:  dec.Category,
	})
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
	if _, err := safeID(claim.JobID); err != nil {
		return jobStateError("job_state_unavailable", fmt.Errorf("invalid claimed job id: %w", err))
	}
	paths := jobPathsFor(jobsDir, claim.JobID)

	switch claim.State {
	case "started":
		return Response{OK: true, JobID: claim.JobID, Status: "accepted", IdempotentReplay: true}
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

	evidence, err := jobHasExecutionEvidence(paths, claim.JobID)
	if err != nil {
		return jobStateError("job_state_unavailable", err)
	}
	if evidence {
		claim.State = "started"
		if err := updateJobClaim(claimPath, claim); err != nil {
			return jobStateError("job_state_unavailable", err)
		}
		return Response{OK: true, JobID: claim.JobID, Status: "accepted", IdempotentReplay: true}
	}

	if claim.State == "claimed" {
		// The durable claim is written before any launch attempt. If the
		// executor crashed while preparing the protected handoff, no unit could
		// have been launched yet. Rebuild that handoff from the retried,
		// fingerprint-matched request instead of leaving a permanent claim.
		cleanupJobPaths(paths)
		if err := s.prepareJobFiles(paths, req); err != nil {
			claim.State = "failed"
			_ = updateJobClaim(claimPath, claim)
			return jobStateError("job_prepare_failed", err)
		}
		claim.State = "launching"
		if err := updateJobClaim(claimPath, claim); err != nil {
			cleanupJobPaths(paths)
			return jobStateError("job_state_unavailable", err)
		}
	} else {
		if _, err := os.Lstat(paths.command); err != nil {
			if os.IsNotExist(err) {
				claim.State = "failed"
				_ = updateJobClaim(claimPath, claim)
				return Response{
					Error:            "persistent-job launch state was interrupted after launch became possible; completion cannot be proven; use a new operation_id to retry",
					ReasonCode:       "unknown_completion",
					ErrorCode:        "unknown_completion",
					ErrorClass:       "state",
					JobID:            claim.JobID,
					IdempotentReplay: true,
				}
			}
			return jobStateError("job_state_unavailable", err)
		}
	}

	resp := s.launchPreparedJob(req, paths, claim.JobID)
	resp.IdempotentReplay = true
	if resp.OK {
		claim.State = "started"
		if err := updateJobClaim(claimPath, claim); err != nil {
			return jobStateError("job_state_unavailable", err)
		}
	} else {
		claim.State = "failed"
		_ = updateJobClaim(claimPath, claim)
	}
	return resp
}

func (s *Server) launchPreparedJob(req Request, paths jobPaths, id string) Response {
	home := s.cfg.WorkspaceDir
	workDir := s.cfg.WorkspaceDir
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

	out := newBoundedOutputCollector(systemdOutputLimit)
	cmd := exec.Command("systemd-run", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	err = cmd.Run()
	result := out.Result()
	if err != nil {
		evidence, evidenceErr := jobHasExecutionEvidence(paths, id)
		if evidenceErr != nil {
			return jobStateError("unknown_completion", fmt.Errorf("systemd-run failed and job state could not be reconciled: %w", evidenceErr))
		}
		if !evidence {
			_ = os.Remove(paths.command)
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

func cleanupJobPaths(paths jobPaths) {
	_ = os.Remove(paths.command)
	_ = os.Remove(paths.log)
	_ = os.Remove(paths.status)
	_ = os.Remove(paths.started)
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

func operationClaimName(operationID string) string {
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

func (s *Server) failJobClaim(path string, claim *jobClaim) {
	if path == "" || claim == nil {
		return
	}
	claim.State = "failed"
	_ = updateJobClaim(path, *claim)
}

func activeAgentJobCount() (int, error) {
	out := newBoundedOutputCollector(systemdOutputLimit)
	cmd := exec.Command("systemctl", "list-units", "--type=service", "--state=activating,active,deactivating,reloading", "--no-legend", "--no-pager", "ai-job-*.service")
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("list persistent jobs: %w: %s", err, out.Result().Output)
	}
	result := out.Result()
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
	out := newBoundedOutputCollector(systemdOutputLimit)
	cmd := exec.Command("systemctl", "show", "ai-job-"+id, "--property=LoadState", "--value", "--no-pager")
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	result := out.Result()
	value := strings.TrimSpace(result.Output)
	if err != nil {
		if strings.Contains(value, "not-found") || strings.Contains(value, "could not be found") {
			return false, nil
		}
		return false, fmt.Errorf("inspect persistent job unit: %w: %s", err, value)
	}
	return value != "" && value != "not-found", nil
}

func jobHasExecutionEvidence(paths jobPaths, id string) (bool, error) {
	for _, path := range []string{paths.started, paths.status} {
		fi, err := os.Lstat(path)
		if err == nil {
			if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
				return false, fmt.Errorf("job evidence path is unsafe: %s", path)
			}
			if fi.Size() > 0 {
				return true, nil
			}
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return jobUnitExists(id)
}

func jobStateHasReserve(path string) (bool, int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false, 0, err
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return free >= jobStateSafetyReserveBytes, free, nil
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
		statusPath := filepath.Join(jobsDir, entry.Name())
		f, err := s.openJobFile(statusPath)
		if err != nil {
			return err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, 65))
		fi, statErr := f.Stat()
		_ = f.Close()
		if readErr != nil || statErr != nil {
			if readErr != nil {
				return readErr
			}
			return statErr
		}
		status := strings.TrimSpace(string(b))
		if status == "" {
			continue
		}
		if len(b) > 64 {
			return fmt.Errorf("job status file exceeds limit: %s", statusPath)
		}
		if _, err := strconv.Atoi(status); err != nil {
			return fmt.Errorf("invalid job status file: %s", statusPath)
		}
		_ = os.Remove(filepath.Join(jobsDir, id+".cmd"))
		completed = append(completed, completedJobArtifact{id: id, modTime: fi.ModTime()})
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].modTime.After(completed[j].modTime) })
	if len(completed) > maxCompletedJobArtifacts {
		for _, old := range completed[maxCompletedJobArtifacts:] {
			paths := jobPathsFor(jobsDir, old.id)
			cleanupJobPaths(paths)
			if err := removeClaimsForJob(claimsDir, old.id); err != nil {
				return err
			}
		}
	}
	return nil
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

func cleanupFailedJobClaims(claimsDir string) error {
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
		AvailableFromOffset: header.AvailableFrom,
		NextOffset:          next,
		CurrentEnd:          header.CurrentEnd,
		EOF:                 next >= header.CurrentEnd,
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
		NextOffset:          next,
		CurrentEnd:          size,
		EOF:                 next >= size,
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

func readAndRemoveJobCommand(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	validated := false
	defer func() {
		_ = f.Close()
		if validated {
			_ = os.Remove(path)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0077 != 0 {
		return nil, errors.New("protected command handoff has unsafe ownership or mode")
	}
	validated = true
	b, err := io.ReadAll(io.LimitReader(f, maxCommandBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCommandBytes {
		return nil, errors.New("protected command handoff exceeds command size limit")
	}
	return b, nil
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
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
