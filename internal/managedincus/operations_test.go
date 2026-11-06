package managedincus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func nativeStatusReq(req Request) Request {
	return Request{Operation: "operation_status", Project: req.Project, Instance: req.Instance, UUID: req.UUID, OperationID: req.OperationID}
}
func mode(t *testing.T, b Backend, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestNativeAsyncStatusIsDurableAndReplaySafe(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "safe-a", OperationID: "snap-unique-id"}
	first, err := b.Run(ctx, req, false, "")
	if err != nil || first.Status != "running" || first.Operation == nil || first.Operation.NativeID == "" {
		t.Fatalf("submit %+v %v", first, err)
	}
	// Simulate a long native operation, repeated MCP delivery, and a new Agent.
	mode(t, b, "api_running")
	recovered := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe, apiOverride: b.apiOverride}
	running, err := recovered.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || running.Status != "running" {
		t.Fatalf("read after restart %+v %v", running, err)
	}
	initialInfo, err := os.Stat(b.nativeOperationsPath())
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := recovered.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || repeat.Status != "running" {
		t.Fatalf("poll %+v %v", repeat, err)
	}
	sameInfo, err := os.Stat(b.nativeOperationsPath())
	if err != nil || !os.SameFile(initialInfo, sameInfo) {
		t.Fatal("unchanged running poll rewrote durable journal unnecessarily")
	}

	replay, err := recovered.Run(ctx, req, false, "")
	if err != nil || replay.Status != "running" || replay.Operation.NativeID != first.Operation.NativeID {
		t.Fatalf("duplicate submission %+v %v", replay, err)
	}
	if _, err = recovered.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: req.UUID}, false, ""); err == nil || !strings.Contains(err.Error(), "native_operation_in_flight") {
		t.Fatalf("overlapping mutation accepted: %v", err)
	}
	mode(t, b, "")
	done, err := recovered.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || done.Status != "native_succeeded" {
		t.Fatalf("terminal status %+v %v", done, err)
	}
	// A native operation may have expired. Last verified terminal state remains.
	mode(t, b, "api_op_expired")
	persisted, err := recovered.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || persisted.Status != "native_succeeded" {
		t.Fatalf("terminal state lost %+v %v", persisted, err)
	}
	_, err = recovered.Run(ctx, Request{Operation: "snapshot", Project: req.Project, Instance: req.Instance, UUID: req.UUID, Snapshot: req.Snapshot, OperationID: "different-id"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "snapshot_collision") {
		t.Fatalf("existing snapshot duplicated: %v", err)
	}
}
func TestNativeOutcomeExpiredBeforeObservationNeverSucceeds(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "possible", OperationID: "snap-expire"}
	if _, err = b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	mode(t, b, "api_op_expired")
	r, err := b.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || r.Status != "outcome_uncertain" {
		t.Fatalf("expired op incorrectly marked complete: %+v %v", r, err)
	}
	if _, err = b.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: req.UUID}, false, ""); err == nil || !strings.Contains(err.Error(), "native_operation_in_flight") {
		t.Fatalf("unsafe retry after expired op: %v", err)
	}
}
func TestNativeFailedOperationRecordedAndNotResubmitted(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "failed-one", OperationID: "snap-native-failure"}
	if _, err = b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	mode(t, b, "api_failed")
	failed, err := b.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || failed.Status != "native_failed" {
		t.Fatalf("native failure not preserved %+v %v", failed, err)
	}
	again, err := b.Run(ctx, req, false, "")
	if err != nil || again.Status != "native_failed" {
		t.Fatalf("failed operation resubmitted %+v %v", again, err)
	}
}
func TestNativeJournalRejectsSymlinkAndInvalidOwnership(t *testing.T) {
	b := fixture(t)
	dir := filepath.Dir(b.nativeOperationsPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret-free"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, b.nativeOperationsPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.loadNativeOperations(); err == nil {
		t.Fatal("journal symlink followed")
	}
	if err := os.Remove(b.nativeOperationsPath()); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(nativeOperationJournal{Version: 1, Operations: []NativeOperation{{ID: "invalid"}}})
	if err := os.WriteFile(b.nativeOperationsPath(), data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.loadNativeOperations(); err == nil {
		t.Fatal("insecure journal accepted")
	}
}
func TestRestoreNativeETagRequired(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "a", OperationID: "first"}
	if _, err = b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Run(ctx, nativeStatusReq(req), false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Run(ctx, Request{Operation: "stop", Project: "default", Instance: "guest-a", UUID: req.UUID}, false, ""); err != nil {
		t.Fatal(err)
	}
	old := b.apiOverride
	b.apiOverride = func(c context.Context, m, r string, body []byte, etag string) (nativeAPIResponse, error) {
		response, err := old(c, m, r, body, etag)
		if m == "GET" && strings.Contains(r, "/instances/") {
			response.ETag = ""
		}
		return response, err
	}
	restore := Request{Operation: "restore", Project: req.Project, Instance: req.Instance, UUID: req.UUID, Snapshot: req.Snapshot, OperationID: "restore-no-etag"}
	outcome, err := b.Run(ctx, restore, true, "")
	if err == nil || !strings.Contains(err.Error(), "native_restore_etag_unavailable") {
		t.Fatalf("preflight without ETag must be rejected before mutation: %+v %v", outcome, err)
	}
	journal, err := b.loadNativeOperations()
	if err != nil || len(journal.Operations) != 1 {
		t.Fatalf("failed restore preflight left journal entry: %+v %v", journal, err)
	}
}

func TestUncertainNativeOperationRequiresApprovedVerifiedRetirement(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "possible", OperationID: "uncertain-retirement"}
	if _, err = b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	mode(t, b, "api_op_expired")
	if r, err := b.Run(ctx, nativeStatusReq(req), false, ""); err != nil || r.Status != "outcome_uncertain" {
		t.Fatalf("unexpected status: %+v %v", r, err)
	}
	resolve := nativeStatusReq(req)
	resolve.Operation = "resolve_uncertain"
	if _, err = b.Run(ctx, resolve, false, ""); err == nil || !strings.Contains(err.Error(), "approval_required") {
		t.Fatalf("unapproved resolution: %v", err)
	}
	mode(t, b, "pending_operation")
	if _, err = b.Run(ctx, resolve, true, ""); err == nil || !strings.Contains(err.Error(), "active Incus operation") {
		t.Fatalf("outstanding operation retired: %v", err)
	}
	mode(t, b, "api_op_expired")
	closed, err := b.Run(ctx, resolve, true, "")
	if err != nil || closed.Status != "closed_unverified" || closed.Operation == nil || closed.Operation.NativeTerminal != "" {
		t.Fatalf("manual retirement claimed native completion: %+v %v", closed, err)
	}
	duplicate, err := b.Run(ctx, req, false, "")
	if err != nil || duplicate.Status != "closed_unverified" {
		t.Fatalf("retired operation replayed: %+v %v", duplicate, err)
	}
	if _, err = b.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: req.UUID}, false, ""); err != nil {
		t.Fatalf("guest remained permanently blocked after approved retirement: %v", err)
	}
}
func TestVerifiedNativeTerminalSurvivesTransientGuestInventoryFailure(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "restore-me", OperationID: "terminal-record"}
	if _, err = b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	mode(t, b, "list_unavailable")
	uncertain, err := b.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || uncertain.Status != "outcome_uncertain" || uncertain.Operation.NativeTerminal != "success" {
		t.Fatalf("native success evidence lost: %+v %v", uncertain, err)
	}
	// The original native operation is now expired, but retained terminal
	// evidence allows identity reconciliation without a false failure.
	mode(t, b, "api_op_expired")
	recovered, err := b.Run(ctx, nativeStatusReq(req), false, "")
	if err != nil || recovered.Status != "native_succeeded" {
		t.Fatalf("transient identity failure lost native outcome: %+v %v", recovered, err)
	}
}
func TestNonblockingSameIDConcurrentSubmissionSingleNativeIntent(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "concurrent", OperationID: "same-nonblocking-id"}
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := b.Run(ctx, req, false, "")
			if err != nil || r.Operation == nil || r.Operation.ID != req.OperationID {
				errors <- fmt.Errorf("unexpected request %+v %v", r, err)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	journal, err := b.loadNativeOperations()
	if err != nil || len(journal.Operations) != 1 {
		t.Fatalf("concurrent requests duplicated journal: %+v %v", journal, err)
	}
	names, err := b.Snapshots(ctx, req.Project, req.Instance)
	if err != nil || len(names) != 1 || names[0] != req.Snapshot {
		t.Fatalf("native duplicate: %v %v", names, err)
	}
}

func TestDefiniteNativeHTTPRejectionDoesNotBecomeUnrecoverable(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	old := b.apiOverride
	b.apiOverride = func(c context.Context, m, r string, body []byte, etag string) (nativeAPIResponse, error) {
		if m == "POST" && strings.Contains(r, "/snapshots?") {
			return nativeAPIResponse{Status: 412}, fmt.Errorf("incus_api_http_412")
		}
		return old(c, m, r, body, etag)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "definite-reject", OperationID: "snapshot-rejected-etag"}
	r, err := b.Run(ctx, req, false, "")
	if err != nil || r.Status != "native_failed" || r.Operation == nil || r.Operation.NativeID != "" {
		t.Fatalf("definite rejected mutation reported uncertainty: %+v %v", r, err)
	}
	// A failed operation is durable under the original ID and doesn't cause
	// indefinite blocking of unrelated permitted actions.
	repeated, err := b.Run(ctx, req, false, "")
	if err != nil || repeated.Status != "native_failed" {
		t.Fatalf("replayed rejected HTTP response: %+v %v", repeated, err)
	}
	if _, err = b.Run(ctx, Request{Operation: "stop", Project: "default", Instance: "guest-a", UUID: req.UUID}, false, ""); err != nil {
		t.Fatalf("definite rejection blocked guest: %v", err)
	}
}
func TestNativeStatusRejectsMismatchedProviderIdentity(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "bound", OperationID: "identity-bound"}
	if _, err = b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	mismatched := nativeStatusReq(req)
	mismatched.UUID = "99887766-1234-1234-1234-123456789abc"
	if _, err = b.Run(ctx, mismatched, false, ""); err == nil || !strings.Contains(err.Error(), "native_operation_identity_mismatch") {
		t.Fatalf("wrong guest identity exposed native operation: %v", err)
	}
	repeated := req
	repeated.Snapshot = "other"
	if _, err = b.Run(ctx, repeated, false, ""); err == nil || !strings.Contains(err.Error(), "native_operation_id_collision") {
		t.Fatalf("same operation id authorized changed snapshot: %v", err)
	}
}

func TestColdNewChatRediscoversNativeOperationWithoutPriorID(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "cold-chat", OperationID: "cold-chat-durable-id"}
	if _, err = b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	cold := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe, apiOverride: b.apiOverride}
	all, err := cold.Run(ctx, Request{Operation: "operation_list", Project: "default"}, false, "")
	if err != nil || len(all.Operations) != 1 || all.Operations[0].ID != "cold-chat-durable-id" || all.Operations[0].UUID != req.UUID {
		t.Fatalf("cold chat cannot rediscover operation by project: %+v %v", all, err)
	}
	none, err := cold.Run(ctx, Request{Operation: "operation_list", Project: "other-project"}, false, "")
	if err != nil || len(none.Operations) != 0 {
		t.Fatalf("project filter leaked operation metadata: %+v %v", none, err)
	}
	running, err := cold.Run(ctx, Request{Operation: "operation_status", Project: all.Operations[0].Project, Instance: all.Operations[0].Instance, UUID: all.Operations[0].UUID, OperationID: all.Operations[0].ID}, false, "")
	if err != nil || running.Status != "native_succeeded" {
		t.Fatalf("recovered status after cold discovery: %+v %v", running, err)
	}
}

func TestNativeOperationsAreRecoverableOnlyByOriginalPrincipal(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "principal-bound", OperationID: "principal-identity", ActorID: "direct-a"}
	accepted, err := b.Run(ctx, req, false, "")
	if err != nil || accepted.Status != "running" {
		t.Fatalf("submit %+v %v", accepted, err)
	}
	wrong := req
	wrong.ActorID = "direct-b"
	if _, err = b.Run(ctx, wrong, false, ""); err == nil || !strings.Contains(err.Error(), "native_operation_id_collision") {
		t.Fatalf("other principal replay accepted: %v", err)
	}
	otherStatus := nativeStatusReq(req)
	otherStatus.ActorID = "direct-b"
	if _, err = b.Run(ctx, otherStatus, false, ""); err == nil || !strings.Contains(err.Error(), "native_operation_identity_mismatch") {
		t.Fatalf("other principal status accepted: %v", err)
	}
	entries, err := b.Run(ctx, Request{Operation: "operation_list", Project: "default", ActorID: "direct-b"}, false, "")
	if err != nil || len(entries.Operations) != 0 {
		t.Fatalf("other principal discovered operation: %+v %v", entries, err)
	}
	owned, err := b.Run(ctx, Request{Operation: "operation_list", Project: "default", ActorID: "direct-a"}, false, "")
	if err != nil || len(owned.Operations) != 1 {
		t.Fatalf("owner cannot discover operation: %+v %v", owned, err)
	}
	correct := nativeStatusReq(req)
	correct.ActorID = req.ActorID
	complete, err := b.Run(ctx, correct, false, "")
	if err != nil || complete.Status != "native_succeeded" {
		t.Fatalf("owner cannot complete: %+v %v", complete, err)
	}
}

func TestExecutorStartupRecoveryCandidatesWithoutSpuriousState(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	candidates, err := b.RecoverableNativeOperations()
	if err != nil || len(candidates) != 0 {
		t.Fatalf("missing journal must be optional: %v %+v", err, candidates)
	}
	if _, err := os.Lstat(filepath.Dir(b.nativeOperationsPath())); !os.IsNotExist(err) {
		t.Fatalf("passive recovery created an Incus state directory: %v", err)
	}
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, OperationID: "restart-recover-id", Snapshot: "restart-probe", ActorID: "direct-recovery"}
	mode(t, b, "api_running")
	if _, err := b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	cold := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe, apiOverride: b.apiOverride}
	candidates, err = cold.RecoverableNativeOperations()
	if err != nil || len(candidates) != 1 || candidates[0].ID != req.OperationID || candidates[0].ActorID != req.ActorID {
		t.Fatalf("startup cannot discover pending identity: %+v %v", candidates, err)
	}
	mode(t, b, "")
	poll, stop := context.WithTimeout(context.Background(), 4*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() { cold.WatchNativeOperation(poll, nativeStatusReqWithActor(req)); close(done) }()
	select {
	case <-done:
	case <-poll.Done():
		t.Fatal("restarted observer failed to persist terminal native operation")
	}
	recovered, err := cold.Run(ctx, nativeStatusReqWithActor(req), false, "")
	if err != nil || recovered.Status != "native_succeeded" {
		t.Fatalf("startup observer lost terminal proof: %+v %v", recovered, err)
	}
	candidates, err = cold.RecoverableNativeOperations()
	if err != nil || len(candidates) != 0 {
		t.Fatalf("terminal operation still scheduled after recovery: %+v %v", candidates, err)
	}
}
func nativeStatusReqWithActor(req Request) Request {
	status := nativeStatusReq(req)
	status.ActorID = req.ActorID
	return status
}
