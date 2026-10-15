package executor

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// js-debug may send startDebugging before the parent configurationDone reply.
// That legal ordering must not be rejected as an arbitrary reverse command.
func TestNodeEarlyTypedChildDuringParentConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name             string
		browser          bool
		token            string
		adapterType      string
		configReplyFirst bool
	}{
		{"browser_launch_reply_first", true, "9FFA7743642C2F40E6F77F4A4E89E83C", "pwa-chrome", false},
		{"browser_config_reply_first", true, "9FFA7743642C2F40E6F77F4A4E89E83C", "pwa-chrome", true},
		{"node_launch_reply_first", false, "9ffa7743642c2f40e6f77f4a4e89e83c", "pwa-node", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			d := &dapState{conn: client, adapter: "javascript/js-debug", browserMode: tc.browser,
				initialized: true, pendingLaunchSeq: 42, stage: "starting"}
			done := make(chan error, 1)
			go func() {
				defer close(done)
				_ = server.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(server)
				request, err := dapFixtureRead(reader)
				if err != nil {
					done <- err
					return
				}
				if request.Command != "configurationDone" {
					done <- fmt.Errorf("unexpected parent operation: %s", request.Command)
					return
				}
				if err = dapFrame(server, map[string]any{"seq": 99, "type": "request",
					"command": "startDebugging", "arguments": map[string]any{
						"request":       "launch",
						"configuration": map[string]any{"type": tc.adapterType, "__pendingTargetId": tc.token},
					}}); err != nil {
					done <- err
					return
				}
				ack, err := dapFixtureRead(reader)
				if err != nil {
					done <- err
					return
				}
				if ack.Type != "response" || ack.Command != "startDebugging" ||
					ack.RequestSeq != 99 || !ack.Success {
					done <- fmt.Errorf("typed child reverse not accepted: %+v", ack)
					return
				}
				launch := map[string]any{"seq": 101, "type": "response", "request_seq": 42,
					"command": "launch", "success": true}
				config := map[string]any{"seq": 102, "type": "response",
					"request_seq": request.Seq, "command": "configurationDone", "success": true}
				if tc.configReplyFirst {
					launch, config = config, launch
				}
				if err = dapFrame(server, launch); err != nil {
					done <- err
					return
				}
				done <- dapFrame(server, config)
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if _, err := d.call(ctx, "configurationDone", map[string]any{}); err != nil {
				t.Fatalf("valid early typed reverse not reconciled: %v", err)
			}
			if d.pendingLaunchSeq != 0 || !d.nodeChildAccepted {
				t.Fatalf("parent launch/child provenance incomplete: pending=%d child=%v", d.pendingLaunchSeq, d.nodeChildAccepted)
			}
			token, err := d.receiveTypedNodeChild(ctx)
			if err != nil || token != tc.token {
				t.Fatalf("pre-config reverse token lost: %s %v", token, err)
			}
			if second, err := d.receiveTypedNodeChild(ctx); err == nil || second != "" {
				t.Fatalf("replayed child accepted: %q %v", second, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Only js-debug's typed local child target is allowed. Extra attach data,
// ports, remote URLs, and a second reverse request must fail closed even
// when delivered during configurationDone instead of later.
func TestNodeEarlyUntrustedReverseFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments map[string]any
	}{
		{"browser_remote_port", map[string]any{"request": "launch", "configuration": map[string]any{
			"type": "pwa-chrome", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C", "port": 9222}}},
		{"browser_wrong_language", map[string]any{"request": "launch", "configuration": map[string]any{
			"type": "pwa-node", "__pendingTargetId": "9FFA7743642C2F40E6F77F4A4E89E83C"}}},
		{"browser_runInTerminal", map[string]any{"request": "launch", "configuration": map[string]any{
			"type": "pwa-chrome", "__pendingTargetId": "x$()"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			d := &dapState{conn: client, adapter: "javascript/js-debug", browserMode: true, initialized: true}
			done := make(chan error, 1)
			go func() {
				defer close(done)
				_ = server.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(server)
				req, err := dapFixtureRead(reader)
				if err != nil {
					done <- err
					return
				}
				if req.Command != "configurationDone" {
					done <- fmt.Errorf("wrong parent command: %v", req.Command)
					return
				}
				done <- dapFrame(server, map[string]any{"seq": 41, "type": "request",
					"command": "startDebugging", "arguments": tc.arguments})
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err := d.call(ctx, "configurationDone", map[string]any{})
			if err == nil || !strings.Contains(err.Error(), "node_dap_reverse_unverified") ||
				d.queuedNodeTargetID != "" || !d.uncertain {
				t.Fatalf("unsafe early reverse not rejected: error=%v state=%+v", err, d)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
