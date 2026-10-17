package executor

import (
	"context"
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

const (
	maxRuntimeListEntries         = 32
	runtimeListObservationTimeout = 5 * time.Second
)

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
	return s.runtimeListContext(context.Background(), req)
}

func (s *Server) runtimeListContext(parent context.Context, req Request) Response {
	timeout := runtimeListObservationTimeout
	if s.runtimeListObservationTimeoutTest > 0 {
		timeout = s.runtimeListObservationTimeoutTest
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
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
		RuntimeCapacity: &RuntimeCapacity{TaskLimit: s.persistentTaskCapacity(), ServiceLimit: maxActivePersistentServices},
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
	ids, err := activeAgentUnitIDsContext(ctx)
	active := make(map[string]bool, len(ids))
	if err != nil {
		response.Truncated = true // the active state and capacity are not established
		if ctx.Err() != nil {
			return response
		}
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
		files, overflow, readErr := readDirBounded(dir, maxPersistentJobClaims+maxFailedJobClaims)
		if readErr != nil {
			return jobStateError("runtime_unavailable", readErr)
		}
		if overflow {
			response.Truncated = true
		} else {
			for _, entry := range files {
				if ctx.Err() != nil {
					response.Truncated = true
					break
				}
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
			if ctx.Err() != nil {
				response.Truncated = true
				break
			}
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
					} else if value == jobStatusStopped {
						status = "stopped"
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
			entries, overflow, readErr := readDirBounded(recordsDir, maxRuntimeListEntries*4)
			if readErr != nil {
				response.Truncated = true
			} else if overflow {
				response.Truncated = true
			} else {
				for _, entry := range entries {
					if ctx.Err() != nil {
						response.Truncated = true
						break
					}
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
						!validTmuxSessionName(rec.Name) || !validTmuxPaneID(rec.Pane) ||
						!filepath.IsAbs(rec.Workspace) || filepath.Clean(rec.Workspace) != rec.Workspace ||
						rec.ExpiresAt.IsZero() || !rec.ExpiresAt.After(now) ||
						!passiveWorkerTerminalSocketPresent(s.cfg.StateDir, rec) {
						continue
					}
					status := "active"
					if err := s.verifyRuntimeTerminalBackend(ctx, rec.Name); err != nil {
						status = "unknown"
						if ctx.Err() != nil {
							response.Truncated = true
						}
					}
					response.RuntimeEntries = append(response.RuntimeEntries, RuntimeEntry{
						Kind: "terminal", ID: rec.ID, Workspace: rec.Workspace, Status: status,
						CreatedAt: rec.ExpiresAt.Add(-maxStdioSessionAge).UTC().Format(time.RFC3339Nano),
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

func (s *Server) verifyRuntimeTerminalBackend(ctx context.Context, name string) error {
	if s.runtimeTerminalVerifyTestHook != nil {
		return s.runtimeTerminalVerifyTestHook(ctx, name)
	}
	return verifyScopedTerminalBackendContext(ctx, name)
}

// This read-only check parallels the production reconnect socket identity
// boundary without calling terminalRecordsDir (which creates directories),
// opening a tmux client, or refreshing a terminal expiry.
func passiveWorkerTerminalSocketPresent(stateDir string, rec terminalRecord) bool {
	base := filepath.Join(stateDir, "worker-terminals")
	if !filepath.IsAbs(rec.SocketDir) || rec.SocketDir == base ||
		!withinPath(base, rec.SocketDir) ||
		!strings.HasPrefix(filepath.Base(rec.SocketDir), ".asa-tmux-") {
		return false
	}
	socket := filepath.Join(rec.SocketDir, "socket")
	if len(socket) > terminalMaxUnixSocketPath {
		return false
	}
	info, err := os.Lstat(rec.SocketDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	info, err = os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return false
	}
	st, ok = info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

// Read only one entry beyond the accepted state bound. A root-controlled
// namespace can still be corrupt; inventory must not allocate an unbounded
// list of directory names before setting truncated/unknown.
func readDirBounded(path string, limit int) ([]os.DirEntry, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	entries, err := f.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}
