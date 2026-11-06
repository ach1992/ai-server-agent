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
	"io"
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
	CPU         int      `json:"cpu,omitempty"`
	MemoryMiB   int      `json:"memory_mib,omitempty"`
	DiskGiB     int      `json:"disk_gib,omitempty"`
	Command     []string `json:"command,omitempty"`
	Snapshot    string   `json:"snapshot,omitempty"`
}
type Instance struct {
	Name    string                       `json:"name"`
	Project string                       `json:"project,omitempty"`
	Type    string                       `json:"type,omitempty"`
	Status  string                       `json:"status,omitempty"`
	Config  map[string]string            `json:"config,omitempty"`
	Devices map[string]map[string]string `json:"devices,omitempty"`
	State   *struct {
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
	CPU         string   `json:"cpu,omitempty"`
	Memory      string   `json:"memory,omitempty"`
	Addresses   []string `json:"addresses,omitempty"`
	Managed     bool     `json:"managed"`
}
type Result struct {
	Status          string    `json:"status"`
	HostID          string    `json:"host_id"`
	Available       bool      `json:"available"`
	Diagnostic      string    `json:"diagnostic,omitempty"`
	Instances       []Summary `json:"instances,omitempty"`
	Instance        *Summary  `json:"instance,omitempty"`
	Output          string    `json:"output,omitempty"`
	ExitCode        int       `json:"exit_code,omitempty"`
	Snapshots       []string  `json:"snapshots,omitempty"`
	OutputTruncated bool      `json:"output_truncated,omitempty"`
}
type Backend struct {
	HostID        string
	Binary        string               // absolute trusted CLI; test fixture overrides only in tests
	StateDir      string               // lock directory already owned by root Executor
	capacityProbe func(int, int) error // test-only; production always checks actual host capacity
}

const maxOutput = 64 << 10

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}$`)
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
	bin, err := b.binary()
	if err != nil {
		return "", -1, false, err
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"--force-local"}, args...)...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LC_ALL=C.UTF-8", "LANG=C.UTF-8"}
	cmd.Stdin = stdin
	var out bounded
	if discardOutput {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	} else {
		cmd.Stdout = &out
		cmd.Stderr = &out
	}
	err = cmd.Run()
	code := 0
	if err != nil {
		code = -1
		var e *exec.ExitError
		if errors.As(err, &e) {
			code = e.ExitCode()
		}
	}
	if discardOutput {
		return "", code, false, err
	}
	return out.b.String(), code, out.seen > maxOutput, err
}
func (b Backend) command(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, _, truncated, err := b.cli(runCtx, nil, false, args...)
	if truncated {
		return "", errors.New("incus output exceeded bounded response")
	}
	if err != nil {
		return "", fmt.Errorf("native Incus operation failed (%s): %w", strings.Join(args[:min(2, len(args))], " "), err)
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
	result := make([]Summary, 0, len(xs))
	for _, x := range xs {
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
	if !validName(req.Project) || !validName(req.Instance) || req.UUID == "" {
		return Summary{}, errors.New("project, instance and verified native UUID required")
	}
	native, found, err := b.inspect(ctx, req.Project, req.Instance)
	if err != nil {
		return Summary{}, err
	}
	if !found {
		return Summary{}, errors.New("instance_missing: rediscover native Incus state before retry")
	}
	if !native.Managed || native.UUID != req.UUID {
		return Summary{}, errors.New("instance_identity_mismatch: host/provider/UUID does not match this Agent-managed guest")
	}
	return native, nil
}
func (b Backend) Lock(fn func() (Result, error)) (Result, error) {
	// Scoped OS lock serializes create/reconcile on this Agent host. It is NOT
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
	if _, err := b.command(ctx, 12*time.Second, "info", "--format", "json"); err != nil {
		r.Diagnostic = "incus_unready: native daemon/socket/project not initialized or unavailable; inspect incus admin status and initialization"
		return r
	}
	r.Available = true
	r.Status = "ready"
	if _, err := os.Stat("/dev/kvm"); err != nil {
		r.Diagnostic = "KVM unavailable for VMs; containers may still work; verify /dev/kvm and host virtualization"
	}
	return r
}
func (b Backend) Run(ctx context.Context, req Request, approval bool, secret string) (Result, error) {
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
	case "create":
		return b.Lock(func() (Result, error) {
			if !validName(req.Project) || !validName(req.Instance) || !validName(req.WorkID) || !validName(req.OperationID) || !imagePattern.MatchString(req.Image) {
				return base, errors.New("create requires validated project, instance, image, work_id and stable operation_id")
			}
			if req.Type != "container" && req.Type != "virtual-machine" {
				return base, errors.New("type must be container or virtual-machine")
			}
			if req.CPU < 1 || req.MemoryMiB < 128 || req.DiskGiB < 1 {
				return base, errors.New("explicit positive cpu/memory_mib/disk_gib required")
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
				capacityFn = func(mem, disk int) error { return capacity(mem, disk, b.StateDir) }
			}
			if err := capacityFn(req.MemoryMiB, req.DiskGiB); err != nil {
				return base, err
			}
			args := []string{"launch", req.Image, req.Instance, "--project", req.Project,
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
				return base, errors.New("create_outcome_uncertain: native state could not be reconciled; inspect before retry")
			}
			if found && x.Managed && x.OperationID == req.OperationID && x.WorkID == req.WorkID && x.Type == req.Type {
				base.Instance = &x
				if launchErr == nil {
					base.Status = "created"
				} else {
					base.Status = "created_but_launch_reported_error"
				}
				return base, nil
			}
			if found {
				return base, errors.New("create_collision_after_launch: unexpected guest identity")
			}
			if launchErr != nil {
				return base, fmt.Errorf("launch_failed_no_instance: %w", launchErr)
			}
			return base, errors.New("create_unverified: Incus reported success but instance not found")
		})
	case "start", "stop", "exec", "snapshots", "snapshot", "restore", "delete":
		x, err := b.selectInstance(ctx, req)
		if err != nil {
			return base, err
		}
		base.Instance = &x
		if req.Operation == "snapshots" {
			list, err := b.Snapshots(ctx, req.Project, req.Instance)
			base.Snapshots = list
			if err == nil {
				base.Status = "snapshots_observed"
			}
			return base, err
		}
		if req.Operation == "snapshot" || req.Operation == "restore" {
			if !validName(req.Snapshot) {
				return base, errors.New("snapshot name required")
			}
			list, err := b.Snapshots(ctx, req.Project, req.Instance)
			if err != nil {
				return base, err
			}
			if req.Operation == "snapshot" && hasSnapshot(list, req.Snapshot) {
				return base, errors.New("snapshot_collision: snapshot name already exists")
			}
			if req.Operation == "restore" && !hasSnapshot(list, req.Snapshot) {
				return base, errors.New("snapshot_missing: restore target does not exist")
			}
		}
		if (req.Operation == "delete" || req.Operation == "restore") && !approval {
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
		case "snapshot":
			if !validName(req.Snapshot) {
				return base, errors.New("snapshot name required")
			}
			args = []string{"snapshot", "create", req.Instance, req.Snapshot, "--project", req.Project}
		case "restore":
			if !validName(req.Snapshot) {
				return base, errors.New("snapshot name required")
			}
			args = []string{"snapshot", "restore", req.Instance, req.Snapshot, "--project", req.Project}
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
		if req.Operation == "exec" {
			timeout = 4 * time.Minute
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var input io.Reader
		if secret != "" {
			input = strings.NewReader(secret + "\n")
		}
		out, code, trunc, runErr := b.cli(runCtx, input, secret != "", args...)
		base.Output = out
		base.ExitCode = code
		base.OutputTruncated = trunc
		if runErr != nil {
			if req.Operation == "exec" && x.Type == "virtual-machine" && strings.Contains(strings.ToLower(out), "agent") {
				return base, errors.New("guest_agent_unavailable: Incus VM guest agent is required for native exec")
			}
			return base, fmt.Errorf("native operation failed, exit_code=%d: %w", code, runErr)
		}
		if req.Operation == "exec" {
			base.Status = "guest_command_completed"
			return base, nil
		}
		if req.Operation == "snapshot" {
			list, err := b.Snapshots(ctx, req.Project, req.Instance)
			if err != nil || !hasSnapshot(list, req.Snapshot) {
				return base, errors.New("snapshot_outcome_uncertain: snapshot not verified in Incus")
			}
			base.Snapshots = list
			base.Status = "snapshot_created"
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
			base.Instance = nil
			base.Status = "deleted"
			return base, nil
		}
		after, exists, err := b.inspect(ctx, req.Project, req.Instance)
		if err != nil || !exists {
			return base, errors.New("operation_outcome_uncertain: inspect current native state")
		}
		if after.UUID != x.UUID || !after.Managed {
			return base, errors.New("instance_identity_changed: rediscover before further operations")
		}
		base.Instance = &after
		base.Status = "completed_native"
		return base, nil
	default:
		return base, errors.New("unsupported managed environment operation")
	}
}
func capacity(memoryMiB, diskGiB int, stateDir string) error {
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
	diskPath := "/var/lib/incus"
	if _, err := os.Stat(diskPath); err != nil {
		diskPath = stateDir
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &st); err == nil {
		free := st.Bavail * uint64(st.Bsize)
		if free < uint64(diskGiB+2)*(1<<30) {
			return errors.New("host_capacity_disk_insufficient: inspect storage pool and real host free space")
		}
	}
	return nil
}
