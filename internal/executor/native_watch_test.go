package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/managedincus"
)

// A restart can recover more pending journal records than the fixed 32
// concurrent native watchers. Every record must eventually get an observation
// opportunity; no unbounded goroutine spawn or silent drop is acceptable.
func TestNativeObserverQueueRecoversMoreThanThirtyTwoPendingOperations(t *testing.T) {
	b := managedincus.Backend{HostID: "asa_test_host", StateDir: t.TempDir()}
	dir := filepath.Join(b.StateDir, "managed-incus")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	operations := make([]managedincus.NativeOperation, 40)
	for i := range operations {
		operations[i] = managedincus.NativeOperation{
			HostID: b.HostID, WorkID: "issue221", ID: fmt.Sprintf("queue-op-%02d", i),
			Project: "default", Instance: "guest-a", UUID: "c8922954-19cd-4cbe-974c-adba463cd59a",
			Action: "snapshot", Snapshot: fmt.Sprintf("queue-snap-%02d", i),
			NativeID: fmt.Sprintf("22222222-2222-4222-8222-%012d", i),
			State:    "running", SubmittedAt: now, UpdatedAt: now,
		}
	}
	journal, err := json.Marshal(map[string]any{"version": 1, "operations": operations})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "native-operations.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := b.RecoverableNativeOperations()
	if err != nil || len(recovered) != len(operations) {
		t.Fatalf("restart did not discover all pending records: count=%d err=%v", len(recovered), err)
	}

	release := make(chan struct{})
	tailStarted := make(chan struct{}, 1)
	var mu sync.Mutex
	observed := make([]string, 0, len(operations))
	server := &Server{}
	server.managedWatchObserveTestHook = func(ctx context.Context, id string) bool {
		if id == "queue-op-32" {
			select {
			case tailStarted <- struct{}{}:
			default:
			}
		}
		select {
		case <-release:
			mu.Lock()
			observed = append(observed, id)
			mu.Unlock()
			return true // Simulated persisted native terminal proof.
		case <-ctx.Done():
			return false
		}
	}
	for _, op := range recovered {
		server.watchNativeOperation(b, op)
	}
	// Duplicate watcher admission must not consume another worker/queue slot.
	server.watchNativeOperation(b, recovered[0])
	server.managedWatchMu.Lock()
	active, queued, registered := server.managedWatchActive, len(server.managedWatchQueue), len(server.managedWatching)
	server.managedWatchMu.Unlock()
	if active != 32 || queued != 8 || registered != 40 {
		close(release)
		t.Fatalf("observer admission lost work or exceeded bound: active=%d queued=%d registered=%d", active, queued, registered)
	}
	// The first 32 operations remain unresolved until their observation
	// time slice expires. The 33rd must nevertheless receive a turn.
	select {
	case <-tailStarted:
	case <-time.After(4 * time.Second):
		close(release)
		t.Fatal("queued operation starved behind 32 long-running observers")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		server.managedWatchMu.Lock()
		left, running := len(server.managedWatching), server.managedWatchActive
		server.managedWatchMu.Unlock()
		if running > 32 {
			t.Fatal("more than 32 native watchers were started")
		}
		if left == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	server.managedWatchMu.Lock()
	left := len(server.managedWatching)
	server.managedWatchMu.Unlock()
	mu.Lock()
	captured := append([]string(nil), observed...)
	mu.Unlock()
	if left != 0 || len(captured) != 40 {
		t.Fatalf("observation queue dropped pending operations: left=%d observed=%d", left, len(captured))
	}
	seen := map[string]bool{}
	for _, id := range captured {
		if seen[id] {
			t.Fatalf("operation observed more than once on terminal completion: %s", id)
		}
		seen[id] = true
	}
}
