package executor

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type shellDAPTestClient struct {
	t       *testing.T
	conn    net.Conn
	parser  *dapState
	seq     int
	pending []dapPacket
}

func (c *shellDAPTestClient) packet() dapPacket {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	raw, err := c.parser.readFrame()
	if err != nil {
		c.t.Fatal(err)
	}
	var packet dapPacket
	if err := json.Unmarshal(raw, &packet); err != nil {
		c.t.Fatal(err)
	}
	return packet
}
func (c *shellDAPTestClient) request(command string, args any) dapPacket {
	c.t.Helper()
	c.seq++
	if err := dapFrame(c.conn, map[string]any{"seq": c.seq, "type": "request", "command": command, "arguments": args}); err != nil {
		c.t.Fatal(err)
	}
	for {
		p := c.packet()
		if p.Type == "event" {
			c.pending = append(c.pending, p)
			continue
		}
		if p.Type != "response" || p.RequestSeq != c.seq || p.Command != command {
			c.t.Fatalf("unexpected reply to %s: %+v", command, p)
		}
		return p
	}
}
func (c *shellDAPTestClient) success(command string, args any) dapPacket {
	c.t.Helper()
	p := c.request(command, args)
	if !p.Success {
		c.t.Fatalf("%s rejected: %+v", command, p)
	}
	return p
}
func (c *shellDAPTestClient) event(name string) dapPacket {
	c.t.Helper()
	for {
		for i, p := range c.pending {
			if p.Event == name {
				c.pending = append(c.pending[:i], c.pending[i+1:]...)
				return p
			}
		}
		p := c.packet()
		if p.Type != "event" {
			c.t.Fatalf("unexpected response waiting for %s: %+v", name, p)
		}
		c.pending = append(c.pending, p)
	}
}

func TestShellDAPWorkerRealBashBreakpointStepScalarsAndExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "demo.sh")
	source := "#!/usr/bin/env bash\nalpha=7\nbeta=5\nANSWER=11\ntotal=$((alpha+beta))\necho \"$total\"\n"
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	client, worker := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	c.success("initialize", map[string]any{"adapterID": "shell"})
	c.event("initialized")
	c.success("launch", map[string]any{"program": script, "cwd": dir, "stopOnEntry": true})
	reply := c.success("setBreakpoints", map[string]any{"source": map[string]any{"path": script}, "breakpoints": []any{map[string]any{"line": 5}}})
	if !strings.Contains(string(reply.Body), "\"verified\":false") {
		t.Fatalf("Bash line breakpoint incorrectly claimed verified: %s", reply.Body)
	}
	c.success("configurationDone", map[string]any{})
	proc := c.event("process")
	var body struct {
		SystemProcessID int `json:"systemProcessId"`
	}
	if err := json.Unmarshal(proc.Body, &body); err != nil || body.SystemProcessID <= 0 {
		t.Fatalf("no Bash process: %+v", proc)
	}
	if _, err := os.Stat(filepath.Join("/proc", strings.TrimSpace(stringMustNumber(body.SystemProcessID)), "exe")); err != nil {
		t.Fatal(err)
	}
	c.success("asaActivate", map[string]any{})
	stop := c.event("stopped")
	if !strings.Contains(string(stop.Body), "\"entry\"") {
		t.Fatalf("entry stop expected: %s", stop.Body)
	}
	stack := c.success("stackTrace", map[string]any{"threadId": 1})
	if !strings.Contains(string(stack.Body), "\"line\":2") {
		t.Fatalf("wrong entry line: %s", stack.Body)
	}
	c.success("stepIn", map[string]any{"threadId": 1})
	c.event("stopped")
	vars := c.success("variables", map[string]any{"variablesReference": 1})
	if !strings.Contains(string(vars.Body), "\"alpha\"") || !strings.Contains(string(vars.Body), "\"7\"") {
		t.Fatalf("alpha not available: %s", vars.Body)
	}
	c.success("continue", map[string]any{"threadId": 1})
	breakpoint := c.event("stopped")
	if !strings.Contains(string(breakpoint.Body), "\"breakpoint\"") {
		t.Fatalf("breakpoint reason: %s", breakpoint.Body)
	}
	stack = c.success("stackTrace", map[string]any{"threadId": 1})
	if !strings.Contains(string(stack.Body), "\"line\":5") {
		t.Fatalf("wrong breakpoint line: %s", stack.Body)
	}
	vars = c.success("variables", map[string]any{"variablesReference": 1})
	if !strings.Contains(string(vars.Body), "\"beta\"") || !strings.Contains(string(vars.Body), "\"ANSWER\"") {
		t.Fatalf("beta not available: %s", vars.Body)
	}
	c.success("continue", map[string]any{"threadId": 1})
	c.event("terminated")
	c.success("disconnect", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter did not exit after disconnect")
	}
}

func TestShellDAPWorkerRejectsUnsupportedSourceAndBeforeConfigure(t *testing.T) {
	client, worker := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	c.success("initialize", map[string]any{})
	c.event("initialized")
	if r := c.request("launch", map[string]any{"program": "../../etc/passwd", "cwd": "/tmp"}); r.Success {
		t.Fatal("relative outside source accepted")
	}
	if r := c.request("configurationDone", map[string]any{}); r.Success {
		t.Fatal("configuration without accepted launch")
	}
	if r := c.request("runInTerminal", map[string]any{"args": []string{"bash"}}); r.Success {
		t.Fatal("adapter reverse command accepted")
	}
	c.success("disconnect", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter stop timed out")
	}
}
func stringMustNumber(n int) string { return strconv.Itoa(n) }

// Two independent Bash launches prove that next does not enter a called
// function and stepOut resumes only after the caller's next command.
// DEBUG trap emits real Bash function frames, not synthesized Go-only events.
func TestShellDAPWorkerRealBashFunctionStepOverAndOut(t *testing.T) {
	source := "#!/usr/bin/env bash\nalpha=7\nwork() {\n  local inner=4\n  alpha=$((alpha+inner))\n}\nwork\nprintf '%d\\n' \"$alpha\"\n"
	for _, mode := range []string{"next", "stepOut"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "functions.sh")
			if err := os.WriteFile(script, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			client, worker := net.Pipe()
			defer client.Close()
			done := make(chan error, 1)
			go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
			c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
			c.success("initialize", map[string]any{})
			c.event("initialized")
			c.success("launch", map[string]any{"program": script, "cwd": dir})
			c.success("setBreakpoints", map[string]any{
				"source":      map[string]any{"path": script},
				"breakpoints": []any{map[string]any{"line": 7}},
			})
			c.success("configurationDone", map[string]any{})
			c.event("process")
			c.success("asaActivate", map[string]any{})
			bp := c.event("stopped")
			if !strings.Contains(string(bp.Body), "breakpoint") {
				t.Fatalf("function call breakpoint absent: %s", bp.Body)
			}
			stack := c.success("stackTrace", map[string]any{"threadId": 1})
			if !strings.Contains(string(stack.Body), "\"line\":7") {
				t.Fatalf("not stopped before call: %s", stack.Body)
			}
			if mode == "stepOut" {
				c.success("stepIn", map[string]any{"threadId": 1})
				c.event("stopped")
			}
			c.success(mode, map[string]any{"threadId": 1})
			c.event("stopped")
			stack = c.success("stackTrace", map[string]any{"threadId": 1})
			if !strings.Contains(string(stack.Body), "\"line\":8") {
				t.Fatalf("%s did not return to caller line 8: %s", mode, stack.Body)
			}
			c.success("continue", map[string]any{"threadId": 1})
			c.event("terminated")
			c.success("disconnect", map[string]any{})
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("function adapter did not stop")
			}
		})
	}
}

// Instrumentation must not silently turn a failed Bash script into success
// or continue past a failing command under errexit/nounset/pipefail.
func TestShellDAPWorkerPreservesStrictBashExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "strict.sh")
	source := "#!/usr/bin/env bash\nset -euo pipefail\nvalue=42\nfalse\necho SHOULD_NOT_EXECUTE\n"
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	client, worker := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	c.success("initialize", map[string]any{})
	c.event("initialized")
	c.success("launch", map[string]any{"program": script, "cwd": dir})
	c.success("configurationDone", map[string]any{})
	c.event("process")
	c.success("asaActivate", map[string]any{})
	exited := c.event("exited")
	if !strings.Contains(string(exited.Body), "\"exitCode\":1") {
		t.Fatalf("strict Bash failure was hidden: %s", exited.Body)
	}
	for _, event := range c.pending {
		if event.Event == "output" && strings.Contains(string(event.Body), "SHOULD_NOT_EXECUTE") {
			t.Fatal("debugging bypassed Bash strict-mode exit")
		}
	}
	c.event("terminated")
	c.success("disconnect", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Bash adapter not closed")
	}
}

// configurationDone may spawn the original Bash process only into its
// blocked initial hook. No user script command is allowed until the root
// executor verifies the separately reported kernel identity and sends the
// private asaActivate. This test proves the worker-side half of that gate.
func TestShellDAPWorkerHoldsBeforeFirstCommandUntilActivation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gated.sh")
	marker := filepath.Join(dir, "marker")
	source := "#!/usr/bin/env bash\nprintf 'EXECUTED' > " + shellDAPQuotedTestArg(marker) + "\n"
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	client, worker := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	c.success("initialize", map[string]any{})
	c.event("initialized")
	c.success("launch", map[string]any{"program": script, "cwd": dir})
	c.success("configurationDone", map[string]any{})
	c.event("process")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("script ran before protected activation: %v", err)
	}
	c.success("asaActivate", map[string]any{})
	c.event("terminated")
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "EXECUTED" {
		t.Fatalf("activation did not run script command: %q, %v", got, err)
	}
	if r := c.request("asaActivate", map[string]any{}); r.Success {
		t.Fatal("second activation was accepted")
	}
	c.success("disconnect", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("protected adapter not closed")
	}
}

// The temp fixture path contains no quote characters, but keep this shell
// quoting explicit to avoid coupling tests to a particular workspace spelling.
func shellDAPQuotedTestArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Source paths in the adapter must stay within the exact original file.
// Relative traversal, alternate source files, symlinks and unbounded
// breakpoint lists are not a shortcut around the outer broker checks.
func TestShellDAPWorkerRejectsSymlinkAndForeignBreakpoints(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "original.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.sh")
	if err := os.Symlink(script, link); err != nil {
		t.Fatal(err)
	}
	client, worker := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	c.success("initialize", map[string]any{})
	c.event("initialized")
	if p := c.request("launch", map[string]any{"program": link, "cwd": dir}); p.Success {
		t.Fatal("linked Bash script accepted")
	}
	c.success("launch", map[string]any{"program": script, "cwd": dir})
	if p := c.request("setBreakpoints", map[string]any{"source": map[string]any{"path": link}, "breakpoints": []any{}}); p.Success {
		t.Fatal("foreign source breakpoint accepted")
	}
	bulk := make([]any, 33)
	for i := range bulk {
		bulk[i] = map[string]any{"line": i + 1}
	}
	if p := c.request("setBreakpoints", map[string]any{"source": map[string]any{"path": script}, "breakpoints": bulk}); p.Success {
		t.Fatal("oversized breakpoint set accepted")
	}
	c.success("disconnect", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter teardown timed out")
	}
}

// A DEBUG trap must not change the meaning of the target's immediate $?
// inspection. This is a critical Bash semantic guard, not a synthetic DAP
// packet test: the fixture executes a real /usr/bin/bash script.
func TestShellDAPWorkerPreservesPreviousCommandStatus(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "status.sh")
	source := "#!/usr/bin/env bash\nset +e\nfalse\nprevious=$?\nprintf 'LAST:%s\\n' \"$previous\"\n"
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	client, worker := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	c.success("initialize", map[string]any{})
	c.event("initialized")
	c.success("launch", map[string]any{"program": script, "cwd": dir})
	c.success("configurationDone", map[string]any{})
	c.event("process")
	c.success("asaActivate", map[string]any{})
	c.event("terminated")
	seen := false
	for _, ev := range c.pending {
		if ev.Event == "output" && strings.Contains(string(ev.Body), "LAST:1") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("DEBUG instrumentation changed Bash $? semantics: %+v", c.pending)
	}
	c.success("disconnect", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter exit not confirmed")
	}
}

func TestShellDAPWorkerRejectsOversizedDAPHeader(t *testing.T) {
	huge := "Content-Length: " + strings.Repeat("9", 100000)
	_, err := shellDAPRead(bufio.NewReaderSize(strings.NewReader(huge), 4096))
	if err == nil {
		t.Fatal("unbounded Content-Length header was accepted")
	}
	_, err = shellDAPRead(bufio.NewReader(strings.NewReader("Content-Length: 100000\\r\\n\\r\\n")))
	if err == nil {
		t.Fatal("oversized frame was accepted")
	}
}
