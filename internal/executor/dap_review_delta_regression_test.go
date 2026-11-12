package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// A passive status poll may consume a completed deferred launch response.
// Size alone is not unknown completion and must not poison the active session.
func TestDAPDeferredOversizePassiveDrainKeepsSessionHealthy(t *testing.T) {
	for _, afterExit := range []bool{false, true} {
		name := "live"
		if afterExit {
			name = "post-exit"
		}
		t.Run(name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			d := &dapState{conn: client, stage: "starting", pendingLaunchSeq: 12}
			done := make(chan error, 1)
			go func() {
				err := dapFrame(peer, map[string]any{
					"seq": 20, "type": "response", "request_seq": 12, "command": "launch",
					"success": true, "body": map[string]any{"result": strings.Repeat("x", 132<<10)},
				})
				if err != nil {
					done <- err
					return
				}
				if afterExit {
					done <- peer.Close()
					return
				}
				req, err := dapFixtureRead(bufio.NewReader(peer))
				if err != nil {
					done <- err
					return
				}
				if req.Command != "threads" {
					done <- fmt.Errorf("wrong followup: %s", req.Command)
					return
				}
				done <- dapFrame(peer, map[string]any{
					"seq": 21, "type": "response", "request_seq": req.Seq, "command": req.Command,
					"success": true, "body": map[string]any{"threads": []any{}},
				})
			}()
			if afterExit {
				if !d.drainAfterExit(context.Background()) {
					t.Fatal("post-exit scan did not reach EOF")
				}
				if !d.passiveClosed {
					t.Fatal("post-exit stream not marked closed")
				}
			} else {
				d.drain(context.Background())
			}
			if d.uncertain || d.pendingLaunchSeq != 0 || d.stage != "starting" {
				t.Fatalf("complete deferred reply poisoned session: uncertain=%v pending=%d stage=%s", d.uncertain, d.pendingLaunchSeq, d.stage)
			}
			if !afterExit {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				body, err := d.call(ctx, "threads", map[string]any{})
				if err != nil || string(body) != "{\"threads\":[]}" {
					t.Fatalf("live session could not complete narrower request: %s %v", body, err)
				}
			} else if _, err := d.call(context.Background(), "threads", nil); err == nil || !strings.Contains(err.Error(), "dap_state_uncertain") {
				t.Fatalf("dead adapter wrongly considered runnable: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDAPDeferredPassiveDrainStillFailsClosedOnUnknown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requestSeq int
		command    string
		success    bool
		partial    bool
	}{
		{name: "wrong-sequence", requestSeq: 13, command: "launch", success: true},
		{name: "wrong-command", requestSeq: 12, command: "initialize", success: true},
		{name: "failed-launch", requestSeq: 12, command: "launch", success: false},
		{name: "partial-transport", partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			d := &dapState{conn: client, stage: "starting", pendingLaunchSeq: 12}
			done := make(chan error, 1)
			go func() {
				if tc.partial {
					_, err := peer.Write([]byte("Content-Length: 256\r\n\r\n{\"type\":\"response\"}"))
					if err != nil {
						done <- err
						return
					}
				} else if err := dapFrame(peer, map[string]any{
					"seq": 20, "type": "response", "request_seq": tc.requestSeq,
					"command": tc.command, "success": tc.success, "body": map[string]any{},
				}); err != nil {
					done <- err
					return
				}
				done <- peer.Close()
			}()
			d.drainAfterExit(context.Background())
			if !d.uncertain || d.stage != "uncertain" {
				t.Fatalf("unknown deferred outcome did not fail closed: uncertain=%v stage=%s", d.uncertain, d.stage)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDAPSuccessfulOversizeInitializeClassifiesAsResource(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	d := &dapState{conn: client, stage: "starting"}
	done := make(chan error, 1)
	go func() {
		req, err := dapFixtureRead(bufio.NewReader(peer))
		if err != nil {
			done <- err
			return
		}
		if req.Command != "initialize" {
			done <- fmt.Errorf("wrong startup request: %s", req.Command)
			return
		}
		done <- dapFrame(peer, map[string]any{
			"seq": 20, "type": "response", "request_seq": req.Seq, "command": req.Command, "success": true,
			"body": map[string]any{"capabilities": strings.Repeat("x", 132<<10)},
		})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := d.call(ctx, "initialize", map[string]any{"adapterID": "go"})
	if !errors.Is(err, errDAPResultTooLarge) || d.uncertain {
		t.Fatalf("complete oversized initialize not known: %v uncertain=%v", err, d.uncertain)
	}
	r := debugDAPOperationFailure(err, "debug_initialize_failed", "runtime")
	if r.OK || r.ErrorCode != "debug_result_too_large" || r.ErrorClass != "resource" || !strings.Contains(r.Error, "131072") {
		t.Fatalf("startup size wrongly classified: %+v", r)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDAPNodeDeferredOversizeClassificationAndSecurity(t *testing.T) {
	// An oversized, correctly identified parent launch response can arrive
	// while configurationDone is in flight. Its known size error is not a
	// reverse-request provenance violation.
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	d := &dapState{conn: client, adapter: "javascript/js-debug", stage: "starting", pendingLaunchSeq: 12, initialized: true}
	done := make(chan error, 1)
	go func() {
		req, err := dapFixtureRead(bufio.NewReader(peer))
		if err != nil {
			done <- err
			return
		}
		if req.Command != "configurationDone" {
			done <- fmt.Errorf("unexpected Node parent method: %s", req.Command)
			return
		}
		done <- dapFrame(peer, map[string]any{
			"seq": 20, "type": "response", "request_seq": 12, "command": "launch", "success": true,
			"body": map[string]any{"result": strings.Repeat("x", 132<<10)},
		})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := d.call(ctx, "configurationDone", map[string]any{})
	if !errors.Is(err, errDAPResultTooLarge) || d.uncertain || d.pendingLaunchSeq != 0 {
		t.Fatalf("known Node launch completion lost: %v uncertain=%v pending=%d", err, d.uncertain, d.pendingLaunchSeq)
	}
	for _, tc := range []struct{ code, class string }{
		{"javascript_dap_parent_configure_failed", "runtime"},
		{"javascript_dap_child_reverse_rejected", "security"},
	} {
		r := debugDAPOperationFailure(err, tc.code, tc.class)
		if r.ErrorCode != "debug_result_too_large" || r.ErrorClass != "resource" {
			t.Fatalf("Node size reported as %s/%s: %+v", tc.code, tc.class, r)
		}
	}
	native := errors.New("node_dap_reverse_wrong_adapter_or_repeated_child")
	r := debugDAPOperationFailure(native, "javascript_dap_child_reverse_rejected", "security")
	if r.ErrorCode != "javascript_dap_child_reverse_rejected" || r.ErrorClass != "security" {
		t.Fatalf("real Node identity failure downgraded: %+v", r)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDAPStartupErrorMappingsKeepNativeFailures(t *testing.T) {
	for _, tc := range []struct{ name, code, class string }{
		{"Go initialize", "debug_initialize_failed", "runtime"},
		{"Go launch", "debug_launch_failed", "runtime"},
		{"Python initialize", "python_dap_initialize_failed", "runtime"},
		{"PHP launch", "php_dap_launch_failed", "runtime"},
		{"Rust initialize", "rust_dap_initialize_failed", "runtime"},
		{"Shell launch", "shell_dap_launch_failed", "runtime"},
		{"Node parent", "javascript_dap_parent_launch_failed", "runtime"},
		{"Node child", "javascript_dap_child_initialize_failed", "runtime"},
		{"Node reverse", "javascript_dap_child_reverse_rejected", "security"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{errDAPResultTooLarge, fmt.Errorf("native %s failure", tc.name)} {
				r := debugDAPOperationFailure(err, tc.code, tc.class)
				if errors.Is(err, errDAPResultTooLarge) {
					if r.ErrorCode != "debug_result_too_large" || r.ErrorClass != "resource" {
						t.Fatalf("size mapping: %+v", r)
					}
				} else if r.ErrorCode != tc.code || r.ErrorClass != tc.class || r.Error != err.Error() {
					t.Fatalf("native mapping: %+v", r)
				}
			}
		})
	}
}

func TestDAPNodeReverseInvalidRequestStaysSecurityFailure(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	d := &dapState{conn: client, adapter: "javascript/js-debug", stage: "starting"}
	done := make(chan error, 1)
	go func() {
		done <- dapFrame(peer, map[string]any{
			"seq": 7, "type": "request", "command": "startDebugging",
			"arguments": map[string]any{"request": "attach", "configuration": map[string]any{"type": "pwa-node", "__pendingTargetId": "untrusted"}},
		})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := d.receiveTypedNodeChild(ctx)
	if err == nil || errors.Is(err, errDAPResultTooLarge) {
		t.Fatalf("unsafe Node reverse request accepted or misclassified: %v", err)
	}
	r := debugDAPOperationFailure(err, "javascript_dap_child_reverse_rejected", "security")
	if r.ErrorClass != "security" || r.ErrorCode != "javascript_dap_child_reverse_rejected" {
		t.Fatalf("Node trust failure diluted: %+v", r)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The contract maps refusals through the existing MCP envelope. A completed
// successful response too large to present must never become complete output.
func TestDAPSizeFailureResponseIsNotSuccess(t *testing.T) {
	r := debugDAPOperationFailure(fmt.Errorf("%w: %d byte reply", errDAPResultTooLarge, dapMaxReply+1), "debug_initialize_failed", "runtime")
	if r.OK || r.Output != "" || r.ErrorCode != "debug_result_too_large" || r.ErrorClass != "resource" {
		t.Fatalf("size refused but advertised success: %+v", r)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
}
