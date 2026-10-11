package executor

import (
	"context"
	"errors"
	"log"
	"time"

	"golang.org/x/sys/unix"
)

// The sweeper uses the same serialized, worker-owned staging state and
// Landlock file-helper boundary as a user transfer. No public MCP operation
// dispatches workspace_transfer_gc and it never runs with root filesystem
// authority. It runs at startup and on a bounded schedule, even with no new
// client traffic; a stopped service cannot promise active retention enforcement.
func (s *Server) runTransferSweepLoop(ctx context.Context) {
	const sweepInterval = time.Minute
	timer := time.NewTicker(sweepInterval)
	defer timer.Stop()
	sweep := func() {
		result := s.workerWorkspaceFile(ctx, Request{
			Action: "workspace_transfer_gc", Workspace: s.cfg.WorkspaceDir,
			PrincipalID: "agent-internal", PrincipalClass: "internal",
		})
		if !result.OK && result.ErrorCode != "resource_limit" {
			log.Printf("ai-server-agent transfer sweep incomplete: reason_code=%s error_class=%s", result.ErrorCode, result.ErrorClass)
		}
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			sweep()
		}
	}
}

func workerWorkspaceTransferGC(op workspaceFileOperation) Response {
	if op.WorkspaceRoot == "" || op.Workspace != op.WorkspaceRoot || op.TransferRoot == "" {
		return transferError("invalid_transfer_config", "state", "internal sweep requires configured worker paths")
	}
	root, err := unix.Open(op.TransferRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fileError("transfer_storage_unavailable", "state", err)
	}
	defer unix.Close(root)
	if err = unix.Flock(root, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return transferError("transfer_busy", "capacity", "active transfer owns staging lock")
		}
		return fileError("transfer_lock_failed", "io", err)
	}
	defer unix.Flock(root, unix.LOCK_UN)
	_, _, _, err = transferInventory(op, time.Now().Unix())
	if err != nil {
		return fileError("transfer_cleanup_failed", "state", err)
	}
	return Response{OK: true, Status: "swept"}
}
