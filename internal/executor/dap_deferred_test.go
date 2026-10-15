package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func dapFixtureRead(r *bufio.Reader) (dapPacket, error) {
	n := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return dapPacket{}, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(key, "Content-Length") {
			n, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return dapPacket{}, err
			}
		}
	}
	if n <= 0 || n > dapMaxFrame {
		return dapPacket{}, fmt.Errorf("invalid DAP fixture size %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return dapPacket{}, err
	}
	var packet dapPacket
	err := json.Unmarshal(body, &packet)
	return packet, err
}

func dapFixtureResult(c net.Conn, request dapPacket, success bool) error {
	return dapFrame(c, map[string]any{
		"seq": 10 + request.Seq, "type": "response", "request_seq": request.Seq,
		"command": request.Command, "success": success,
		"message": "failure when requested", "body": map[string]any{},
	})
}

func TestDAPDeferredLaunchAllowsConfigurationBeforeLaunchResponse(t *testing.T) {
	client, adapter := net.Pipe()
	defer client.Close()
	defer adapter.Close()
	d := &dapState{conn: client, stage: "starting"}
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapter)
		launch, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if launch.Command != "launch" {
			done <- fmt.Errorf("wrong launch: %s", launch.Command)
			return
		}
		if err := dapFrame(adapter, map[string]any{"seq": 51, "type": "event", "event": "initialized"}); err != nil {
			done <- err
			return
		}
		set, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if set.Command != "setBreakpoints" {
			done <- fmt.Errorf("wrong config: %s", set.Command)
			return
		}
		if err := dapFixtureResult(adapter, set, true); err != nil {
			done <- err
			return
		}
		configured, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if configured.Command != "configurationDone" {
			done <- fmt.Errorf("wrong terminal config: %s", configured.Command)
			return
		}
		// Crucial valid ordering: configurationDone response precedes launch.
		if err := dapFixtureResult(adapter, configured, true); err != nil {
			done <- err
			return
		}
		time.Sleep(30 * time.Millisecond)
		done <- dapFixtureResult(adapter, launch, true)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.callDeferredLaunch(ctx, map[string]any{"mode": "test"}); err != nil {
		t.Fatalf("deferred launch deadlocked before configuration: %v", err)
	}
	if d.pendingLaunchSeq == 0 || !d.initialized {
		t.Fatalf("initialized/pending launch state lost: %+v", d)
	}
	if _, err := d.call(ctx, "threads", map[string]any{}); err == nil || !strings.Contains(err.Error(), "pending_configuration") {
		t.Fatalf("unsafe runtime control during deferred launch: %v", err)
	}
	if _, err := d.call(ctx, "setBreakpoints", map[string]any{}); err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if _, err := d.call(ctx, "configurationDone", map[string]any{}); err != nil {
		t.Fatalf("configurationDone/launch response: %v", err)
	}
	if d.pendingLaunchSeq != 0 || d.uncertain {
		t.Fatalf("launch not reconciled: pending=%d uncertain=%t", d.pendingLaunchSeq, d.uncertain)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("adapter completion timeout")
	}
}

func TestDAPDeferredLaunchFailsClosedOnRejectedLateLaunch(t *testing.T) {
	client, adapter := net.Pipe()
	defer client.Close()
	defer adapter.Close()
	d := &dapState{conn: client, stage: "starting"}
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapter)
		launch, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if err := dapFrame(adapter, map[string]any{"seq": 61, "type": "event", "event": "initialized"}); err != nil {
			done <- err
			return
		}
		configured, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if err := dapFixtureResult(adapter, configured, true); err != nil {
			done <- err
			return
		}
		done <- dapFixtureResult(adapter, launch, false)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.callDeferredLaunch(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.call(ctx, "configurationDone", map[string]any{}); err == nil || !strings.Contains(err.Error(), "deferred_launch_failed") {
		t.Fatalf("late failed launch erroneously accepted: %v", err)
	}
	if !d.uncertain {
		t.Fatal("failed delayed launch did not poison session")
	}
	if _, err := d.call(ctx, "evaluate", map[string]any{"expression": "1"}); err == nil {
		t.Fatal("control allowed after unknown/failed launch")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("adapter timeout")
	}
}

func TestDAPDeferredLaunchRejectsRunInTerminal(t *testing.T) {
	client, adapter := net.Pipe()
	defer client.Close()
	defer adapter.Close()
	d := &dapState{conn: client, stage: "starting"}
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapter)
		_, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if err := dapFrame(adapter, map[string]any{"seq": 100, "type": "request", "command": "runInTerminal",
			"arguments": map[string]any{"args": []string{"/bin/sh", "-c", "do-not-execute"}}}); err != nil {
			done <- err
			return
		}
		reply, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if reply.Type != "response" || reply.RequestSeq != 100 || reply.Success {
			done <- fmt.Errorf("unsafe reverse launch reply: %+v", reply)
			return
		}
		done <- dapFrame(adapter, map[string]any{"seq": 102, "type": "event", "event": "initialized"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.callDeferredLaunch(ctx, map[string]any{}); err != nil {
		t.Fatalf("reverse request handling: %v", err)
	}
	if d.pendingLaunchSeq == 0 {
		t.Fatal("deferred session lost after reverse request")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("adapter timeout")
	}
}

func TestDAPDeferredLaunchEarlyResponseStillWaitsForInitialized(t *testing.T) {
	client, adapter := net.Pipe()
	defer client.Close()
	defer adapter.Close()
	d := &dapState{conn: client, stage: "starting"}
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapter)
		launch, err := dapFixtureRead(reader)
		if err != nil {
			done <- err
			return
		}
		if err := dapFixtureResult(adapter, launch, true); err != nil {
			done <- err
			return
		}
		time.Sleep(40 * time.Millisecond)
		done <- dapFrame(adapter, map[string]any{"seq": 106, "type": "event", "event": "initialized"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.callDeferredLaunch(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !d.initialized || d.pendingLaunchSeq != 0 || d.uncertain {
		t.Fatalf("early response accepted before initialization: initialized=%t pending=%d uncertain=%t", d.initialized, d.pendingLaunchSeq, d.uncertain)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("adapter timeout")
	}
}
