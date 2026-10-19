package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func shellDAPTestVersion(t *testing.T, path string) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return fileVersion(st)
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
	c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script), "stopOnEntry": true})
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
	if !strings.Contains(string(stack.Body), script) || strings.Contains(string(stack.Body), "/proc/self/fd/6") {
		t.Fatalf("source-line identity leaked internal sealed descriptor: %s", stack.Body)
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
			c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script)})
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
	c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script)})
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

// configurationDone starts only the fixed, blocked Bash bootstrap. It
// does not even parse script bytes until the root Executor has pinned the
// original Bash process and explicitly activated execution. The exec of
// the real script interpreter must preserve that original Bash PID.
func TestShellDAPWorkerHoldsBeforeFirstCommandUntilActivation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gated.sh")
	marker := filepath.Join(dir, "marker")
	source := "#!/usr/bin/env bash\nprintf '%s' $BASHPID > " + shellDAPQuotedTestArg(marker) + "\n"
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
	c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script)})
	c.success("configurationDone", map[string]any{})
	process := c.event("process")
	var proc struct {
		SystemProcessID int `json:"systemProcessId"`
	}
	if err := json.Unmarshal(process.Body, &proc); err != nil || proc.SystemProcessID <= 0 {
		t.Fatalf("unverified original Bash identity: %+v", process)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("script ran before protected activation: %v", err)
	}
	c.success("asaActivate", map[string]any{})
	c.event("terminated")
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != strconv.Itoa(proc.SystemProcessID) {
		t.Fatalf("activation changed original Bash PID or failed: source=%q original=%d err=%v", got, proc.SystemProcessID, err)
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
	if p := c.request("launch", map[string]any{"program": link, "cwd": dir, "fileVersion": shellDAPTestVersion(t, link)}); p.Success {
		t.Fatal("linked Bash script accepted")
	}
	c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script)})
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
	c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script)})
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

// The initial gate must be outside the user script's parser, not just its
// DEBUG trap: an invalid script can still enter the original-Bash PID proof
// state, and its syntax is not evaluated until after explicit activation.
func TestShellDAPWorkerDoesNotParseScriptBeforeActivation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "malformed.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nif then\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, worker := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	c.success("initialize", map[string]any{})
	c.event("initialized")
	c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script)})
	c.success("configurationDone", map[string]any{})
	c.event("process") // still alive, despite syntactically invalid source
	c.success("asaActivate", map[string]any{})
	exit := c.event("exited")
	if strings.Contains(string(exit.Body), "\"exitCode\":0") {
		t.Fatalf("malformed script reported success: %s", exit.Body)
	}
	c.event("terminated")
	c.success("disconnect", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter was not retired")
	}
}

// Regression for the mutable-path gap: the adapter must execute the original
// safely-opened snapshot, not whatever a concurrent writer replaces the
// pathname with while the original Bash PID is blocked.
func TestShellDAPWorkerFrozenSourceSurvivesConcurrentWorkspaceMutation(t *testing.T) {
	for _, mutation := range []string{"rename", "inplace", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "snapshot.sh")
			if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho ORIGINAL_PINNED\n"), 0600); err != nil {
				t.Fatal(err)
			}
			client, worker := net.Pipe()
			defer client.Close()
			done := make(chan error, 1)
			go func() { defer worker.Close(); done <- serveShellDAP(worker, worker) }()
			c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
			c.success("initialize", map[string]any{})
			c.event("initialized")
			c.success("launch", map[string]any{"program": script, "cwd": dir, "fileVersion": shellDAPTestVersion(t, script)})
			newBytes := []byte("#!/usr/bin/env bash\necho REPLACED_UNVERIFIED\n")
			switch mutation {
			case "inplace":
				if err := os.WriteFile(script, newBytes, 0600); err != nil {
					t.Fatal(err)
				}
			case "rename", "symlink":
				backup := filepath.Join(dir, "old.sh")
				if err := os.Rename(script, backup); err != nil {
					t.Fatal(err)
				}
				if mutation == "symlink" {
					replacement := filepath.Join(dir, "replacement.sh")
					if err := os.WriteFile(replacement, newBytes, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(replacement, script); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(script, newBytes, 0600); err != nil {
					t.Fatal(err)
				}
			}
			c.success("configurationDone", map[string]any{})
			c.event("process")
			c.success("asaActivate", map[string]any{})
			c.event("terminated")
			var seenOriginal, seenReplaced bool
			for _, packet := range c.pending {
				if packet.Event != "output" {
					continue
				}
				seenOriginal = seenOriginal || strings.Contains(string(packet.Body), "ORIGINAL_PINNED")
				seenReplaced = seenReplaced || strings.Contains(string(packet.Body), "REPLACED_UNVERIFIED")
			}
			if !seenOriginal || seenReplaced {
				t.Fatalf("mutable path reached interpreter: original=%v replaced=%v", seenOriginal, seenReplaced)
			}
			c.success("disconnect", map[string]any{})
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("adapter did not retire")
			}
		})
	}
}

func TestShellDAPSourceSnapshotRejectsStaleAndNonBoundedFiles(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "source.sh")
	if err := os.WriteFile(script, []byte("echo accepted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	version := shellDAPTestVersion(t, script)
	fd, err := openShellDAPSnapshot(dir, script, version)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	seals, err := unix.FcntlInt(fd.Fd(), unix.F_GET_SEALS, 0)
	if err != nil {
		t.Fatal(err)
	}
	const required = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if seals&required != required {
		t.Fatalf("snapshot memfd not immutable: seals=%x", seals)
	}
	if _, err := fd.WriteAt([]byte("modified"), 0); err == nil {
		t.Fatal("sealed Bash source remained writable")
	}
	// Change the size to guarantee a distinct metadata-version precondition
	// even on filesystems with coarse mtime/ctime resolution.
	if err := os.WriteFile(script, []byte("echo replaced with different size\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openShellDAPSnapshot(dir, script, version); err == nil {
		t.Fatal("stale workspace source version was accepted")
	}
	link := filepath.Join(dir, "link.sh")
	if err := os.Symlink(script, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openShellDAPSnapshot(dir, link, shellDAPTestVersion(t, link)); err == nil {
		t.Fatal("symlink source passed descriptor-anchored open")
	}
	if err := os.WriteFile(script, bytes.Repeat([]byte{'x'}, maxFileReadBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openShellDAPSnapshot(dir, script, shellDAPTestVersion(t, script)); err == nil {
		t.Fatal("oversized snapshot was silently truncated")
	}
}

// Exercise a large number of concurrent adapter-originated notifications and
// control replies against the exact framed stream, not an unordered channel
// of parsed objects. DAP seq must be strictly increasing ON THE WIRE.
func TestShellDAPWorkerConcurrentOutboundWireSequence(t *testing.T) {
	var output bytes.Buffer
	w := &shellDAPWorker{out: &output}
	const goroutines, messages = 8, 35
	var group sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		group.Add(1)
		go func(g int) {
			defer group.Done()
			for i := 0; i < messages; i++ {
				if i%3 == 0 {
					if err := w.response(shellDAPRequest{Seq: g*100 + i + 1, Command: "threads"}, map[string]any{"threads": []any{}}, ""); err != nil {
						t.Error(err)
						return
					}
					continue
				}
				if err := w.event([]string{"stopped", "output", "exited"}[i%3], map[string]any{"worker": g, "iteration": i}); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	group.Wait()
	remaining := output.Bytes()
	last := 0
	for len(remaining) > 0 {
		frame, used, err := readLSPFrame(remaining)
		if err != nil || used <= 0 {
			t.Fatalf("invalid concurrent output framing at message %d: used=%d err=%v", last, used, err)
		}
		var packet dapPacket
		if err := json.Unmarshal(frame, &packet); err != nil {
			t.Fatal(err)
		}
		if packet.Seq != last+1 {
			t.Fatalf("outbound DAP wire sequence jumped or reordered: prev=%d got=%d", last, packet.Seq)
		}
		if packet.Type == "response" && packet.RequestSeq <= 0 {
			t.Fatalf("lost response correlation: %+v", packet)
		}
		last = packet.Seq
		remaining = remaining[used:]
	}
	if last != goroutines*messages {
		t.Fatalf("lost outbound event: got=%d want=%d", last, goroutines*messages)
	}
}

func TestShellDAPWorkerFailureLatchesNoFurtherConfiguration(t *testing.T) {
	for _, cause := range []string{"readiness_timeout", "child_exit_before_ready"} {
		t.Run(cause, func(t *testing.T) {
			pipeRead, pipeWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer pipeRead.Close()
			w := &shellDAPWorker{
				spawned: true, launched: true, configured: false,
				ready: make(chan int), exit: make(chan struct{}), command: pipeWrite,
			}
			if cause == "child_exit_before_ready" {
				close(w.exit)
			}
			timeout := 2 * time.Millisecond
			err = w.awaitShellDAPReady(timeout)
			if err == nil || !w.failed || !w.spawned || w.command != nil {
				t.Fatalf("post-spawn uncertainty was not latched: state=%+v err=%v", w, err)
			}
			client, server := net.Pipe()
			done := make(chan error, 1)
			w.reader, w.out = bufio.NewReader(server), server
			go func() { defer server.Close(); done <- w.serve() }()
			c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
			if r := c.request("configurationDone", map[string]any{}); r.Success || r.Message != "shell_dap_state_uncertain_stop_required" {
				t.Fatalf("second Bash start accepted after uncertain first: %+v", r)
			}
			if w.bash != nil {
				t.Fatal("another Bash process was spawned")
			}
			c.success("disconnect", map[string]any{})
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("adapter not closed")
			}
			client.Close()
		})
	}
}

func TestShellDAPWorkerActivationBrokenPipeLatchesFailure(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rd.Close()
	// Retain the already-closed command stream to force a permanent EPIPE
	// instead of a fabricated negative DAP reply from an unused activation.
	w := &shellDAPWorker{initialized: true, launched: true, configured: true, spawned: true, command: wr}
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	w.reader, w.out = bufio.NewReader(server), server
	go func() { defer server.Close(); done <- w.serve() }()
	c := &shellDAPTestClient{t: t, conn: client, parser: &dapState{conn: client}}
	if r := c.request("asaActivate", map[string]any{}); r.Success || !w.failed || !w.activated {
		t.Fatalf("failed START not terminal: reply=%+v failed=%v activated=%v", r, w.failed, w.activated)
	}
	if r := c.request("asaActivate", map[string]any{}); r.Success || r.Message != "shell_dap_state_uncertain_stop_required" {
		t.Fatalf("activation retry accepted: %+v", r)
	}
	c.success("disconnect", map[string]any{})
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter not closed")
	}
}

func TestShellDAPRootUncertaintyLatchAfterConfiguration(t *testing.T) {
	d := &dapState{stage: "starting", adapter: "shell/bash-trap"}
	failure := shellDAPUncertain(d, "shell_dap_configuration_uncertain", context.Canceled)
	if failure.OK || !d.uncertain || d.stage != "uncertain" {
		t.Fatalf("post-config negative response not latched: %+v state=%+v", failure, d)
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	d.conn = client
	if _, err := d.call(context.Background(), "configurationDone", map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "dap_state_uncertain") {
		t.Fatalf("root DAP accepted a second configure after uncertainty: %v", err)
	}
}
