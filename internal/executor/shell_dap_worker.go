package executor

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// RunShellDAPHelper is the unprivileged, same-binary DAP adapter entrypoint.
// The root executor owns its cgroup, pidfd, session, audit and authorization;
// this helper cannot connect to the Agent's privileged socket.
func RunShellDAPHelper() int {
	if err := serveShellDAP(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "shell DAP adapter:", err)
		return 1
	}
	return 0
}

// No bashdb, terminal reverse request, pathname FIFO, host-wide signalling,
// third-party module, listener or writeable adapter source is needed. The Bash
// DEBUG trap communicates solely over three inherited anonymous descriptors:
// sealed read-only BASH_ENV, event pipe and command pipe. Even if the worker
// target manipulates these descriptors, it can only affect its own private DAP
// session; the executor's cgroup is authoritative for descendant cleanup.
const shellDAPPrelude = `# sourced only from the sealed inherited BASH_ENV fd
unset BASH_ENV
_asa_target=$ASA_SHELL_TARGET
_asa_breakpoints=$ASA_SHELL_BREAKPOINTS
_asa_entry=$ASA_SHELL_STOP_ON_ENTRY
_asa_first=1
_asa_step=continue
_asa_step_depth=0

_asa_vars() {
  local _asa_name _asa_value _asa_count=0
  while IFS= read -r _asa_name; do
    [[ $_asa_name =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || continue
    case "$_asa_name" in _asa_*|ASA_*|BASH*|COMP*|FUNCNAME|LINENO|PIPESTATUS|PWD|OLDPWD|SHELLOPTS|BASHOPTS) continue;; esac
    _asa_value=${!_asa_name-}
    if (("${#_asa_value}" > 200)); then _asa_value=${_asa_value:0:200}; fi
    builtin printf 'V\t%s\t%q\n' "$_asa_name" "$_asa_value" >&4 || exit 97
    ((_asa_count+=1))
    ((_asa_count>=40)) && break
  done < <(compgen -v)
}
_asa_hook() {
  local _asa_line=$1 _asa_file=$2 _asa_depth=${#FUNCNAME[@]} _asa_reason=''
  [[ "${BASH_SUBSHELL:-0}" == 0 && "$_asa_file" == "$_asa_target" ]] || return 0
  if [[ $_asa_first == 1 ]]; then
    _asa_first=0
    builtin printf 'READY\t%s\n' "$_asa_line" >&4 || exit 97
    local _asa_answer
    IFS= read -r _asa_answer <&5 || exit 98
    [[ $_asa_answer == START ]] || exit 98
  fi
  case "$_asa_step" in
    into) _asa_reason=step;;
    over) ((_asa_depth<=_asa_step_depth)) && _asa_reason=step;;
    out) ((_asa_depth<_asa_step_depth)) && _asa_reason=step;;
  esac
  case "$_asa_breakpoints" in *":${_asa_line}:"*) _asa_reason=breakpoint;; esac
  if [[ $_asa_entry == 1 ]]; then
    _asa_entry=0
    [[ -z $_asa_reason ]] && _asa_reason=entry
  fi
  [[ -n $_asa_reason ]] || return 0
  _asa_step=continue
  _asa_vars
  builtin printf 'STOP\t%s\t%s\t%s\n' "$_asa_line" "$_asa_depth" "$_asa_reason" >&4 || exit 97
  local _asa_answer
  while IFS= read -r _asa_answer <&5; do
    case "$_asa_answer" in
      CONTINUE) _asa_step=continue; return 0;;
      INTO) _asa_step=into; _asa_step_depth=$_asa_depth; return 0;;
      OVER) _asa_step=over; _asa_step_depth=$_asa_depth; return 0;;
      OUT) _asa_step=out; _asa_step_depth=$_asa_depth; return 0;;
      *) exit 98;;
    esac
  done
  exit 98
}
set -T
trap '_asa_hook "$LINENO" "${BASH_SOURCE[0]}" "$BASH_COMMAND"' DEBUG
`

type shellDAPRequest struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Arguments json.RawMessage `json:"arguments"`
}
type shellDAPVariable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	VariablesReference int    `json:"variablesReference"`
}
type shellDAPWorker struct {
	reader        *bufio.Reader
	out           io.Writer
	sendMu        sync.Mutex
	mu            sync.Mutex
	seq           int
	program       string
	workspace     string
	stopEntry     bool
	points        []int
	initialized   bool
	launched      bool
	configured    bool
	activated     bool
	bash          *exec.Cmd
	command       *os.File
	ready         chan int
	exit          chan struct{}
	paused        bool
	line          int
	variables     []shellDAPVariable
	variableBytes int
}

func shellDAPRead(r *bufio.Reader) (shellDAPRequest, error) {
	var msg shellDAPRequest
	// ReadSlice stops at the bounded reader capacity instead of
	// allocating an unbounded attacker-supplied header via ReadString.
	line, err := r.ReadSlice('\n')
	if err == io.EOF && len(line) == 0 {
		return msg, io.EOF
	}
	if err != nil || len(line) > 128 || !bytes.HasPrefix(line, []byte("Content-Length: ")) || !bytes.HasSuffix(line, []byte("\r\n")) {
		return msg, errors.New("invalid DAP Content-Length header")
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(string(line), "Content-Length: "), "\r\n"))
	if err != nil || n < 2 || n > dapMaxFrame {
		return msg, errors.New("invalid DAP frame length")
	}
	end := make([]byte, 2)
	if _, err := io.ReadFull(r, end); err != nil || !bytes.Equal(end, []byte("\r\n")) {
		return msg, errors.New("invalid DAP header delimiter")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return msg, err
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Type != "request" || msg.Seq <= 0 || msg.Command == "" {
		return msg, errors.New("invalid DAP request")
	}
	return msg, nil
}

func (w *shellDAPWorker) send(payload any) error {
	body, err := json.Marshal(payload)
	if err != nil || len(body) > dapMaxFrame {
		return errors.New("shell DAP response too large")
	}
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	_, err = fmt.Fprintf(w.out, "Content-Length: %d\r\n\r\n", len(body))
	if err == nil {
		n, writeErr := w.out.Write(body)
		if writeErr != nil {
			return writeErr
		}
		if n != len(body) {
			return io.ErrShortWrite
		}
	}
	return err
}
func (w *shellDAPWorker) response(r shellDAPRequest, body any, failure string) error {
	w.mu.Lock()
	w.seq++
	seq := w.seq
	w.mu.Unlock()
	packet := map[string]any{"seq": seq, "type": "response", "request_seq": r.Seq, "command": r.Command, "success": failure == ""}
	if failure != "" {
		packet["message"] = failure
	} else {
		packet["body"] = body
	}
	return w.send(packet)
}
func (w *shellDAPWorker) event(name string, body any) error {
	w.mu.Lock()
	w.seq++
	seq := w.seq
	w.mu.Unlock()
	return w.send(map[string]any{"seq": seq, "type": "event", "event": name, "body": body})
}

func (w *shellDAPWorker) consumeTrace(r *os.File) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), "\t", 4)
		if len(fields) == 2 && fields[0] == "READY" {
			line, err := strconv.Atoi(fields[1])
			if err == nil && line > 0 && line <= 1000000 {
				select {
				case w.ready <- line:
				default:
				}
			}
			continue
		}
		if len(fields) == 3 && fields[0] == "V" {
			w.mu.Lock()
			// The Executor's DAP result limit is 16 KiB. Budget the
			// *whole* scalar snapshot below that limit rather than
			// relying on a per-variable cap that permits 40*512 bytes.
			cost := len(fields[1]) + len(fields[2]) + 96
			if len(w.variables) < 40 && len(fields[1]) <= 128 && len(fields[2]) <= 512 &&
				w.variableBytes+cost <= 10<<10 {
				w.variables = append(w.variables, shellDAPVariable{Name: fields[1], Value: fields[2]})
				w.variableBytes += cost
			}
			w.mu.Unlock()
			continue
		}
		if len(fields) == 4 && fields[0] == "STOP" {
			line, lineErr := strconv.Atoi(fields[1])
			depth, depthErr := strconv.Atoi(fields[2])
			if lineErr != nil || depthErr != nil || line < 1 || line > 1000000 || depth < 2 || depth > 512 {
				continue
			}
			if fields[3] != "entry" && fields[3] != "step" && fields[3] != "breakpoint" {
				continue
			}
			w.mu.Lock()
			w.line, w.paused = line, true
			w.mu.Unlock()
			_ = w.event("stopped", map[string]any{"reason": fields[3], "threadId": 1, "allThreadsStopped": true})
		}
	}
	_ = r.Close()
}

func (w *shellDAPWorker) startBash() error {
	prelude, err := unix.MemfdCreate("asa-shell-dap-prelude", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(prelude), "shell-dap-sealed-prelude")
	defer file.Close()
	if _, err = file.Write([]byte(shellDAPPrelude)); err != nil {
		return err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE); err != nil {
		return err
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		eventReader.Close()
		eventWriter.Close()
		return err
	}
	cmd := exec.Command("/usr/bin/bash", "--noprofile", "--norc", w.program)
	cmd.Dir = w.workspace
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME"), "LC_ALL=C",
		"BASH_ENV=/proc/self/fd/3", "ASA_SHELL_TARGET=" + w.program,
		"ASA_SHELL_BREAKPOINTS=" + shellDAPPoints(w.points),
		"ASA_SHELL_STOP_ON_ENTRY=" + func() string {
			if w.stopEntry {
				return "1"
			}
			return "0"
		}(),
	}
	cmd.ExtraFiles = []*os.File{file, eventWriter, commandReader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.Stdout = &shellDAPOutput{w: w, category: "stdout"}
	cmd.Stderr = &shellDAPOutput{w: w, category: "stderr"}
	if err := cmd.Start(); err != nil {
		eventReader.Close()
		eventWriter.Close()
		commandReader.Close()
		commandWriter.Close()
		return err
	}
	eventWriter.Close()
	commandReader.Close()
	w.mu.Lock()
	w.bash, w.command = cmd, commandWriter
	w.exit, w.ready = make(chan struct{}), make(chan int, 1)
	w.mu.Unlock()
	go w.consumeTrace(eventReader)
	go func() {
		exitCode := 0
		if err := cmd.Wait(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				exitCode = exit.ExitCode()
			} else {
				exitCode = -1
			}
		}
		w.mu.Lock()
		w.paused = false
		w.mu.Unlock()
		_ = w.event("exited", map[string]any{"exitCode": exitCode})
		_ = w.event("terminated", map[string]any{})
		close(w.exit)
	}()
	// First command is paused by the sealed prelude, BEFORE script code is
	// executed. This handshake lets the Executor verify the original Bash PID,
	// trusted executable inode and exact private cgroup membership.
	select {
	case <-w.ready:
		return nil
	case <-w.exit:
		return errors.New("Bash exited before first debug trap")
	case <-time.After(5 * time.Second):
		return errors.New("Bash debug trap did not become ready")
	}
}
func shellDAPPoints(lines []int) string {
	var sb strings.Builder
	sb.WriteByte(':')
	for _, n := range lines {
		sb.WriteString(strconv.Itoa(n))
		sb.WriteByte(':')
	}
	return sb.String()
}

type shellDAPOutput struct {
	w        *shellDAPWorker
	category string
}

func (o *shellDAPOutput) Write(p []byte) (int, error) {
	original := len(p)
	// Bounded, non-buffering DAP output; no shell escape sequences are run
	// by the adapter. Ordinary command output remains text-only and opt-in.
	for len(p) > 0 {
		n := len(p)
		if n > 2048 {
			n = 2048
		}
		data := p[:n]
		value, encoding := string(data), "utf8"
		if !utf8.Valid(data) {
			value, encoding = base64.StdEncoding.EncodeToString(data), "base64"
		}
		// DAP output is text. Explicitly label any binary/non-UTF8
		// segment rather than silently replacing invalid bytes.
		if err := o.w.event("output", map[string]any{
			"category": o.category, "output": value, "encoding": encoding,
		}); err != nil {
			return original - len(p), err
		}
		p = p[n:]
	}
	return original, nil
}

func serveShellDAP(input io.Reader, output io.Writer) error {
	w := &shellDAPWorker{reader: bufio.NewReaderSize(input, 4096), out: output}
	defer func() {
		if w.command != nil {
			_ = w.command.Close()
		}
	}()
	for {
		req, err := shellDAPRead(w.reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var body any = map[string]any{}
		failure := ""
		switch req.Command {
		case "initialize":
			if w.initialized {
				failure = "shell_dap_already_initialized"
				break
			}
			w.initialized = true
			body = map[string]any{"supportsConfigurationDoneRequest": true, "supportsEvaluateForHovers": false, "supportsStepBack": false, "supportsSetVariable": false}
		case "launch":
			if !w.initialized {
				failure = "shell_dap_not_initialized"
				break
			}
			if w.launched {
				failure = "shell_dap_already_launched"
				break
			}
			var in struct {
				Program     string `json:"program"`
				Cwd         string `json:"cwd"`
				StopOnEntry bool   `json:"stopOnEntry"`
			}
			if json.Unmarshal(req.Arguments, &in) != nil || !filepath.IsAbs(in.Program) || !filepath.IsAbs(in.Cwd) ||
				filepath.Clean(in.Program) != in.Program || filepath.Clean(in.Cwd) != in.Cwd ||
				!withinPath(in.Cwd, in.Program) {
				failure = "shell_dap_invalid_launch"
				break
			}
			info, err := os.Lstat(in.Program)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
				(filepath.Ext(in.Program) != ".sh" && filepath.Ext(in.Program) != ".bash") {
				failure = "shell_dap_invalid_source"
				break
			}
			w.launched, w.program, w.workspace, w.stopEntry = true, in.Program, in.Cwd, in.StopOnEntry
		case "setBreakpoints":
			if !w.launched || w.configured {
				failure = "shell_dap_breakpoints_not_configurable"
				break
			}
			var in struct {
				Source struct {
					Path string `json:"path"`
				} `json:"source"`
				Breakpoints []struct {
					Line int `json:"line"`
				} `json:"breakpoints"`
			}
			if json.Unmarshal(req.Arguments, &in) != nil || in.Source.Path != w.program || len(in.Breakpoints) > 32 {
				failure = "shell_dap_invalid_breakpoints"
				break
			}
			w.points = w.points[:0]
			points := make([]map[string]any, 0, len(in.Breakpoints))
			for _, p := range in.Breakpoints {
				if p.Line <= 0 || p.Line > 1000000 {
					failure = "shell_dap_invalid_breakpoints"
					break
				}
				w.points = append(w.points, p.Line)
				// The DEBUG trap accepts this line as a requested breakpoint,
				// but Bash does not expose a proof that it is executable.
				// Never claim source-level breakpoint verification before a hit.
				points = append(points, map[string]any{
					"verified": false, "line": p.Line,
					"message": "pending Bash DEBUG trap hit",
				})
			}
			if failure == "" {
				sort.Ints(w.points)
				body = map[string]any{"breakpoints": points}
			}
		case "configurationDone":
			if !w.launched || w.configured {
				failure = "shell_dap_not_configurable"
				break
			}
			if err := w.startBash(); err != nil {
				failure = "shell_dap_start_failed: " + err.Error()
				break
			}
			w.configured = true
			// This process event must be received and verified by the root
			// broker while Bash is still blocked at the first DEBUG trap.
			if err := w.event("process", map[string]any{"name": "Bash", "isLocalProcess": true, "systemProcessId": w.bash.Process.Pid, "startMethod": "launch"}); err != nil {
				return err
			}
		case "asaActivate":
			if !w.configured || w.activated {
				failure = "shell_dap_not_activatable"
				break
			}
			w.activated = true
			if _, err := io.WriteString(w.command, "START\n"); err != nil {
				failure = "shell_dap_activation_failed"
			}
		case "threads":
			if !w.configured {
				failure = "shell_dap_not_configured"
			} else {
				body = map[string]any{"threads": []any{map[string]any{"id": 1, "name": "Bash main script"}}}
			}
		case "stackTrace":
			if !shellDAPPositiveThread(req.Arguments) {
				failure = "shell_dap_invalid_thread"
				break
			}
			w.mu.Lock()
			paused, line := w.paused, w.line
			w.mu.Unlock()
			if !paused {
				failure = "shell_dap_not_stopped"
			} else {
				body = map[string]any{"stackFrames": []any{map[string]any{"id": 1, "name": "Bash command", "line": line, "column": 1, "source": map[string]any{"path": w.program, "name": filepath.Base(w.program)}}}, "totalFrames": 1}
			}
		case "scopes":
			var in struct {
				FrameID int `json:"frameId"`
			}
			if json.Unmarshal(req.Arguments, &in) != nil || in.FrameID != 1 {
				failure = "shell_dap_invalid_frame"
				break
			}
			w.mu.Lock()
			paused := w.paused
			w.mu.Unlock()
			if !paused {
				failure = "shell_dap_not_stopped"
			} else {
				body = map[string]any{"scopes": []any{map[string]any{"name": "Bash scalars (bounded snapshot)", "variablesReference": 1, "expensive": false}}}
			}
		case "variables":
			var in struct {
				VariablesReference int `json:"variablesReference"`
			}
			if json.Unmarshal(req.Arguments, &in) != nil || in.VariablesReference != 1 {
				failure = "shell_dap_invalid_reference"
				break
			}
			w.mu.Lock()
			paused, variables := w.paused, append([]shellDAPVariable(nil), w.variables...)
			w.mu.Unlock()
			if !paused {
				failure = "shell_dap_not_stopped"
			} else {
				body = map[string]any{"variables": variables}
			}
		case "continue", "next", "stepIn", "stepOut":
			if !shellDAPPositiveThread(req.Arguments) {
				failure = "shell_dap_invalid_thread"
				break
			}
			w.mu.Lock()
			paused := w.paused
			w.paused = false
			w.variables = nil
			w.variableBytes = 0
			w.mu.Unlock()
			if !paused {
				failure = "shell_dap_not_stopped"
				break
			}
			mode := map[string]string{"continue": "CONTINUE", "next": "OVER", "stepIn": "INTO", "stepOut": "OUT"}[req.Command]
			// Order continued before the next stopped event from Bash.
			// Emitting continued after the command write would race a very
			// short debuggee and falsely leave the DAP state "running".
			if err := w.event("continued", map[string]any{"threadId": 1, "allThreadsContinued": true}); err != nil {
				return err
			}
			if _, err := io.WriteString(w.command, mode+"\n"); err != nil {
				failure = "shell_dap_resume_failed"
				_ = w.event("terminated", map[string]any{})
			} else {
				body = map[string]any{"allThreadsContinued": true}
			}
		case "disconnect":
			if err := w.response(req, body, ""); err != nil {
				return err
			}
			return nil // root broker verifies actual descendant cgroup cleanup
		default:
			failure = "unsupported_shell_dap_request"
		}
		if err := w.response(req, body, failure); err != nil {
			return err
		}
		if req.Command == "initialize" && failure == "" {
			if err := w.event("initialized", map[string]any{}); err != nil {
				return err
			}
		}
	}
}

// A single Bash process has exactly one controlled DAP thread. A client
// cannot direct a resume or stack query to an adapter-invented thread ID.
func shellDAPPositiveThread(raw json.RawMessage) bool {
	var in struct {
		ThreadID int `json:"threadId"`
	}
	return json.Unmarshal(raw, &in) == nil && in.ThreadID == 1
}
