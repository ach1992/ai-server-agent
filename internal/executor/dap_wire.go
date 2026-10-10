package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DAP is the private-stream consumer of #66's existing worker process broker.
// It owns only framed adapter semantics, never a second process registry.
const (
	dapMaxFrame       = 64 << 10
	dapMaxHeader      = 1024
	dapMaxReply       = 16 << 10
	dapMaxEvents      = 24
	dapMaxQueuedBytes = 16 << 10
)

type dapState struct {
	mu                  sync.Mutex // serializes DAP frames and events
	actionMu            sync.Mutex // serializes complete user operations and close
	conn                net.Conn
	pending             []byte // incomplete frame retained across status timeouts
	workspace           string
	adapter             string
	program             string
	adapterPID          int
	adapterPin          *os.File // duplicated original adapter pidfd, not a numeric PID claim
	workerUID           uint32
	debuggeePID         int
	debuggeePin         *os.File
	containment         *dapCgroup
	containmentProven   bool
	launchMayHaveTarget bool
	pinError            string
	uncertain           bool
	uncertainReason     string
	sourceVersion       string
	sourceVersions      map[string]string // workspace-relative breakpoint source identities
	seq                 int
	events              []json.RawMessage
	eventBytes          int
	eventsDropped       uint64
	stage               string
	configured          bool
	socketDir           string
}

func dapFrame(conn net.Conn, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > dapMaxFrame {
		return errors.New("dap_input_frame_too_large")
	}
	buf := append([]byte("Content-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"), body...)
	for len(buf) > 0 {
		n, e := conn.Write(buf)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

// DAP and LSP use the same bounded Content-Length wire envelope. Keep any
// incomplete header/body in this adapter state when a poll deadline expires;
// discarding partial data would corrupt the next command's response.
func (d *dapState) readFrame() (json.RawMessage, error) {
	for {
		body, used, err := readLSPFrame(d.pending)
		if err != nil {
			return nil, fmt.Errorf("dap_bad_frame: %w", err)
		}
		if used > 0 {
			result := append(json.RawMessage(nil), body...)
			d.pending = append(d.pending[:0], d.pending[used:]...)
			if !json.Valid(result) {
				return nil, errors.New("dap_invalid_json")
			}
			return result, nil
		}
		if len(d.pending) > dapMaxHeader+dapMaxFrame+4 {
			return nil, errors.New("dap_frame_too_large")
		}
		var chunk [4096]byte
		n, e := d.conn.Read(chunk[:])
		if n > 0 {
			d.pending = append(d.pending, chunk[:n]...)
			continue
		}
		if e != nil {
			return nil, e
		}
		return nil, io.ErrNoProgress
	}
}

type dapPacket struct {
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	Command    string          `json:"command,omitempty"`
	Event      string          `json:"event,omitempty"`
	RequestSeq int             `json:"request_seq,omitempty"`
	Success    bool            `json:"success,omitempty"`
	Message    string          `json:"message,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

func (d *dapState) acceptEvent(packet dapPacket) {
	if packet.Event == "process" && d.pinError == "" {
		if err := d.pinDebuggeeProcess(packet.Body); err != nil {
			d.pinError = err.Error()
			d.stage = "failed"
		}
	}
	// A rejected second process identity permanently poisons the session.
	// No later continued/stopped/terminated event can undo that verdict.
	if d.pinError != "" {
		d.stage = "failed"
	} else if !d.uncertain {
		switch packet.Event {
		case "stopped":
			d.stage = "stopped"
		case "continued", "process":
			d.stage = "running"
		case "terminated", "exited":
			d.stage = "terminated"
		}
	}
	event, err := json.Marshal(packet)
	if err != nil {
		return
	}
	if len(event) > dapMaxQueuedBytes {
		d.eventsDropped++
		return
	}
	for len(d.events) >= dapMaxEvents || d.eventBytes+len(event) > dapMaxQueuedBytes {
		if len(d.events) == 0 {
			break
		}
		d.eventBytes -= len(d.events[0])
		d.events[0] = nil
		d.events = d.events[1:]
		d.eventsDropped++
	}
	d.events = append(d.events, event)
	d.eventBytes += len(event)
}

func (d *dapState) markUncertain(err error) error {
	d.uncertain = true
	if d.uncertainReason == "" {
		d.uncertainReason = "dap_request_completion_unproven"
	}
	if d.pinError != "" {
		d.stage = "failed"
	} else {
		d.stage = "uncertain"
	}
	return err
}

// A single request/response sequence is serialized per authenticated session.
// Asynchronous notifications are queued within a strict total byte budget.
func (d *dapState) call(ctx context.Context, command string, args any) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, errors.New("dap_disconnected")
	}
	if command != "disconnect" {
		if d.pinError != "" {
			return nil, errors.New("dap_debuggee_identity_unproven: " + d.pinError)
		}
		if d.uncertain {
			return nil, errors.New("dap_state_uncertain: stop or inspect status before restarting")
		}
	}
	d.seq++
	seq := d.seq
	deadline := time.Now().Add(12 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := d.conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	defer d.conn.SetDeadline(time.Time{})
	// After the first launch byte is sent, absence of a process event can
	// NEVER mean no target exists. This is the early-adapter-crash invariant.
	if command == "launch" {
		d.launchMayHaveTarget = true
	}
	if err := dapFrame(d.conn, map[string]any{"seq": seq, "type": "request", "command": command, "arguments": args}); err != nil {
		return nil, d.markUncertain(err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, d.markUncertain(err)
		}
		raw, err := d.readFrame()
		if err != nil {
			return nil, d.markUncertain(fmt.Errorf("dap_transport_unknown: %w", err))
		}
		var packet dapPacket
		if err = json.Unmarshal(raw, &packet); err != nil {
			return nil, d.markUncertain(errors.New("dap_invalid_packet"))
		}
		switch packet.Type {
		case "event":
			d.acceptEvent(packet)
			if d.pinError != "" {
				return nil, d.markUncertain(errors.New("dap_target_identity_unproven"))
			}
		case "request":
			// Explicitly reject adapter-initiated runInTerminal/other reverse
			// commands; never add a hidden PTY or privileged process path.
			d.seq++
			if err := dapFrame(d.conn, map[string]any{"seq": d.seq, "type": "response", "request_seq": packet.Seq, "command": packet.Command, "success": false, "message": "unsupported_adapter_reverse_request"}); err != nil {
				return nil, d.markUncertain(err)
			}
		case "response":
			if packet.RequestSeq != seq || packet.Command != command {
				return nil, d.markUncertain(errors.New("dap_response_identity_mismatch"))
			}
			if !packet.Success {
				msg := packet.Message
				if len(msg) > 300 {
					msg = msg[:300]
				}
				return nil, fmt.Errorf("dap_%s_failed: %s", command, msg)
			}
			if len(packet.Body) > dapMaxReply {
				return nil, d.markUncertain(errors.New("dap_result_too_large"))
			}
			if len(packet.Body) == 0 {
				return json.RawMessage(`{}`), nil
			}
			return packet.Body, nil
		default:
			return nil, d.markUncertain(errors.New("dap_invalid_packet_type"))
		}
	}
}

func (d *dapState) drain(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return
	}
	defer d.conn.SetReadDeadline(time.Time{})
	for i := 0; i < 12; i++ {
		if ctx.Err() != nil {
			return
		}
		_ = d.conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
		raw, err := d.readFrame()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return
			}
			// Non-timeout transport and malformed framing cannot recover
			// ordering guarantees; status stays queryable, actions poison.
			d.markUncertain(fmt.Errorf("dap_drain_transport_unproven: %w", err))
			return
		}
		var packet dapPacket
		if json.Unmarshal(raw, &packet) != nil {
			d.markUncertain(errors.New("dap_drain_invalid_packet"))
			return
		}
		if packet.Type == "event" {
			d.acceptEvent(packet)
		} else if packet.Type == "request" {
			d.seq++
			_ = d.conn.SetWriteDeadline(time.Now().Add(time.Second))
			if err := dapFrame(d.conn, map[string]any{"seq": d.seq, "type": "response", "request_seq": packet.Seq, "command": packet.Command, "success": false, "message": "unsupported_adapter_reverse_request"}); err != nil {
				d.markUncertain(fmt.Errorf("dap_drain_reverse_request_failed: %w", err))
				return
			}
		} else {
			d.markUncertain(errors.New("dap_drain_unexpected_response_or_packet"))
			return
		}
	}
}

// Peek first. A failed response-envelope encoding must NOT erase the events
// or their dropped-event accounting; only acknowledge after a validated reply.
func (d *dapState) peek() (string, []json.RawMessage, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	events := append([]json.RawMessage(nil), d.events...)
	state := d.stage
	if d.pinError != "" {
		state = "failed"
	} else if d.uncertain {
		state = "uncertain"
	}
	return state, events, d.eventsDropped
}
func (d *dapState) acknowledge(events []json.RawMessage, dropped uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.events) < len(events) {
		return
	}
	for i, e := range events {
		if !bytes.Equal(d.events[i], e) {
			return
		}
	}
	for i := range events {
		d.eventBytes -= len(d.events[i])
		d.events[i] = nil
	}
	d.events = d.events[len(events):]
	if d.eventsDropped >= dropped {
		d.eventsDropped -= dropped
	}
}

// Adapter paths are untrusted output. Return workspace-relative paths only;
// never treat adapter paths as authority to read arbitrary executor files.
func normalizeDebugResult(workspace string, raw json.RawMessage) (json.RawMessage, error) {
	var object any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	var walk func(any, int) error
	walk = func(v any, depth int) error {
		if depth > 20 {
			return errors.New("dap_result_too_deep")
		}
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			for k, item := range x {
				if k == "name" {
					if str, ok := item.(string); ok && filepath.IsAbs(str) {
						rel, err := filepath.Rel(workspace, filepath.Clean(str))
						if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
							x[k] = "external"
						} else {
							x[k] = filepath.ToSlash(rel)
						}
					}
					continue
				}
				if k == "path" {
					p, ok := item.(string)
					if !ok {
						return errors.New("dap_bad_source_path")
					}
					if filepath.IsAbs(p) {
						rel, err := filepath.Rel(workspace, filepath.Clean(p))
						if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
							x[k] = "external"
							x["external"] = true
						} else {
							x[k] = filepath.ToSlash(rel)
						}
					}
					continue
				}
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(object, 0); err != nil {
		return nil, err
	}
	b, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	if len(b) > dapMaxReply {
		return nil, errors.New("dap_result_too_large")
	}
	return b, nil
}
