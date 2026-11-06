package managedincus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func candidateOperation(project, instance, snapshot, action, id string, created time.Time, status int) map[string]any {
	description := "Snapshotting instance"
	if action == "restore" {
		description = "Restoring snapshot"
	}
	base := "/1.0/instances/" + instance
	return map[string]any{
		"id": id, "description": description, "created_at": created,
		"status_code": status,
		"resources": map[string][]string{
			"instances":           {base + "?project=" + project},
			"instances_snapshots": {base + "/snapshots/" + snapshot + "?project=" + project},
		},
	}
}

func listNativeOperations(rows ...map[string]any) []byte {
	out, _ := json.Marshal(map[string]any{"type": "sync", "metadata": map[string]any{"running": rows}})
	return out
}

func TestLostAcknowledgementMatchesFullNativeOperationIdentity(t *testing.T) {
	now := time.Now().UTC()
	const ownID = "e5391721-c6db-45a2-a340-378e9e420da1"
	const otherID = "e5391721-c6db-45a2-a340-378e9e420da2"
	own := func(action string) map[string]any {
		return candidateOperation("asa-issue65", "guest-a", "same-name", action, ownID, now, 103)
	}
	testCases := []struct {
		name      string
		action    string
		rows      []map[string]any
		wantID    string
		wantState string
	}{
		{"matching-snapshot", "snapshot", []map[string]any{own("snapshot")}, ownID, "running"},
		{"two-guests-same-snapshot-name", "snapshot", []map[string]any{candidateOperation("asa-issue65", "guest-b", "same-name", "snapshot", otherID, now, 103)}, "", "outcome_uncertain"},
		{"cross-project-snapshot-name", "snapshot", []map[string]any{candidateOperation("another-project", "guest-a", "same-name", "snapshot", otherID, now, 103)}, "", "outcome_uncertain"},
		{"ambiguous-multiple-native-resources", "snapshot", []map[string]any{func() map[string]any {
			x := own("snapshot")
			resources := x["resources"].(map[string][]string)
			resources["instances"] = append(resources["instances"], "/1.0/instances/guest-b?project=asa-issue65")
			return x
		}()}, "", "outcome_uncertain"},
		{"restore-offered-as-snapshot", "snapshot", []map[string]any{own("restore")}, "", "outcome_uncertain"},
		{"snapshot-offered-as-restore", "restore", []map[string]any{own("snapshot")}, "", "outcome_uncertain"},
		{"matching-restore", "restore", []map[string]any{own("restore")}, ownID, "running"},
		{"incus-604-restore-without-snapshot-resource", "restore", []map[string]any{func() map[string]any {
			x := own("restore")
			x["resources"] = map[string][]string{"instances": {"/1.0/instances/guest-a?project=asa-issue65"}}
			return x
		}()}, "", "outcome_uncertain"},
		{"early-visible-name-without-target-resource", "snapshot", []map[string]any{func() map[string]any {
			x := own("snapshot")
			x["resources"] = map[string][]string{"instances": {"/1.0/instances/guest-a?project=asa-issue65"}}
			x["description"] = "Snapshotting instance same-name"
			return x
		}()}, "", "outcome_uncertain"},
		{"preexisting-operation-before-durable-intent", "snapshot", []map[string]any{candidateOperation("asa-issue65", "guest-a", "same-name", "snapshot", otherID, now.Add(-time.Hour), 103)}, "", "outcome_uncertain"},
		{"two-identical-target-operations-ambiguous", "snapshot", []map[string]any{own("snapshot"), candidateOperation("asa-issue65", "guest-a", "same-name", "snapshot", otherID, now, 103)}, "", "outcome_uncertain"},
		{"unrelated-concurrent-native-operation", "snapshot", []map[string]any{candidateOperation("asa-issue65", "guest-b", "same-name", "snapshot", otherID, now, 103), own("snapshot")}, ownID, "running"},
		{"verified-native-terminal-from-short-lived-list", "snapshot", []map[string]any{candidateOperation("asa-issue65", "guest-a", "same-name", "snapshot", ownID, now, 200)}, ownID, "native_succeeded"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixture(t)
			b.apiOverride = func(_ context.Context, method, route string, _ []byte, _ string) (nativeAPIResponse, error) {
				if method != http.MethodGet || !strings.Contains(route, "/operations?") {
					return nativeAPIResponse{}, fmt.Errorf("unexpected request: %s %s", method, route)
				}
				return nativeAPIResponse{Status: 200, Body: listNativeOperations(tc.rows...)}, nil
			}
			intent := NativeOperation{
				HostID: b.HostID, ID: "current-intent", Project: "asa-issue65",
				Instance: "guest-a", UUID: "c8922954-19cd-4cbe-974c-adba463cd59a",
				Action: tc.action, Snapshot: "same-name", State: "outcome_uncertain",
				SubmittedAt: now.Add(-time.Second), UpdatedAt: now,
			}
			b.reconcileLostAck(context.Background(), &intent)
			if intent.NativeID != tc.wantID || intent.State != tc.wantState {
				t.Fatalf("unsafe reattachment: native_id=%q state=%q expected %q/%q", intent.NativeID, intent.State, tc.wantID, tc.wantState)
			}
		})
	}
}

func TestLostNativeAcknowledgementIsObservedAndRetainedAfterClientDisconnect(t *testing.T) {
	for _, action := range []string{"snapshot", "restore"} {
		t.Run(action, func(t *testing.T) {
			b := fixture(t)
			ctx := context.Background()
			created, err := b.Run(ctx, createRequest(), false, "")
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Operation: action, Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "tested", OperationID: "lost-" + action}
			if action == "restore" {
				initial := req
				initial.Operation = "snapshot"
				initial.OperationID = "first-snapshot"
				if _, err := b.Run(ctx, initial, false, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := b.Run(ctx, nativeStatusReq(initial), false, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := b.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: req.UUID}, false, ""); err != nil {
					t.Fatal(err)
				}
			}
			original := b.apiOverride
			submitted := time.Time{}
			mutations := 0
			b.apiOverride = func(c context.Context, method, route string, payload []byte, etag string) (nativeAPIResponse, error) {
				if method == http.MethodGet && strings.Contains(route, "/operations?") {
					if submitted.IsZero() {
						return nativeAPIResponse{}, errors.New("native operation not yet created")
					}
					op := candidateOperation("default", req.Instance, req.Snapshot, action, "a40f5541-5e98-454f-b3b6-8a51ef5dbd3c", submitted, 200)
					if action == "restore" {
						// Observed on real Incus 6.0.4: restore operations
						// expose "instances" only, never the snapshot path.
						delete(op["resources"].(map[string][]string), "instances_snapshots")
					}
					return nativeAPIResponse{Status: 200, Body: listNativeOperations(op)}, nil
				}
				if (action == "snapshot" && method == http.MethodPost && strings.Contains(route, "/snapshots?")) ||
					(action == "restore" && method == http.MethodPut && strings.Contains(route, "/instances/")) {
					mutations++
					_, err := original(c, method, route, payload, etag)
					if err != nil {
						return nativeAPIResponse{}, err
					}
					submitted = time.Now().UTC()
					return nativeAPIResponse{}, errors.New("HTTP 202 accepted by Incus, acknowledgement lost")
				}
				return original(c, method, route, payload, etag)
			}
			result, err := b.Run(ctx, req, action == "restore", "")
			if err != nil || result.Status != "outcome_uncertain" || result.Operation == nil || result.Operation.NativeID != "" {
				t.Fatalf("lost ack incorrectly considered complete: %+v, %v", result, err)
			}
			// Simulate a disconnected client and a new Executor with only its
			// durable journal. The observer is independent of the request.
			cold := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe, apiOverride: b.apiOverride}
			timeout := 3 * time.Second
			if action == "restore" {
				timeout = 150 * time.Millisecond
			}
			observe, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			verified := cold.WatchNativeOperation(observe, nativeStatusReq(req))
			if verified != (action == "snapshot") {
				t.Fatalf("observer claimed invalid native proof for %s: verified=%v", action, verified)
			}
			mode(t, b, "api_op_expired")
			terminal, err := cold.Run(ctx, nativeStatusReq(req), false, "")
			if err != nil || mutations != 1 || terminal.Operation == nil {
				t.Fatalf("lost ack/expiry incorrectly reconciled: %+v err=%v submissions=%d", terminal, err, mutations)
			}
			if action == "snapshot" {
				if terminal.Status != "native_succeeded" || terminal.Operation.NativeID == "" {
					t.Fatalf("strict snapshot resource proof not recovered: %+v", terminal)
				}
			} else {
				if terminal.Status != "outcome_uncertain" || terminal.Operation.NativeID != "" {
					t.Fatalf("Incus restore without snapshot resource falsely proven: %+v", terminal)
				}
				repeat, err := cold.Run(ctx, req, true, "")
				if err != nil || repeat.Status != "outcome_uncertain" || mutations != 1 {
					t.Fatalf("uncertain restore retried: %+v err=%v submissions=%d", repeat, err, mutations)
				}
			}
		})
	}
}

func TestConcurrentRestoreConfigChangeRefusesStaleETag(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "etag-target", OperationID: "etag-snap"}
	if _, err = b.Run(ctx, snapshot, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Run(ctx, nativeStatusReq(snapshot), false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Run(ctx, Request{Operation: "stop", Project: "default", Instance: "guest-a", UUID: snapshot.UUID}, false, ""); err != nil {
		t.Fatal(err)
	}
	original := b.apiOverride
	puts := 0
	b.apiOverride = func(c context.Context, method, route string, payload []byte, etag string) (nativeAPIResponse, error) {
		if method == http.MethodPut && strings.Contains(route, "/instances/") {
			puts++
			if etag != "fixture-etag" {
				t.Fatalf("restore lost its native ETag precondition: %q", etag)
			}
			// A different native actor updated instance configuration after
			// the GET and before this conditional PUT.
			return nativeAPIResponse{Status: http.StatusPreconditionFailed}, errors.New("incus_api_http_412: stale If-Match")
		}
		return original(c, method, route, payload, etag)
	}
	restore := Request{Operation: "restore", Project: "default", Instance: "guest-a", UUID: snapshot.UUID, Snapshot: snapshot.Snapshot, OperationID: "stale-config"}
	got, err := b.Run(ctx, restore, true, "")
	if err != nil || got.Status != "native_failed" || got.Operation == nil || got.Operation.NativeID != "" || puts != 1 {
		t.Fatalf("stale ETag was allowed to restore: %+v err=%v puts=%d", got, err, puts)
	}
	repeat, err := b.Run(ctx, restore, true, "")
	if err != nil || repeat.Status != "native_failed" || puts != 1 {
		t.Fatalf("stale ETag replay submitted mutation: %+v err=%v puts=%d", repeat, err, puts)
	}
}
