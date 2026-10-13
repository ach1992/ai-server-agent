package executor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxRuntimeListEntries = 32

type RuntimeEntry struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Label     string `json:"label,omitempty"`
	Root      bool   `json:"root,omitempty"`
}

type RuntimeCapacity struct {
	TaskLimit          int  `json:"task_limit"`
	ServiceLimit       int  `json:"service_limit"`
	TasksActive        int  `json:"tasks_active"`
	ServicesActive     int  `json:"services_active"`
	UnclassifiedActive int  `json:"unclassified_active"`
	StateReliable      bool `json:"state_reliable"`
}

func runtimeRank(s string) int {
	switch s {
	case "active", "pending":
		return 0
	case "unknown":
		return 1
	default:
		return 2
	}
}

func (s *Server) runtimeList(req Request) Response {
	if req.PrincipalID == "" || req.PrincipalClass == "" {
		return fileError("invalid_principal", "authorization", errors.New("authenticated principal required for runtime discovery"))
	}
	switch req.Kind {
	case "", "task", "service", "terminal":
	default:
		return fileError("invalid_kind", "validation", errors.New("kind must be task, service, terminal or omitted"))
	}
	workspace := ""
	if req.Workspace != "" {
		var err error
		workspace, err = s.workerCommandWorkspace(req.Workspace)
		if err != nil {
			return fileError("invalid_workspace", "validation", err)
		}
	}
	now := time.Now().UTC()
	response := Response{
		OK:              true,
		Status:          "observed",
		GeneratedAt:     now,
		RuntimeEntries:  []RuntimeEntry{},
		RuntimeCapacity: &RuntimeCapacity{TaskLimit: maxActivePersistentJobs, ServiceLimit: maxActivePersistentServices},
	}
	cap := response.RuntimeCapacity
	// A passive inspection never creates directories, claims, markers or units.
	if err := trustedDir(s.cfg.StateDir); err != nil {
		return jobStateError("runtime_unavailable", err)
	}
	jobsDir := filepath.Join(s.cfg.StateDir, "jobs")
	if err := trustedDir(jobsDir); err != nil {
		return jobStateError("runtime_unavailable", err)
	}
	ids, err := activeAgentUnitIDs()
	active := make(map[string]bool, len(ids))
	if err != nil {
		response.Truncated = true // the active state and capacity are not established
	} else {
		for _, id := range ids {
			active[id] = true
		}
	}
	dir := filepath.Join(jobsDir, "claims")
	claims := map[string]jobClaim{}
	if err := trustedDir(dir); err != nil {
		if !os.IsNotExist(err) {
			return jobStateError("runtime_unavailable", err)
		}
	} else {
		files, readErr := os.ReadDir(dir)
		if readErr != nil {
			return jobStateError("runtime_unavailable", readErr)
		}
		if len(files) > maxPersistentJobClaims+maxFailedJobClaims {
			response.Truncated = true
		} else {
			for _, entry := range files {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					response.Truncated = true
					continue
				}
				claim, found, readErr := readJobClaim(filepath.Join(dir, entry.Name()))
				if readErr != nil || !found {
					response.Truncated = true
					continue
				}
				if _, err := safeID(claim.JobID); err != nil {
					response.Truncated = true
					continue
				}
				if _, exists := claims[claim.JobID]; exists {
					response.Truncated = true
					continue
				}
				claims[claim.JobID] = claim
			}
		}
	}
	cap.StateReliable = err == nil && !response.Truncated
	for _, id := range ids {
		claim, found := claims[id]
		if !found || (claim.Kind != "task" && claim.Kind != "service") || claim.Workspace == "" {
			cap.UnclassifiedActive++
			cap.StateReliable = false
			continue
		}
		if claim.Kind == "task" {
			cap.TasksActive++
		} else {
			cap.ServicesActive++
		}
	}
	if req.Kind == "" || req.Kind == "task" || req.Kind == "service" {
		for id, claim := range claims {
			if claim.PrincipalID != req.PrincipalID || claim.PrincipalClass != req.PrincipalClass {
				continue
			}
			if claim.Workspace == "" || (claim.Kind != "task" && claim.Kind != "service") {
				response.Truncated = true
				continue
			}
			if workspace != "" && workspace != claim.Workspace {
				continue
			}
			if req.Kind != "" && req.Kind != claim.Kind {
				continue
			}
			status := "unknown"
			switch {
			case active[id]:
				status = "active"
			case err != nil:
				status = "unknown"
			case claim.State == "failed":
				status = "failed"
			case claim.State == "claimed":
				status = "pending"
			default:
				value, _, readErr := s.readJobStatusValue(filepath.Join(jobsDir, id+".status"))
				if readErr == nil && value != "" {
					if value == jobStatusUnknown {
						status = "unknown"
					} else if _, parseErr := strconv.Atoi(value); parseErr == nil {
						status = "completed"
					}
				}
			}
			response.RuntimeEntries = append(response.RuntimeEntries, RuntimeEntry{
				Kind: claim.Kind, ID: id, Workspace: claim.Workspace, Status: status,
				CreatedAt: claim.CreatedAt, Label: claim.Label, Root: claim.Root,
			})
		}
	}
	if req.Kind == "" || req.Kind == "terminal" {
		recordsDir := filepath.Join(s.cfg.StateDir, "terminal-sessions")
		if err := trustedDir(recordsDir); err == nil {
			entries, readErr := os.ReadDir(recordsDir)
			if readErr != nil {
				response.Truncated = true
			} else if len(entries) > maxRuntimeListEntries*4 {
				response.Truncated = true
			} else {
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
						continue
					}
					id := strings.TrimSuffix(entry.Name(), ".json")
					if !validTerminalID(id) {
						response.Truncated = true
						continue
					}
					f, openErr := os.OpenFile(filepath.Join(recordsDir, entry.Name()), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
					if openErr != nil {
						response.Truncated = true
						continue
					}
					fi, statErr := f.Stat()
					var rec terminalRecord
					valid := statErr == nil && fi.Mode().IsRegular() && fi.Mode().Perm() == 0600 && fi.Size() <= 4096
					if valid {
						st, ok := fi.Sys().(*syscall.Stat_t)
						valid = ok && st.Uid == uint32(os.Geteuid())
					}
					if valid {
						data, readErr := io.ReadAll(io.LimitReader(f, 4097))
						valid = readErr == nil && len(data) <= 4096 && json.Unmarshal(data, &rec) == nil
					}
					_ = f.Close()
					if !valid {
						response.Truncated = true
						continue
					}
					// Never offer root-PTY discovery or cross-principal records.
					if rec.OwnerID != req.PrincipalID || rec.OwnerClass != req.PrincipalClass || rec.Root {
						continue
					}
					if workspace != "" && rec.Workspace != workspace {
						continue
					}
					if rec.ID != id || rec.Version != 1 || !rec.Scoped || rec.Lifecycle != "active" ||
						!validTmuxSessionName(rec.Name) || rec.ExpiresAt.IsZero() ||
						!rec.ExpiresAt.After(now) {
						continue
					}
					status := "active"
					if err := verifyScopedTerminalBackend(rec.Name); err != nil {
						status = "unknown"
					}
					response.RuntimeEntries = append(response.RuntimeEntries, RuntimeEntry{
						Kind: "terminal", ID: rec.ID, Workspace: rec.Workspace, Status: status,
						ExpiresAt: rec.ExpiresAt.UTC().Format(time.RFC3339),
					})
				}
			}
		} else if !os.IsNotExist(err) {
			response.Truncated = true
		}
	}
	sort.Slice(response.RuntimeEntries, func(i, j int) bool {
		a, b := response.RuntimeEntries[i], response.RuntimeEntries[j]
		if runtimeRank(a.Status) != runtimeRank(b.Status) {
			return runtimeRank(a.Status) < runtimeRank(b.Status)
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID > b.ID
	})
	if len(response.RuntimeEntries) > maxRuntimeListEntries {
		response.RuntimeEntries = response.RuntimeEntries[:maxRuntimeListEntries]
		response.Truncated = true
	}
	return response
}
