package executor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// This bounded, read-only PID1 view is shared by admission and discovery.
// Never return systemctl's unit descriptions: they can include shell text.
func activeAgentUnitIDs() ([]string, error) {
	return activeAgentUnitIDsContext(context.Background())
}

func activeAgentUnitIDsContext(parent context.Context) ([]string, error) {
	out := newBoundedOutputCollector(systemdOutputLimit)
	// Inventory runs on both admission and passive discovery; never let a
	// stalled PID1/dbus interaction monopolize either path indefinitely.
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service",
		"--state=activating,active,deactivating,reloading",
		"--no-legend", "--no-pager", "ai-job-*.service")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("systemd job inventory unavailable: %w", err)
	}
	result := out.Result()
	if result.Truncated {
		return nil, errors.New("systemd job inventory exceeds bound")
	}
	ids := make([]string, 0, maxActivePersistentJobs+maxActivePersistentServices)
	seen := map[string]bool{}
	for _, line := range strings.Split(result.Output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		if !strings.HasPrefix(unit, "ai-job-") || !strings.HasSuffix(unit, ".service") {
			return nil, errors.New("systemd job inventory contains unrecognized unit")
		}
		id := strings.TrimSuffix(strings.TrimPrefix(unit, "ai-job-"), ".service")
		if _, err := safeID(id); err != nil || seen[id] {
			return nil, errors.New("systemd job inventory contains untrusted identity")
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) > maxActivePersistentJobs+maxActivePersistentServices {
			return nil, errors.New("active Agent units exceed configured total capacity")
		}
	}
	return ids, nil
}

type activeKindCounts struct{ tasks, services int }

// The active unit *alone* cannot attest whether its job is a task or service.
// The protected record is authoritative; missing/corrupt records fail closed.
func activeAgentJobKindCounts(jobsDir string) (activeKindCounts, error) {
	ids, err := activeAgentUnitIDs()
	if err != nil {
		return activeKindCounts{}, err
	}
	counts := activeKindCounts{}
	if len(ids) == 0 {
		return counts, nil
	}
	dir := filepath.Join(jobsDir, "claims")
	if err := trustedDir(dir); err != nil {
		return counts, err
	}
	files, truncated, err := readDirBounded(dir, maxPersistentJobClaims+maxFailedJobClaims)
	if err != nil {
		return counts, err
	}
	if truncated {
		return counts, errors.New("persistent job claim namespace exceeds its bound")
	}
	claims := make(map[string]jobClaim, len(files))
	for _, entry := range files {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		claim, found, err := readJobClaim(filepath.Join(dir, entry.Name()))
		if err != nil {
			return counts, err
		}
		if found {
			if _, exists := claims[claim.JobID]; exists {
				return counts, errors.New("duplicate persistent job identity")
			}
			claims[claim.JobID] = claim
		}
	}
	for _, id := range ids {
		claim, exists := claims[id]
		if !exists || claim.Workspace == "" {
			return counts, errors.New("active Agent unit lacks a trusted job record")
		}
		switch claim.Kind {
		case "task":
			counts.tasks++
		case "service":
			counts.services++
		default:
			return counts, errors.New("active Agent unit kind is untrusted")
		}
	}
	return counts, nil
}
