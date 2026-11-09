package managedincus

// Optional native Incus CLI backend, executed only inside the authenticated
// root Executor. No daemon, project registry, QEMU compatibility shim or
// mandatory Incus package is introduced by Agent startup.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ach1992/ai-server-agent/internal/secretoutput"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Request struct {
	Operation   string   `json:"operation"`
	Project     string   `json:"project,omitempty"`
	Instance    string   `json:"instance,omitempty"`
	UUID        string   `json:"uuid,omitempty"`
	Image       string   `json:"image,omitempty"`
	Type        string   `json:"type,omitempty"` // container or virtual-machine
	WorkID      string   `json:"work_id,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
	ActorID     string   `json:"-"` // injected only by authenticated root Executor
	CPU         int      `json:"cpu,omitempty"`
	MemoryMiB   int      `json:"memory_mib,omitempty"`
	DiskGiB     int      `json:"disk_gib,omitempty"`
	StoragePool string   `json:"storage_pool,omitempty"`
	Command     []string `json:"command,omitempty"`
	Snapshot    string   `json:"snapshot,omitempty"`
}
type Instance struct {
	Name            string                       `json:"name"`
	Project         string                       `json:"project,omitempty"`
	Type            string                       `json:"type,omitempty"`
	Status          string                       `json:"status,omitempty"`
	Config          map[string]string            `json:"config,omitempty"`
	Devices         map[string]map[string]string `json:"devices,omitempty"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices,omitempty"`
	State           *struct {
		Network map[string]struct {
			Addresses []struct {
				Address string `json:"address"`
				Family  string `json:"family"`
				Scope   string `json:"scope"`
			} `json:"addresses"`
		} `json:"network"`
	} `json:"state,omitempty"`
}
type Summary struct {
	HostID      string   `json:"host_id"`
	Project     string   `json:"project"`
	Name        string   `json:"name"`
	UUID        string   `json:"uuid,omitempty"`
	Generation  string   `json:"generation,omitempty"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	WorkID      string   `json:"work_id,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
	ActorID     string   `json:"-"` // injected only by authenticated root Executor
	CPU         string   `json:"cpu,omitempty"`
	Memory      string   `json:"memory,omitempty"`
	DiskLimit   string   `json:"disk_limit,omitempty"`
	StoragePool string   `json:"storage_pool,omitempty"`
	Addresses   []string `json:"addresses,omitempty"`
	Managed     bool     `json:"managed"`
}
type Result struct {
	Status             string            `json:"status"`
	HostID             string            `json:"host_id"`
	Available          bool              `json:"available"`
	Diagnostic         string            `json:"diagnostic,omitempty"`
	Instances          []Summary         `json:"instances,omitempty"`
	Instance           *Summary          `json:"instance,omitempty"`
	Output             string            `json:"output,omitempty"`
	ExitCode           int               `json:"exit_code,omitempty"`
	Snapshots          []string          `json:"snapshots,omitempty"`
	Pending            []CreateIntent    `json:"pending_creates,omitempty"`
	Operations         []NativeOperation `json:"native_operations,omitempty"`
	Operation          *NativeOperation  `json:"native_operation,omitempty"`
	OutputTruncated    bool              `json:"output_truncated,omitempty"`
	OutputBytesSeen    int64             `json:"output_bytes_seen,omitempty"`
	OutputOmittedBytes int64             `json:"output_omitted_bytes,omitempty"`
	TimedOut           bool              `json:"timed_out,omitempty"`
	GuestExecAttempted bool              `json:"-"` // internal dispatch evidence: preflight does not execute guest command
}
type Backend struct {
	HostID         string
	Binary         string               // absolute trusted CLI; test fixture overrides only in tests
	StateDir       string               // lock directory already owned by root Executor
	capacityProbe  func(int, int) error // test-only; production always checks actual host capacity
	beforeMutation func(Request)        // test-only adversarial interleaving before final identity check
	apiOverride    nativeAPIFunc        // test-only native REST fixture
}

const maxOutput = 64 << 10

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}$`)
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:@-]{0,255}$`)

func validName(s string) bool { return namePattern.MatchString(s) }
func (b Backend) binary() (string, error) {
	if b.Binary != "" {
		return b.Binary, nil
	} // a private, explicitly injected test fixture
	for _, path := range []string{"/usr/bin/incus", "/usr/local/bin/incus"} {
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		sys, ok := fi.Sys().(*syscall.Stat_t)
		if ok && sys.Uid == 0 && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0 && fi.Mode().Perm()&0022 == 0 {
			return path, nil
		}
	}
	return "", errors.New("incus_missing: install and initialize optional Incus on an authorized host; ordinary Agent tools remain available")
}

type bounded struct {
	b    bytes.Buffer
	seen int
}

func (x *bounded) Write(p []byte) (int, error) {
	x.seen += len(p)
	remain := maxOutput - x.b.Len()
	if remain > 0 {
		x.b.Write(p[:min(len(p), remain)])
	}
	return len(p), nil
}
func (b Backend) cli(ctx context.Context, stdin io.Reader, discardOutput bool, args ...string) (string, int, bool, error) {
	out, code, truncated, _, err := b.cliProtected(ctx, stdin, discardOutput, "", args...)
	return out, code, truncated, err
}

// cliProtected streams the exact private value through stdin and redacts
// observed native output before it enters bounded diagnostic capture.
func (b Backend) cliProtected(ctx context.Context, stdin io.Reader, discardOutput bool, secret string, args ...string) (string, int, bool, int64, error) {
	bin, err := b.binary()
	if err != nil {
		return "", -1, false, 0, err
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"--force-local"}, args...)...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LC_ALL=C.UTF-8", "LANG=C.UTF-8"}
	cmd.Stdin = stdin
	var out bounded
	var redactor *secretoutput.Redactor
	if discardOutput {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	} else if secret != "" {
		redactor = secretoutput.New(&out, []byte(secret))
		cmd.Stdout, cmd.Stderr = redactor, redactor
	} else {
		cmd.Stdout, cmd.Stderr = &out, &out
	}
	err = cmd.Run()
	if redactor != nil {
		if flushErr := redactor.Flush(); flushErr != nil {
			err = errors.Join(err, fmt.Errorf("private output flush failed: %w", flushErr))
		}
	}
	code := 0
	if err != nil {
		code = -1
		var e *exec.ExitError
		if errors.As(err, &e) {
			code = e.ExitCode()
		}
	}
	if discardOutput {
		return "", code, false, 0, err
	}
	// Count post-redaction bytes, including bytes beyond the retained
	// 64 KiB diagnostic window; do not expose secret plaintext volume.
	return out.b.String(), code, out.seen > maxOutput, int64(out.seen), err
}

// NativeCommandError keeps provider output out of the error string used by
// Executor reason-code classification. The bounded diagnostic is delivered
// separately in the authenticated managed_environment response.
type NativeCommandError struct {
	Stage        string
	ExitCode     int
	Diagnostic   string
	BytesSeen    int64
	OmittedBytes int64
	Truncated    bool
	TimedOut     bool
	Cause        error
}

func (e *NativeCommandError) Error() string {
	return fmt.Sprintf("native Incus %s failed (exit_code=%d): %v", e.Stage, e.ExitCode, e.Cause)
}

func (e *NativeCommandError) Unwrap() error { return e.Cause }

func (b Backend) command(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, code, truncated, seen, err := b.cliProtected(runCtx, nil, false, "", args...)
	if err != nil {
		// A failed native command may also have returned partial stdout.
		// Keep actual captured-native bytes separate from presentation size:
		// redaction can expand or contract the bounded diagnostic.
		safe, clipped := secretoutput.BoundedDiagnosticText(out, maxOutput)
		return "", &NativeCommandError{
			Stage: args[0], ExitCode: code, Diagnostic: safe,
			BytesSeen: seen, OmittedBytes: seen - int64(len(out)) + clipped,
			Truncated: truncated || clipped > 0,
			TimedOut:  errors.Is(runCtx.Err(), context.DeadlineExceeded),
			Cause:     err,
		}
	}
	if truncated {
		return "", errors.New("incus output exceeded bounded response")
	}
	return out, nil
}
func summarize(x Instance, host string) Summary {
	p := x.Project
	if p == "" {
		p = "default"
	}
	c := x.Config
	out := Summary{HostID: host, Project: p, Name: x.Name, UUID: c["volatile.uuid"], Generation: c["volatile.uuid.generation"], Type: x.Type, Status: x.Status, WorkID: c["user.asa.work"], OperationID: c["user.asa.operation"], CPU: c["limits.cpu"], Memory: c["limits.memory"], Managed: c["user.asa.host"] == host && host != ""}
	root := x.ExpandedDevices["root"]
	if len(root) == 0 {
		root = x.Devices["root"]
	}
	out.DiskLimit = root["size"]
	out.StoragePool = root["pool"]
	if x.State != nil {
		for _, iface := range x.State.Network {
			for _, a := range iface.Addresses {
				if a.Scope == "global" && a.Address != "" {
					out.Addresses = append(out.Addresses, a.Address)
				}
			}
		}
	}
	return out
}
func (b Backend) List(ctx context.Context, project string) ([]Summary, error) {
	args := []string{"list", "--format", "json"}
	if project == "" {
		args = append(args, "--all-projects")
	} else {
		if !validName(project) {
			return nil, errors.New("invalid Incus project")
		}
		args = append(args, "--project", project)
	}
	out, err := b.command(ctx, 25*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var xs []Instance
	if err := json.Unmarshal([]byte(out), &xs); err != nil {
		return nil, errors.New("invalid native Incus list JSON")
	}
	// An authoritative inventory is never inferred from JSON null or a
	// partially identified row. In all-project mode the native project MUST
	// be present: summarize()'s legacy default-project fallback cannot prove
	// the absence of an uncertain create in a different project.
	if xs == nil {
		return nil, errors.New("incus_native_inventory_unverified: native list must be a non-null array")
	}
	result := make([]Summary, 0, len(xs))
	for _, x := range xs {
		if !validName(x.Name) || (project == "" && !validName(x.Project)) {
			return nil, errors.New("incus_native_inventory_unverified: native instance lacks a valid name or all-project identity")
		}
		if project != "" {
			x.Project = project
		}
		result = append(result, summarize(x, b.HostID))
	}
	return result, nil
}

// Snapshot enumeration is native Incus state, never a second registry.
func (b Backend) Snapshots(ctx context.Context, project, name string) ([]string, error) {
	out, err := b.command(ctx, 25*time.Second, "snapshot", "list", name, "--project", project, "--format", "json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, errors.New("invalid native Incus snapshot list JSON")
	}
	names := make([]string, 0, len(rows))
	for _, v := range rows {
		if v.Name != "" {
			names = append(names, v.Name)
		}
	}
	return names, nil
}
func hasSnapshot(names []string, name string) bool {
	for _, v := range names {
		if v == name {
			return true
		}
	}
	return false
}
func (b Backend) inspect(ctx context.Context, project, name string) (Summary, bool, error) {
	all, err := b.List(ctx, project)
	if err != nil {
		return Summary{}, false, err
	}
	for _, v := range all {
		if v.Name == name && v.Project == project {
			return v, true, nil
		}
	}
	return Summary{}, false, nil
}
func (b Backend) selectInstance(ctx context.Context, req Request) (Summary, error) {
	if !validName(req.Project) || !validName(req.Instance) || !uuidPattern.MatchString(req.UUID) {
		return Summary{}, errors.New("project, instance and verified native UUID required")
	}
	native, found, err := b.inspect(ctx, req.Project, req.Instance)
	if err != nil {
		return Summary{}, err
	}
	if !found {
		return Summary{}, errors.New("instance_missing: rediscover native Incus state before retry")
	}
	// The authenticated root operator may manage native Incus guests already
	// present on this host; an Agent creation tag is not an authorization
	// boundary. Always require the *observed* provider UUID, not a name alone.
	// checkConfirmedIdentity still rejects any guest that collides with a
	// journaled Agent create, including an externally replaced native UUID.
	if !uuidPattern.MatchString(native.UUID) || native.UUID != req.UUID {
		return Summary{}, errors.New("instance_identity_mismatch: supplied native UUID does not match the currently observed host/project/instance")
	}
	if err := b.checkConfirmedIdentity(native); err != nil {
		return Summary{}, err
	}
	return native, nil
}

// A previously observed guest disappearing or presenting a different
// native identity is a verified change. Transport, decoding and journal
// observation failures are not proof that the guest changed.
func verifiedNativeIdentityChange(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case strings.HasPrefix(err.Error(), "instance_missing:"),
		strings.HasPrefix(err.Error(), "instance_identity_mismatch:"),
		strings.HasPrefix(err.Error(), "create_intent_identity_mismatch:"),
		strings.HasPrefix(err.Error(), "create_intent_native_uuid_conflict:"),
		strings.HasPrefix(err.Error(), "create_intent_abandoned_identity_conflict:"):
		return true
	}
	return false
}

func (b Backend) Lock(fn func() (Result, error)) (Result, error) {
	// Scoped OS lock serializes Agent-owned create/reconcile and guest mutations. It is NOT
	// a cluster lock, a project manager or a process/guest lifecycle registry.
	dir := filepath.Join(b.StateDir, "managed-incus")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return Result{}, err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return Result{}, errors.New("unsafe Incus lock directory")
	}
	f, err := os.OpenFile(filepath.Join(dir, "create.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return Result{}, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
func (b Backend) Ready(ctx context.Context) Result {
	r := Result{HostID: b.HostID, Status: "unavailable"}
	if _, err := b.binary(); err != nil {
		r.Diagnostic = err.Error()
		return r
	}
	// "incus info" does not support --format=json in supported Incus
	// releases. Query the native API instead; validate the response so a
	// successful command with non-API output cannot falsely report readiness.
	root, err := b.command(ctx, 12*time.Second, "query", "/1.0")
	if err != nil {
		r.Diagnostic = "incus_unready: native daemon/socket/project not initialized or unavailable; inspect incus admin status and initialization"
		var native *NativeCommandError
		if errors.As(err, &native) {
			r.ExitCode = native.ExitCode
			r.Output = native.Diagnostic
			r.OutputBytesSeen = native.BytesSeen
			r.OutputOmittedBytes = native.OmittedBytes
			r.OutputTruncated = native.Truncated
			r.TimedOut = native.TimedOut
		}
		return r
	}
	var api struct {
		Version string `json:"api_version"`
	}
	if json.Unmarshal([]byte(root), &api) != nil || api.Version == "" {
		r.Diagnostic = "incus_unready: native API root returned invalid or incomplete JSON"
		return r
	}
	r.Available = true
	r.Status = "ready"
	if _, err := os.Stat("/dev/kvm"); err != nil {
		r.Diagnostic = "KVM unavailable for VMs; containers may still work; verify /dev/kvm and host virtualization"
	}
	return r
}

// Preserve launch diagnostics when a more authoritative post-launch
// verification error must be reported. The outer recovery reason stays first.
func postLaunchFailure(message string, launchErr error) error {
	if launchErr == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, launchErr)
}

func (b Backend) Run(ctx context.Context, req Request, approval bool, secret string) (Result, error) {
	if req.Operation == "operation_list" {
		return b.Lock(func() (Result, error) {
			journal, err := b.loadNativeOperations()
			if err != nil {
				return Result{}, err
			}
			result := Result{Status: "native_operations_observed", HostID: b.HostID, Available: true, Operations: []NativeOperation{}}
			if req.Project != "" && !validName(req.Project) {
				return result, errors.New("invalid native operation project filter")
			}
			if req.Instance != "" && !validName(req.Instance) {
				return result, errors.New("invalid native operation instance filter")
			}
			for _, v := range journal.Operations {
				if v.ActorID != "" && v.ActorID != req.ActorID {
					continue
				}
				if req.Project != "" && v.Project != req.Project {
					continue
				}
				if req.Instance != "" && v.Instance != req.Instance {
					continue
				}
				result.Operations = append(result.Operations, v)
				if len(result.Operations) > 64 {
					return Result{}, errors.New("native_operation_list_bounded: more than 64 records; narrow project/instance filter")
				}
			}
			return result, nil
		})
	}
	if req.Operation == "operation_status" {
		return b.Lock(func() (Result, error) { return b.nativeStatusLocked(ctx, req) })
	}
	if req.Operation == "resolve_uncertain" {
		return b.Lock(func() (Result, error) { return b.resolveUncertainLocked(ctx, req, approval) })
	}
	if req.Operation == "pending_creates" {
		return b.Lock(func() (Result, error) {
			journal, err := b.loadIntents()
			if err != nil {
				return Result{}, err
			}
			return Result{Status: "pending_creates_observed", HostID: b.HostID, Available: true, Pending: journal.Intents}, nil
		})
	}
	if req.Operation == "status" {
		return b.Ready(ctx), nil
	}
	ready := b.Ready(ctx)
	if !ready.Available {
		return ready, nil
	}
	base := Result{Status: "observed", Available: true, HostID: b.HostID}
	switch req.Operation {
	case "list":
		xs, err := b.List(ctx, req.Project)
		base.Instances = xs
		return base, err
	case "inspect":
		if !validName(req.Project) || !validName(req.Instance) {
			return base, errors.New("project and instance required")
		}
		x, found, err := b.inspect(ctx, req.Project, req.Instance)
		if err != nil {
			return base, err
		}
		if !found {
			return base, errors.New("instance_missing")
		}
		base.Instance = &x
		return base, nil
	case "abandon_create":
		return b.Lock(func() (Result, error) {
			if !approval {
				return base, errors.New("approval_required: abandoning a durably reserved native create requires explicit current Owner authorization")
			}
			if !validName(req.Project) || !validName(req.Instance) || !validName(req.OperationID) || !validName(req.WorkID) {
				return base, errors.New("abandon_create requires exact project, instance, operation_id and work_id")
			}
			journal, err := b.loadIntents()
			if err != nil {
				return base, err
			}
			found := false
			for _, intent := range journal.Intents {
				if intent.OperationID != req.OperationID {
					continue
				}
				found = true
				if intent.HostID != b.HostID || intent.Project != req.Project || intent.Instance != req.Instance || intent.WorkID != req.WorkID {
					return base, errors.New("create_intent_identity_mismatch: cannot abandon another identity")
				}
				if intent.State != "submitted" || intent.NativeUUID != "" {
					return base, errors.New("create_intent_not_pending: confirmed or abandoned intent cannot be retired")
				}
			}
			if !found {
				return base, errors.New("create_intent_missing: no matching pending native create")
			}
			// Both native APIs must be readable and authoritative. The Incus
			// operation list covers all projects; ANY outstanding background
			// operation is conservatively treated as potentially relevant when a
			// native operation ID could not be captured after interrupted launch.
			// Never infer absence merely from timeout or one empty instance list.
			for check := 0; check < 2; check++ {
				operations, err := b.command(ctx, 20*time.Second, "operation", "list", "--all-projects", "--format", "json")
				if err != nil {
					return base, errors.New("create_abandon_outcome_uncertain: Incus outstanding operations could not be enumerated; intent retained")
				}
				var nativeOperations []json.RawMessage
				if json.Unmarshal([]byte(operations), &nativeOperations) != nil || nativeOperations == nil {
					return base, errors.New("create_abandon_outcome_uncertain: native operation list invalid or incomplete; intent retained")
				}
				if len(nativeOperations) != 0 {
					return base, errors.New("create_abandon_outcome_uncertain: native background operations still exist; verify their completion before abandonment")
				}
				all, err := b.List(ctx, "")
				if err != nil {
					return base, errors.New("create_abandon_outcome_uncertain: all-project native guest discovery failed; intent retained")
				}
				for _, guest := range all {
					if (guest.Project == req.Project && guest.Name == req.Instance) || (guest.Managed && guest.OperationID == req.OperationID) {
						return base, errors.New("create_abandon_identity_conflict: a native instance still occupies this creation identity; reconcile it instead")
					}
				}
			}
			abandoned, err := b.abandonIntent(req)
			if err != nil {
				return base, err
			}
			base.Status = "create_intent_abandoned"
			base.Pending = []CreateIntent{abandoned}
			base.Diagnostic = "Abandoned only after explicit approval and two successful Incus all-project instance/operation absence checks. Old operation_id stays retired; use a new stable ID for a subsequent launch. Does not cancel any native operation."
			return base, nil
		})
	case "create":
		return b.Lock(func() (Result, error) {
			if !validName(req.Project) || !validName(req.Instance) || !validName(req.WorkID) || !validName(req.OperationID) || !imagePattern.MatchString(req.Image) {
				return base, errors.New("create requires validated project, instance, image, work_id and stable operation_id")
			}
			if req.Type != "container" && req.Type != "virtual-machine" {
				return base, errors.New("type must be container or virtual-machine")
			}
			if req.CPU < 1 || req.MemoryMiB < 128 || req.DiskGiB < 1 || !validName(req.StoragePool) {
				return base, errors.New("explicit positive cpu/memory_mib/disk_gib and a native storage_pool required")
			}
			// An operation ID is an Agent-host-wide create idempotency key,
			// not merely a name-scoped hint. A reconnect/new chat proposing
			// a different instance name must not duplicate an earlier guest.
			all, err := b.List(ctx, "")
			if err != nil {
				return base, err
			}
			for _, prior := range all {
				if prior.Managed && prior.OperationID == req.OperationID {
					if prior.Project != req.Project || prior.Name != req.Instance || prior.WorkID != req.WorkID || prior.Type != req.Type {
						return base, errors.New("operation_id_collision: existing managed guest already owns this creation id; list and inspect native state before retry")
					}
				}
			}
			x, found, err := b.inspect(ctx, req.Project, req.Instance)
			if err != nil {
				return base, err
			}
			if found {
				if x.Managed && x.OperationID == req.OperationID && x.WorkID == req.WorkID && x.Type == req.Type {
					if x.StoragePool == "" || x.DiskLimit == "" {
						return base, errors.New("native_root_device_unverified: selected pool or root disk limit absent from current Incus instance state")
					}
					if x.StoragePool != req.StoragePool {
						return base, errors.New("instance_storage_pool_mismatch: native root pool no longer matches the creation intent")
					}
					if !uuidPattern.MatchString(x.UUID) {
						return base, errors.New("native_uuid_unverified: Incus guest UUID missing or malformed")
					}
					if _, err := b.reserveCreate(req); err != nil {
						return base, err
					}
					if err := b.confirmCreate(req.OperationID, x.UUID); err != nil {
						return base, err
					}
					base.Instance = &x
					base.Status = "reconciled_existing"
					return base, nil
				}
				return base, errors.New("instance_collision: name exists with different provider identity or operation; inspect before choosing another name")
			}
			if req.Type == "virtual-machine" {
				if _, err := os.Stat("/dev/kvm"); err != nil {
					return base, errors.New("kvm_unavailable: VM requires KVM; choose container only if its isolation is suitable")
				}
			}
			// Capacity is a measured preflight, not an arbitrary fleet/instance cap.
			capacityFn := b.capacityProbe
			if capacityFn == nil {
				capacityFn = func(mem, disk int) error { return capacity(mem) }
			}
			if err := capacityFn(req.MemoryMiB, req.DiskGiB); err != nil {
				return base, err
			}
			if b.capacityProbe == nil {
				if err := b.checkNativePool(ctx, req.StoragePool, req.DiskGiB); err != nil {
					return base, err
				}
			}
			previouslySubmitted, err := b.reserveCreate(req)
			if err != nil {
				return base, err
			}
			if previouslySubmitted {
				return base, errors.New("create_outcome_uncertain: a prior submission exists; native guest is not yet visible. Do not resubmit; inspect Incus operations/guest state")
			}
			args := []string{"launch", req.Image, req.Instance, "--project", req.Project, "--storage", req.StoragePool,
				"-c", "user.asa.host=" + b.HostID, "-c", "user.asa.work=" + req.WorkID,
				"-c", "user.asa.operation=" + req.OperationID, "-c", "limits.cpu=" + strconv.Itoa(req.CPU),
				"-c", "limits.memory=" + strconv.Itoa(req.MemoryMiB) + "MiB",
				"-d", "root,size=" + strconv.Itoa(req.DiskGiB) + "GiB"}
			if req.Type == "virtual-machine" {
				args = append(args, "--vm")
			}
			_, launchErr := b.command(ctx, 4*time.Minute, args...)
			// A launch response alone does not establish identity or completion.
			x, found, inspectErr := b.inspect(ctx, req.Project, req.Instance)
			if inspectErr != nil {
				return base, fmt.Errorf("create_outcome_uncertain: native state could not be reconciled; inspect before retry: %w", errors.Join(launchErr, inspectErr))
			}
			if found && x.Managed && x.OperationID == req.OperationID && x.WorkID == req.WorkID && x.Type == req.Type {
				if x.StoragePool == "" || x.DiskLimit == "" {
					return base, postLaunchFailure("native_root_device_unverified: selected pool or root disk limit missing after launch", launchErr)
				}
				if x.StoragePool != req.StoragePool {
					return base, postLaunchFailure("create_outcome_uncertain: native root storage pool differs from requested pool", launchErr)
				}
				if !uuidPattern.MatchString(x.UUID) {
					return base, postLaunchFailure("native_uuid_unverified: created guest UUID missing or malformed", launchErr)
				}
				if err := b.confirmCreate(req.OperationID, x.UUID); err != nil {
					return base, fmt.Errorf("create_outcome_uncertain: native guest exists but local intent confirmation failed; do not retry: %w", errors.Join(launchErr, err))
				}
				base.Instance = &x
				if launchErr == nil {
					base.Status = "created"
				} else {
					base.Status = "created_but_launch_reported_error"
					var native *NativeCommandError
					if errors.As(launchErr, &native) {
						base.Output = native.Diagnostic
						base.ExitCode = native.ExitCode
						base.OutputBytesSeen = native.BytesSeen
						base.OutputOmittedBytes = native.OmittedBytes
						base.OutputTruncated = native.Truncated
						base.TimedOut = native.TimedOut
					}
				}
				return base, nil
			}
			if found {
				return base, postLaunchFailure("create_collision_after_launch: unexpected guest identity", launchErr)
			}
			if launchErr != nil {
				return base, fmt.Errorf("create_outcome_uncertain: launch reported an error and no native guest is yet visible; submission remains durably reserved for reconciliation: %w", launchErr)
			}
			return base, errors.New("create_unverified: Incus reported success but instance not found")
		})
	case "start", "stop", "exec", "snapshots", "snapshot", "restore", "delete":
		return b.Lock(func() (Result, error) {
			x, err := b.selectInstance(ctx, req)
			if err != nil {
				return base, err
			}
			base.Instance = &x
			if req.Operation == "snapshot" || req.Operation == "restore" {
				if !validName(req.Snapshot) {
					return base, errors.New("snapshot name required")
				}
				if req.Operation == "restore" && !approval {
					return base, errors.New("approval_required: destructive guest restore requires explicit current Owner approval")
				}
				return b.submitNativeLocked(ctx, req, x)
			}
			if req.Operation != "snapshots" {
				if err := b.nativeConflict(req); err != nil {
					return base, err
				}
			}
			if req.Operation == "snapshots" {
				list, err := b.Snapshots(ctx, req.Project, req.Instance)
				base.Snapshots = list
				if err == nil {
					base.Status = "snapshots_observed"
				}
				return base, err
			}

			if req.Operation == "delete" && !approval {
				return base, errors.New("approval_required: destructive guest operation requires explicit current Owner approval")
			}
			args := []string{}
			switch req.Operation {
			case "start":
				args = []string{"start", req.Instance, "--project", req.Project}
			case "stop":
				args = []string{"stop", req.Instance, "--project", req.Project}
			case "delete":
				if x.Status != "Stopped" {
					return base, errors.New("stop and inspect guest before deletion (no implicit force)")
				}
				args = []string{"delete", req.Instance, "--project", req.Project}
			case "exec":
				if len(req.Command) == 0 || len(req.Command) > 64 {
					return base, errors.New("guest command argv required (1..64)")
				}
				for _, s := range req.Command {
					if s == "" || len(s) > 4096 || strings.IndexByte(s, 0) >= 0 {
						return base, errors.New("invalid guest argv")
					}
				}
				args = []string{"exec", req.Instance, "--project", req.Project, "--mode", "non-interactive", "--"}
				args = append(args, req.Command...)
			}
			timeout := time.Minute
			// Snapshot/restore are handled by the separate nonblocking
			// native API path. Guest exec retains its bounded four-minute budget.
			if req.Operation == "exec" {
				timeout = 4 * time.Minute
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			// Serialize Agent mutations and repeat native UUID/management checks
			// immediately before the name-based CLI request. This closes
			// Agent-to-Agent races and catches external replacements that have
			// already occurred; Incus CLI offers no UUID-conditional mutation for
			// these subcommands, so the final external race cannot be claimed
			// atomic. Post-flight identity verification fails closed.
			if b.beforeMutation != nil {
				b.beforeMutation(req)
			}
			latest, err := b.selectInstance(ctx, req)
			if err != nil {
				// A confirmed missing/replaced native guest is different from
				// a list/JSON/journal probe that could not verify identity.
				// Neither outcome authorizes a name-based operation.
				if verifiedNativeIdentityChange(err) {
					return base, fmt.Errorf("instance_identity_changed_before_mutation: refused name-based operation after verified identity mismatch: %w", err)
				}
				return base, fmt.Errorf("native_identity_preflight_unavailable: native guest identity could not be verified before operation; no command dispatched: %w", err)
			}
			if latest.UUID != x.UUID {
				return base, errors.New("instance_identity_changed_before_mutation")
			}
			var input io.Reader
			if secret != "" {
				input = strings.NewReader(secret + "\n")
			}
			// Mark only the guest-exec CLI boundary; native discovery and
			// authorization failures above it must preserve their own evidence.
			base.GuestExecAttempted = req.Operation == "exec"
			out, code, trunc, seen, runErr := b.cliProtected(runCtx, input, false, secret, args...)
			base.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
			base.Output = out
			base.ExitCode = code
			base.OutputTruncated = trunc
			base.OutputBytesSeen = seen
			if runErr != nil {
				if req.Operation == "exec" && x.Type == "virtual-machine" && strings.Contains(strings.ToLower(out), "agent") {
					return base, errors.New("guest_agent_unavailable: Incus VM guest agent is required for native exec")
				}
				return base, fmt.Errorf("native operation failed, exit_code=%d: %w", code, runErr)
			}
			if req.Operation == "exec" {
				verified, verifyErr := b.selectInstance(ctx, req)
				if verifyErr != nil || verified.UUID != x.UUID {
					// Guest effects may have occurred, but the output belongs
					// only to a verified UUID. Withhold all guest output and
					// its presentation counters; retain only the independent
					// native identity-probe error as a diagnostic channel.
					base.Output = ""
					base.OutputBytesSeen = 0
					base.OutputOmittedBytes = 0
					base.OutputTruncated = false
					if verifyErr != nil {
						if verifiedNativeIdentityChange(verifyErr) {
							return base, fmt.Errorf("guest_exec_outcome_uncertain: native guest identity changed during execution; guest output withheld; inspect before retry: %w", verifyErr)
						}
						return base, fmt.Errorf("guest_exec_outcome_uncertain: post-execution native identity could not be verified; guest output withheld; inspect before retry: %w", verifyErr)
					}
					return base, errors.New("guest_exec_outcome_uncertain: native guest identity changed during execution; no output delivered")
				}
				base.Status = "guest_command_completed"
				return base, nil
			}
			if req.Operation == "delete" {
				_, exists, err := b.inspect(ctx, req.Project, req.Instance)
				if err != nil {
					return base, errors.New("delete_outcome_uncertain: inspect Incus state")
				}
				if exists {
					return base, errors.New("delete_unverified: instance still exists")
				}
				if err := b.retireDeleted(req.Project, req.Instance, x.OperationID, x.UUID); err != nil {
					return base, errors.New("delete_outcome_uncertain: native absence verified but local operation intent cleanup failed")
				}
				base.Instance = nil
				base.Status = "deleted"
				return base, nil
			}
			after, exists, err := b.inspect(ctx, req.Project, req.Instance)
			if err != nil || !exists {
				return base, errors.New("operation_outcome_uncertain: inspect current native state")
			}
			if after.UUID != x.UUID {
				return base, errors.New("instance_identity_changed: rediscover before further operations")
			}
			if err := b.checkConfirmedIdentity(after); err != nil {
				return base, errors.New("operation_outcome_uncertain: native guest no longer matches its durable journal identity")
			}
			base.Instance = &after
			base.Status = "completed_native"
			return base, nil
		})
	default:
		return base, errors.New("unsupported managed environment operation")
	}
}
func capacity(memoryMiB int) error {
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					n, _ := strconv.ParseInt(f[1], 10, 64)
					if n > 0 && n/1024 < int64(memoryMiB)+256 {
						return errors.New("host_capacity_memory_insufficient: use a smaller authorized guest or free host memory")
					}
				}
				break
			}
		}
	}
	return nil
}

// The only storage free-space preflight comes from the chosen native pool,
// not from statfs(/var/lib/incus) which may be unrelated to ZFS/LVM/Ceph.
// A zero/unknown capacity cannot be truthfully reported as sufficient.
func (b Backend) checkNativePool(ctx context.Context, pool string, diskGiB int) error {
	if !validName(pool) {
		return errors.New("storage_pool_required")
	}
	path := "/1.0/storage-pools/" + url.PathEscape(pool) + "/resources"
	raw, err := b.command(ctx, 20*time.Second, "query", path)
	if err != nil {
		return fmt.Errorf("incus_storage_unavailable: selected pool resources cannot be read; inspect Incus storage pool and driver: %w", err)
	}
	var data struct {
		Space struct {
			Total uint64 `json:"total"`
			Used  uint64 `json:"used"`
		} `json:"space"`
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil || data.Space.Total == 0 || data.Space.Used > data.Space.Total {
		return errors.New("incus_storage_capacity_unknown: storage pool resources did not report usable total/used space")
	}
	available := data.Space.Total - data.Space.Used
	required := uint64(diskGiB) * (1 << 30)
	if available < required {
		return fmt.Errorf("incus_storage_capacity_insufficient: selected pool %s has %d free bytes, requested root limit %d bytes", pool, available, required)
	}
	return nil
}
