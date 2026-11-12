package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// A broker event is not an LSP frame boundary. In particular, the last
// chunk of a 256 KiB frame can also contain the start of the next frame.
func TestLSPFullFrameThenNextValidNotification(t *testing.T) {
	prefix := `{"jsonrpc":"2.0","method":"window/logMessage","params":{"message":"`
	suffix := `"}}`
	first := []byte(prefix + strings.Repeat("a", lspMaxFrameBytes-len(prefix)-len(suffix)) + suffix)
	if len(first) != lspMaxFrameBytes || !json.Valid(first) {
		t.Fatal("invalid exact-limit LSP fixture")
	}
	second := []byte(prefix + strings.Repeat("b", 4<<10) + suffix)
	framed := func(body []byte) []byte {
		return append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...)
	}
	for _, split := range []bool{false, true} {
		name := "one-burst"
		if split {
			name = "split-body"
		}
		t.Run(name, func(t *testing.T) {
			s, owner, _ := testStdioBroker(t)
			entry := &stdioSession{id: "review-lsp", kind: "lsp", ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass, workspace: owner.Workspace, done: make(chan struct{})}
			if err := s.sessions.reserve(entry); err != nil {
				t.Fatal(err)
			}
			all := append(framed(first), framed(second)...)
			writer := sessionEventWriter{entry: entry, stream: "stdout"}
			w := newLSPWire(s, owner, entry.id)
			if split {
				cut := len(first) / 2
				if _, err := writer.Write(all[:cut]); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
				_, err := w.next(ctx)
				cancel()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("partial frame was not retained: %v", err)
				}
				all = all[cut:]
			}
			if _, err := writer.Write(all); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for i, want := range []string{strings.Repeat("a", len(first)-len(prefix)-len(suffix)), strings.Repeat("b", 4<<10)} {
				packet, err := w.next(ctx)
				if err != nil {
					t.Fatalf("valid LSP frame %d refused: %v", i, err)
				}
				if packet.Method != "window/logMessage" {
					t.Fatalf("wrong frame method %d: %s", i, packet.Method)
				}
				var value struct {
					Message string `json:"message"`
				}
				if err := json.Unmarshal(packet.Params, &value); err != nil || value.Message != want {
					t.Fatalf("LSP frame %d was altered: %v", i, err)
				}
			}
		})
	}
	if _, _, err := readLSPFrame([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", lspMaxFrameBytes+1))); err == nil {
		t.Fatal("oversize individual frame accepted")
	}
}

func TestLSPBrokerRetentionLossStillExplicit(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	entry := &stdioSession{id: "review-loss", kind: "lsp", ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass, workspace: owner.Workspace, done: make(chan struct{})}
	if err := s.sessions.reserve(entry); err != nil {
		t.Fatal(err)
	}
	if _, err := (sessionEventWriter{entry: entry, stream: "stdout"}).Write(bytes.Repeat([]byte("x"), largeStdioOutputBytes+maxStdioEventBytes)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := newLSPWire(s, owner, entry.id).next(ctx); err == nil || !strings.Contains(err.Error(), "lsp_output_retention_lost") {
		t.Fatalf("broker overwrite was not reported: %v", err)
	}
}

func TestDAPCompleteOversizeReplyKeepsSessionUsable(t *testing.T) {
	client, adapter := net.Pipe()
	defer client.Close()
	defer adapter.Close()
	d := &dapState{conn: client, stage: "stopped"}
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapter)
		first, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		err = dapFrame(adapter, map[string]any{"seq": 21, "type": "response", "request_seq": first.Seq, "command": first.Command, "success": true, "body": map[string]any{"value": strings.Repeat("x", 132<<10)}})
		if err != nil {
			done <- err
			return
		}
		second, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		done <- dapFrame(adapter, map[string]any{"seq": 22, "type": "response", "request_seq": second.Seq, "command": second.Command, "success": true, "body": map[string]any{"value": "ok"}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.call(ctx, "variables", map[string]any{"variablesReference": 7}); !errors.Is(err, errDAPResultTooLarge) {
		t.Fatalf("wrong known oversize refusal: %v", err)
	}
	if d.uncertain || d.stage != "stopped" {
		t.Fatalf("complete reply poisoned DAP: uncertain=%v stage=%s", d.uncertain, d.stage)
	}
	raw, err := d.call(ctx, "variables", map[string]any{"variablesReference": 7, "count": 1})
	if err != nil || !bytes.Contains(raw, []byte(`"ok"`)) {
		t.Fatalf("narrow request did not recover: %s %v", raw, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPublicSemanticAndDAPErrorClassifications(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		code, class string
	}{
		{"lsp raw overflow", fmt.Errorf("%w: %d exceeds %d; narrow references", errLSPResultTooLarge, codeMaxResultBytes+1, codeMaxResultBytes), "lsp_result_too_large", "resource"},
		{"lsp normalized overflow", fmt.Errorf("%w: %d exceeds %d; narrow references", errCodeResultTooLarge, codeMaxResultBytes+1, codeMaxResultBytes), "code_result_too_large", "resource"},
		{"lsp malformed", errLSPInvalidResult, "lsp_invalid_result", "data"},
		{"transport fault", errors.New("lsp transport closed"), "code_inspection_failed", "runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := codeInspectionFailure(tc.err)
			if r.OK || r.ErrorCode != tc.code || r.ErrorClass != tc.class || r.Error == "" {
				t.Fatalf("incorrect semantic public error: %+v", r)
			}
		})
	}
	raw := json.RawMessage(`{"locations":[{"uri":"file:///workspace/main.go","details":"` + strings.Repeat("x", 132<<10) + `"}]}`)
	_, err := normalizeCodeLocations("/workspace", raw)
	if !errors.Is(err, errCodeResultTooLarge) {
		t.Fatalf("normalized LSP overflow missing typed cause: %v", err)
	}
	classified := codeInspectionFailure(err)
	if classified.ErrorCode != "code_result_too_large" || classified.ErrorClass != "resource" || !strings.Contains(classified.Error, "131072") {
		t.Fatalf("normalized semantic limit hidden: %+v", classified)
	}
	for _, tc := range []struct {
		name        string
		input       json.RawMessage
		ok          bool
		code, class string
	}{
		{"valid near budget", json.RawMessage(`{"value":"` + strings.Repeat("z", dapMaxReply-250) + `"}`), true, "", ""},
		{"normalized overflow", json.RawMessage(`{"value":"` + strings.Repeat("&", 23<<10) + `"}`), false, "debug_result_too_large", "resource"},
		{"malformed JSON", json.RawMessage(`{"broken":`), false, "debug_result_invalid", "data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &dapState{workspace: "/workspace", adapter: "go/delve", stage: "stopped"}
			r := (&Server{}).debugResult(Request{SessionID: "session"}, d, tc.input, time.Now())
			if r.OK != tc.ok || r.ErrorCode != tc.code || r.ErrorClass != tc.class {
				t.Fatalf("incorrect DAP public response: %+v", r)
			}
			if !r.OK && !strings.Contains(r.Error, "131072") && tc.code == "debug_result_too_large" {
				t.Fatalf("missing DAP budget: %+v", r)
			}
		})
	}
	for _, tc := range []struct {
		name        string
		err         error
		code, class string
	}{
		{"known size refusal", errDAPResultTooLarge, "debug_result_too_large", "resource"},
		{"unknown transport", errors.New("dap_transport_unknown"), "debug_action_uncertain", "runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := debugCallFailure(tc.err)
			if r.ErrorCode != tc.code || r.ErrorClass != tc.class {
				t.Fatalf("wrong DAP action classification: %+v", r)
			}
		})
	}
}

func TestPublicMalformedNormalizedSemanticResult(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`{"locations":`), json.RawMessage(`{"locations":[{"uri":123}]}`)} {
		_, err := normalizeCodeLocations("/workspace", raw)
		if !errors.Is(err, errLSPInvalidResult) {
			t.Fatalf("malformed normalized LSP result lost typed cause: %v", err)
		}
		r := codeInspectionFailure(err)
		if r.ErrorCode != "lsp_invalid_result" || r.ErrorClass != "data" {
			t.Fatalf("malformed LSP result conflated with resource/runtime failure: %+v", r)
		}
	}
}

func TestDAPDeferredKnownOversizeReplyNotUncertain(t *testing.T) {
	d := &dapState{stage: "starting", pendingLaunchSeq: 12}
	matched, err := d.acceptDeferredLaunchReply(dapPacket{Type: "response", RequestSeq: 12, Command: "launch", Success: true, Body: json.RawMessage(`{"result":"` + strings.Repeat("x", 132<<10) + `"}`)})
	if !matched || !errors.Is(err, errDAPResultTooLarge) || d.uncertain || d.pendingLaunchSeq != 0 {
		t.Fatalf("completed deferred launch misclassified: matched=%v err=%v uncertain=%v pending=%d", matched, err, d.uncertain, d.pendingLaunchSeq)
	}
}
