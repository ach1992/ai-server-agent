package managedincus

// Durable, nonblocking native Incus snapshot/restore operations. Incus owns the
// guest and native operation; this small root-only journal owns the submission
// identity and the last independently observed native terminal result.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxNativeOperationRecords = 256
const nativeOperationRetention = 30 * 24 * time.Hour

type NativeOperation struct {
	HostID         string    `json:"host_id"`
	WorkID         string    `json:"work_id"`
	ActorID        string    `json:"actor_id,omitempty"` // non-secret authenticated MCP principal provenance
	ID             string    `json:"id"`
	Project        string    `json:"project"`
	Instance       string    `json:"instance"`
	UUID           string    `json:"uuid"`
	Action         string    `json:"action"`
	Snapshot       string    `json:"snapshot"`
	NativeID       string    `json:"native_id,omitempty"`
	State          string    `json:"state"`
	NativeTerminal string    `json:"native_terminal,omitempty"` // independently observed result: success or failure
	Diagnostic     string    `json:"diagnostic,omitempty"`
	SubmittedAt    time.Time `json:"submitted_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
type nativeOperationJournal struct {
	Version    int               `json:"version"`
	Operations []NativeOperation `json:"operations"`
}
type nativeAPIResponse struct {
	Status int
	ETag   string
	Body   []byte
}

// apiOverride is only used by isolated unit fixtures. Production uses a
// root-verified local Incus UNIX socket, never an arbitrary supplied URL.
type nativeAPIFunc func(context.Context, string, string, []byte, string) (nativeAPIResponse, error)

func (b Backend) nativeOperationsPath() string {
	return filepath.Join(b.StateDir, "managed-incus", "native-operations.json")
}
func (b Backend) loadNativeOperations() (nativeOperationJournal, error) {
	d := nativeOperationJournal{Version: 1}
	path := b.nativeOperationsPath()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return d, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || sys.Nlink != 1 || int(sys.Uid) != os.Geteuid() {
		return d, errors.New("unsafe native Incus operation journal")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return d, err
	}
	if len(data) > 1<<20 {
		return d, errors.New("native Incus operation journal exceeds size limit")
	}
	if err = json.Unmarshal(data, &d); err != nil || d.Version != 1 {
		return d, errors.New("corrupt/unsupported native Incus operation journal")
	}
	if len(d.Operations) > maxNativeOperationRecords {
		return d, errors.New("native Incus operation journal exceeds record limit")
	}
	seen := map[string]bool{}
	for _, v := range d.Operations {
		if v.HostID != b.HostID || (len(v.ActorID) > 160 || strings.IndexFunc(v.ActorID, func(r rune) bool { return r < 32 || r == 127 }) >= 0) || !validName(v.WorkID) || !validName(v.ID) || !validName(v.Project) || !validName(v.Instance) || !uuidPattern.MatchString(v.UUID) || !validName(v.Snapshot) || (v.Action != "snapshot" && v.Action != "restore") || seen[v.ID] || v.SubmittedAt.IsZero() || v.UpdatedAt.IsZero() || (v.NativeID != "" && !uuidPattern.MatchString(v.NativeID)) {
			return d, errors.New("invalid native Incus operation record identity")
		}
		switch v.State {
		case "submitted", "running", "native_succeeded", "native_failed", "outcome_uncertain", "closed_unverified":
		default:
			return d, errors.New("invalid native Incus operation state")
		}
		if v.State == "running" && v.NativeID == "" {
			return d, errors.New("running Incus record lacks native operation ID")
		}
		if v.NativeTerminal != "" && v.NativeTerminal != "success" && v.NativeTerminal != "failure" {
			return d, errors.New("invalid native terminal evidence")
		}
		seen[v.ID] = true
	}
	return d, nil
}
func nativeTerminal(state string) bool {
	return state == "native_succeeded" || state == "native_failed" || state == "closed_unverified"
}
func (b Backend) saveNativeOperations(d nativeOperationJournal) error {
	if len(d.Operations) > maxNativeOperationRecords {
		return errors.New("native Incus operation journal full; retain unresolved operations and reconcile old results before submitting more")
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("native Incus operation journal exceeds size limit")
	}
	var id [12]byte
	if _, err = rand.Read(id[:]); err != nil {
		return err
	}
	dir := filepath.Dir(b.nativeOperationsPath())
	temp := filepath.Join(dir, ".native-"+hex.EncodeToString(id[:]))
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp, b.nativeOperationsPath()); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
func pruneNativeOperations(d *nativeOperationJournal) {
	cutoff := time.Now().UTC().Add(-nativeOperationRetention)
	out := d.Operations[:0]
	for _, v := range d.Operations {
		if nativeTerminal(v.State) && v.UpdatedAt.Before(cutoff) {
			continue
		}
		out = append(out, v)
	}
	d.Operations = out
}
func nativeOperationMatches(x NativeOperation, req Request, host string) bool {
	return x.HostID == host && x.Project == req.Project && x.Instance == req.Instance && x.UUID == req.UUID && x.Action == req.Operation && x.Snapshot == req.Snapshot && (x.ActorID == "" || x.ActorID == req.ActorID)
}
func (b Backend) nativeConflict(req Request) error {
	d, err := b.loadNativeOperations()
	if err != nil {
		return err
	}
	for _, v := range d.Operations {
		if v.Project == req.Project && v.Instance == req.Instance && v.UUID == req.UUID && !nativeTerminal(v.State) {
			return fmt.Errorf("native_operation_in_flight: %s (%s); query operation_status and reconcile before another guest mutation", v.ID, v.State)
		}
	}
	return nil
}
func (b Backend) nativeRequest(ctx context.Context, method, route string, body []byte, etag string) (nativeAPIResponse, error) {
	if b.apiOverride != nil {
		return b.apiOverride(ctx, method, route, body, etag)
	}
	var socket string
	for _, candidate := range []string{"/var/lib/incus/unix.socket", "/run/incus/unix.socket"} {
		st, err := os.Lstat(candidate)
		if err != nil {
			continue
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if ok && st.Mode()&os.ModeSocket != 0 && sys.Uid == 0 && st.Mode().Perm()&0002 == 0 {
			socket = candidate
			break
		}
	}
	if socket == "" {
		return nativeAPIResponse{}, errors.New("incus_socket_unavailable: trusted local Incus UNIX socket not found")
	}
	if !strings.HasPrefix(route, "/1.0/") || strings.Contains(route, "#") {
		return nativeAPIResponse{}, errors.New("invalid native Incus API route")
	}
	conn := &net.Dialer{Timeout: 4 * time.Second}
	transport := &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) { return conn.DialContext(c, "unix", socket) }, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second}
	request, err := http.NewRequestWithContext(ctx, method, "http://incus.socket"+route, bytes.NewReader(body))
	if err != nil {
		return nativeAPIResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	resp, err := client.Do(request)
	if err != nil {
		return nativeAPIResponse{}, fmt.Errorf("native Incus API acknowledgement uncertain: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOutput+1))
	if err != nil || len(data) > maxOutput {
		return nativeAPIResponse{}, errors.New("native Incus API response truncated or unreadable")
	}
	out := nativeAPIResponse{Status: resp.StatusCode, ETag: resp.Header.Get("ETag"), Body: data}
	if resp.StatusCode >= 400 {
		return out, fmt.Errorf("incus_api_http_%d: native request rejected", resp.StatusCode)
	}
	return out, nil
}
func nativeRoute(req Request) string {
	return "/1.0/instances/" + url.PathEscape(req.Instance) + "?project=" + url.QueryEscape(req.Project)
}
func acceptedNativeID(resp nativeAPIResponse) (string, error) {
	if resp.Status != 202 {
		return "", errors.New("native Incus mutation did not return HTTP 202")
	}
	var outer struct {
		Type      string `json:"type"`
		Operation string `json:"operation"`
		Metadata  struct {
			ID         string `json:"id"`
			StatusCode int    `json:"status_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal(resp.Body, &outer) != nil || outer.Type != "async" || !uuidPattern.MatchString(outer.Metadata.ID) || outer.Operation != "/1.0/operations/"+outer.Metadata.ID {
		return "", errors.New("native Incus asynchronous acknowledgement malformed")
	}
	return outer.Metadata.ID, nil
}

// prepareNativeRestore never mutates Incus. A failed read/ETag preflight must
// not create an unresolved journal entry or require manual retirement.
func (b Backend) prepareNativeRestore(ctx context.Context, req Request) ([]byte, string, error) {
	current, err := b.nativeRequest(ctx, http.MethodGet, nativeRoute(req), nil, "")
	if err != nil {
		return nil, "", fmt.Errorf("native_restore_preflight_unavailable: %w", err)
	}
	if current.Status != 200 || current.ETag == "" {
		return nil, "", errors.New("native_restore_etag_unavailable: refuse blind PUT")
	}
	var envelope struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(current.Body, &envelope) != nil || len(envelope.Metadata) == 0 {
		return nil, "", errors.New("native_restore_preflight_invalid: metadata missing")
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(envelope.Metadata, &props) != nil {
		return nil, "", errors.New("native_restore_preflight_invalid: metadata invalid")
	}
	var metadata struct {
		Config map[string]string `json:"config"`
	}
	if json.Unmarshal(envelope.Metadata, &metadata) != nil || metadata.Config["volatile.uuid"] != req.UUID ||
		metadata.Config["user.asa.host"] != b.HostID {
		return nil, "", errors.New("native_restore_identity_mismatch: guest UUID/host changed before PUT")
	}
	// Unlike PATCH, a PUT replaces all writable properties. Require the actual
	// native instance metadata and preserve every supported writable property.
	for _, key := range []string{"config", "devices", "profiles", "architecture", "ephemeral", "stateful", "description"} {
		if props[key] == nil {
			return nil, "", errors.New("native_restore_configuration_unverified: cannot preserve instance PUT properties")
		}
	}
	writable := map[string]json.RawMessage{}
	for _, key := range []string{"architecture", "config", "devices", "ephemeral", "profiles", "stateful", "description"} {
		if props[key] != nil {
			writable[key] = props[key]
		}
	}
	snap, _ := json.Marshal(req.Snapshot)
	writable["restore"] = snap
	payload, err := json.Marshal(writable)
	if err != nil {
		return nil, "", errors.New("native_restore_configuration_unverified: cannot serialize PUT")
	}
	return payload, current.ETag, nil
}

func (b Backend) submitNativeLocked(ctx context.Context, req Request, x Summary) (Result, error) {
	result := Result{Status: "submitted", Available: true, HostID: b.HostID, Instance: &x}
	if !validName(req.OperationID) {
		return result, errors.New("operation_id_required: supply stable operation_id for nonblocking snapshot/restore")
	}
	journal, err := b.loadNativeOperations()
	if err != nil {
		return result, err
	}
	for _, prior := range journal.Operations {
		if prior.ID != req.OperationID {
			continue
		}
		if !nativeOperationMatches(prior, req, b.HostID) {
			return result, errors.New("native_operation_id_collision: operation_id already belongs to another native identity")
		}
		result.Operation = &prior
		result.Status = prior.State
		return result, nil // Exact replay, NEVER a second native mutation.
	}
	list, snapshotErr := b.Snapshots(ctx, req.Project, req.Instance)
	if snapshotErr != nil {
		return result, snapshotErr
	}
	if req.Operation == "snapshot" && hasSnapshot(list, req.Snapshot) {
		return result, errors.New("snapshot_collision: native snapshot already exists")
	}
	if req.Operation == "restore" && !hasSnapshot(list, req.Snapshot) {
		return result, errors.New("snapshot_missing: restore target absent")
	}
	if err = b.nativeConflict(req); err != nil {
		return result, err
	}
	if b.beforeMutation != nil {
		b.beforeMutation(req)
	}
	latest, err := b.selectInstance(ctx, req)
	if err != nil || latest.UUID != x.UUID {
		return result, errors.New("instance_identity_changed_before_mutation: refusing native submission")
	}
	if req.Operation == "restore" && latest.Status != "Stopped" {
		return result, errors.New("restore_requires_stopped_guest: stop and inspect VM before restore")
	}
	var restorePayload []byte
	var restoreETag string
	if req.Operation == "restore" {
		restorePayload, restoreETag, err = b.prepareNativeRestore(ctx, req)
		if err != nil {
			return result, err
		}
	}
	entry := NativeOperation{HostID: b.HostID, WorkID: latest.WorkID, ActorID: req.ActorID, ID: req.OperationID, Project: req.Project, Instance: req.Instance, UUID: req.UUID, Action: req.Operation, Snapshot: req.Snapshot, State: "submitted", SubmittedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	pruneNativeOperations(&journal)
	journal.Operations = append(journal.Operations, entry)
	if err = b.saveNativeOperations(journal); err != nil {
		return result, err
	} // MUST persist before mutation.
	result.Operation = &entry
	var answer nativeAPIResponse
	if req.Operation == "snapshot" {
		payload, _ := json.Marshal(struct {
			Name     string `json:"name"`
			Stateful bool   `json:"stateful"`
		}{req.Snapshot, false})
		route := "/1.0/instances/" + url.PathEscape(req.Instance) + "/snapshots?project=" + url.QueryEscape(req.Project)
		answer, err = b.nativeRequest(ctx, http.MethodPost, route, payload, "")
	} else {
		answer, err = b.nativeRequest(ctx, http.MethodPut, nativeRoute(req), restorePayload, restoreETag)
	}
	if err != nil {
		// An authoritative HTTP rejection is not an ambiguous lost acknowledgement.
		// Do not strand guest mutations if the daemon explicitly refused a request.
		switch answer.Status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusConflict, http.StatusPreconditionFailed:
			entry.State = "native_failed"
			entry.Diagnostic = fmt.Sprintf("Incus rejected native submission (HTTP %d); no async operation accepted. Correct the preflight before a NEW operation_id.", answer.Status)
		default:
			entry.State = "outcome_uncertain"
			entry.Diagnostic = "Native submission/acknowledgement unavailable; inspect operation_status and native Incus operations before any retry"
		}
	} else if id, valid := acceptedNativeID(answer); valid != nil {
		entry.State = "outcome_uncertain"
		entry.Diagnostic = "Native submission acknowledgement malformed; reconciliation required"
	} else {
		entry.NativeID = id
		entry.State = "running"
	}
	entry.UpdatedAt = time.Now().UTC()
	journal.Operations[len(journal.Operations)-1] = entry
	if persistErr := b.saveNativeOperations(journal); persistErr != nil {
		return result, fmt.Errorf("native_operation_outcome_uncertain: native submission may have started but acknowledgement could not be durably recorded: %w", persistErr)
	}
	result.Operation = &entry
	result.Status = entry.State
	if err != nil || entry.State == "outcome_uncertain" {
		result.Diagnostic = entry.Diagnostic
		return result, nil
	}
	return result, nil
}
func operationMetadata(data []byte) (id string, code int, project string, err error) {
	var wrapper struct {
		Type     string `json:"type"`
		Metadata struct {
			ID         string `json:"id"`
			StatusCode int    `json:"status_code"`
			Project    string `json:"project"`
		} `json:"metadata"`
	}
	if json.Unmarshal(data, &wrapper) != nil || wrapper.Type != "sync" || !uuidPattern.MatchString(wrapper.Metadata.ID) {
		return "", 0, "", errors.New("native operation metadata invalid")
	}
	return wrapper.Metadata.ID, wrapper.Metadata.StatusCode, wrapper.Metadata.Project, nil
}

// nativeResourceMatches requires the exact resource type, instance/snapshot path
// and project query from Incus's operation metadata. Description text alone is
// never sufficient to reattach an operation after a lost HTTP acknowledgement.
func nativeResourceMatches(paths []string, targetPath, project string) bool {
	// A multi-resource operation could target other guests as well. Incus
	// snapshot/restore is per-instance; refuse non-unique native resources.
	if len(paths) != 1 {
		return false
	}
	for _, raw := range paths {
		ref, err := url.Parse(raw)
		if err != nil || ref.IsAbs() || ref.Host != "" || ref.Fragment != "" || ref.Path != targetPath {
			continue
		}
		query, err := url.ParseQuery(ref.RawQuery)
		if err != nil {
			continue
		}
		projects, hasProject := query["project"]
		delete(query, "project")
		if len(query) != 0 {
			continue
		}
		if hasProject && len(projects) == 1 && projects[0] == project {
			return true
		}
		if !hasProject && project == "default" {
			return true // Native default-project resource URLs may omit ?project.
		}
	}
	return false
}

func nativeOperationCorrelates(op struct {
	ID          string              `json:"id"`
	Description string              `json:"description"`
	Project     string              `json:"project"`
	CreatedAt   time.Time           `json:"created_at"`
	StatusCode  int                 `json:"status_code"`
	Resources   map[string][]string `json:"resources"`
}, v NativeOperation) bool {
	if !uuidPattern.MatchString(op.ID) || (op.Project != "" && op.Project != v.Project) ||
		op.CreatedAt.IsZero() || op.CreatedAt.Before(v.SubmittedAt) ||
		op.CreatedAt.After(time.Now().UTC().Add(10*time.Second)) {
		return false
	}
	switch v.Action {
	case "snapshot":
		if op.Description != "Snapshotting instance" {
			return false
		}
	case "restore":
		if op.Description != "Restoring snapshot" {
			return false
		}
	default:
		return false
	}
	instancePath := "/1.0/instances/" + url.PathEscape(v.Instance)
	snapshotPath := instancePath + "/snapshots/" + url.PathEscape(v.Snapshot)
	// Real Incus 6.0.4 lists operations under metadata.running and includes
	// both resource kinds with an explicit ?project=... for nondefault projects.
	// A restore whose native metadata does not identify the precise snapshot
	// cannot be proven and MUST remain outcome_uncertain.
	return nativeResourceMatches(op.Resources["instances"], instancePath, v.Project) &&
		nativeResourceMatches(op.Resources["instances_snapshots"], snapshotPath, v.Project)
}

func (b Backend) reconcileLostAck(ctx context.Context, v *NativeOperation) {
	// The Incus 6.0.4 operation-list envelope groups records by state
	// (metadata.running, metadata.pending, ...), NOT a flat metadata array.
	// Never adopt merely by description/snapshot name; preserve uncertainty if
	// no uniquely and independently correlated native operation is visible.
	response, err := b.nativeRequest(ctx, http.MethodGet, "/1.0/operations?recursion=1&project="+url.QueryEscape(v.Project), nil, "")
	if err != nil || response.Status != http.StatusOK {
		return
	}
	var envelope struct {
		Type     string                       `json:"type"`
		Metadata map[string][]json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(response.Body, &envelope) != nil || envelope.Type != "sync" || envelope.Metadata == nil {
		return
	}
	type candidate struct {
		ID          string              `json:"id"`
		Description string              `json:"description"`
		Project     string              `json:"project"`
		CreatedAt   time.Time           `json:"created_at"`
		StatusCode  int                 `json:"status_code"`
		Resources   map[string][]string `json:"resources"`
	}
	var matched candidate
	count := 0
	for _, group := range envelope.Metadata {
		for _, item := range group {
			var op candidate
			if json.Unmarshal(item, &op) != nil || !nativeOperationCorrelates(op, *v) {
				continue
			}
			if (op.StatusCode < 100 || op.StatusCode >= 200) &&
				op.StatusCode != 200 && op.StatusCode != 400 && op.StatusCode != 401 {
				continue
			}
			matched = op
			count++
		}
	}
	if count != 1 {
		return
	}
	v.NativeID = matched.ID
	v.Diagnostic = ""
	switch matched.StatusCode {
	case 200:
		// Strictly matched list metadata is a native terminal observation.
		// nativeStatusLocked still rechecks guest identity and snapshot state.
		v.NativeTerminal = "success"
		v.State = "native_succeeded"
	case 400, 401:
		v.NativeTerminal = "failure"
		v.State = "native_failed"
	default:
		v.State = "running"
	}
}

func (b Backend) nativeStatusLocked(ctx context.Context, req Request) (Result, error) {
	base := Result{Status: "operation_not_found", Available: true, HostID: b.HostID}
	if !validName(req.OperationID) || !validName(req.Project) || !validName(req.Instance) || !uuidPattern.MatchString(req.UUID) {
		return base, errors.New("operation_status requires stable operation_id and project/instance/native UUID")
	}
	journal, err := b.loadNativeOperations()
	if err != nil {
		return base, err
	}
	i := -1
	for j, v := range journal.Operations {
		if v.ID == req.OperationID {
			i = j
			break
		}
	}
	if i < 0 {
		return base, errors.New("native_operation_not_found: no journal record for this operation_id")
	}
	v := journal.Operations[i]
	original := v
	if v.Project != req.Project || v.Instance != req.Instance || v.UUID != req.UUID || v.HostID != b.HostID || (v.ActorID != "" && v.ActorID != req.ActorID) {
		return base, errors.New("native_operation_identity_mismatch")
	}
	base.Operation = &v
	base.Status = v.State
	if nativeTerminal(v.State) {
		return base, nil
	}
	if v.NativeID == "" {
		b.reconcileLostAck(ctx, &v)
	}
	if v.NativeTerminal == "success" {
		// The native operation may already have expired. Preserve its verified
		// terminal code even when a later guest-identity check was unavailable.
		v.State = "native_succeeded"
	} else if v.NativeTerminal == "failure" {
		// A strictly matched native terminal failure survives operation expiry.
		v.State = "native_failed"
	} else if v.NativeID != "" {
		response, callErr := b.nativeRequest(ctx, http.MethodGet, "/1.0/operations/"+v.NativeID+"?project="+url.QueryEscape(v.Project), nil, "")
		if callErr == nil {
			id, code, project, parseErr := operationMetadata(response.Body)
			if parseErr == nil && id == v.NativeID && (project == "" || project == v.Project) {
				switch code {
				case 200:
					v.NativeTerminal = "success"
					v.State = "native_succeeded"
					v.Diagnostic = "Native Incus operation succeeded; verify guest contents and application health separately"
				case 400, 401:
					v.NativeTerminal = "failure"
					v.State = "native_failed"
					v.Diagnostic = "Native Incus operation failed or was canceled; inspect native diagnostics before a new request"
				default:
					if code >= 100 && code < 200 && code != 112 {
						v.State = "running"
						v.Diagnostic = ""
					} else {
						v.State = "outcome_uncertain"
						v.Diagnostic = "Unknown native Incus operation status code"
					}
				}
			} else {
				v.State = "outcome_uncertain"
				v.Diagnostic = "Native operation status identity could not be verified"
			}
		} else if strings.Contains(callErr.Error(), "incus_api_http_404") {
			v.State = "outcome_uncertain"
			v.Diagnostic = "Native operation expired before a terminal outcome was recorded; snapshot existence alone does not establish success"
		} else {
			// Transport outage is transient; keep the operation's prior state.
			v.Diagnostic = "Native status temporarily unavailable; retry operation_status with the same identity"
		}
	} else {
		v.State = "outcome_uncertain"
		v.Diagnostic = "Submission acknowledgement was lost and no uniquely matched running Incus operation was found; do not blindly retry"
	}
	if v.State == "native_succeeded" {
		current, found, inspectErr := b.inspect(ctx, v.Project, v.Instance)
		if inspectErr != nil || !found || current.UUID != v.UUID || !current.Managed || b.checkConfirmedIdentity(current) != nil {
			v.State = "outcome_uncertain"
			v.Diagnostic = "Native operation succeeded but guest identity could not be reverified"
		} else if v.Action == "snapshot" {
			names, snapshotErr := b.Snapshots(ctx, v.Project, v.Instance)
			if snapshotErr != nil || !hasSnapshot(names, v.Snapshot) {
				v.State = "outcome_uncertain"
				v.Diagnostic = "Native snapshot completion verified but snapshot list/identity not verified"
			}
		}
	}
	// A read-only status poll should not fsync the journal every second when
	// no material state changed. Persist transitions, errors and terminal proof.
	if v.NativeID != original.NativeID || v.State != original.State ||
		v.Diagnostic != original.Diagnostic || v.NativeTerminal != original.NativeTerminal {
		v.UpdatedAt = time.Now().UTC()
		journal.Operations[i] = v
		if err = b.saveNativeOperations(journal); err != nil {
			return base, fmt.Errorf("native_operation_outcome_uncertain: status persistence failed: %w", err)
		}
	}
	base.Status = v.State
	base.Operation = &v
	base.Diagnostic = v.Diagnostic
	return base, nil
}

// WatchNativeOperation runs inside the existing root Executor process after a
// nonblocking MCP acknowledgement. It persists terminal state during client
// disconnection. On Executor restart, operation_status reattaches/reconciles.
// resolveUncertainLocked is a deliberately approval-gated escape hatch after
// a native operation has expired without durable terminal proof. It does not
// pretend native success and never reuses the original submission ID.
func (b Backend) resolveUncertainLocked(ctx context.Context, req Request, approval bool) (Result, error) {
	base := Result{Status: "outcome_uncertain", Available: true, HostID: b.HostID}
	if !approval {
		return base, errors.New("approval_required: manual uncertain operation resolution requires explicit Owner authorization and inspected guest state")
	}
	if !validName(req.OperationID) || !validName(req.Project) || !validName(req.Instance) || !uuidPattern.MatchString(req.UUID) {
		return base, errors.New("resolve_uncertain requires exact operation_id, project, instance and native UUID")
	}
	current, err := b.nativeStatusLocked(ctx, req)
	if err != nil {
		return base, err
	}
	if current.Status != "outcome_uncertain" {
		return current, errors.New("native_operation_not_uncertain: terminal or running native operation cannot be retired")
	}
	if _, err = b.selectInstance(ctx, req); err != nil {
		return base, err
	}
	for i := 0; i < 2; i++ {
		raw, scanErr := b.command(ctx, 20*time.Second, "operation", "list", "--all-projects", "--format", "json")
		if scanErr != nil {
			return base, errors.New("native_operation_absence_unverified: operation inventory unavailable")
		}
		var entries []json.RawMessage
		if json.Unmarshal([]byte(raw), &entries) != nil || entries == nil {
			return base, errors.New("native_operation_absence_unverified: native inventory invalid")
		}
		// A lost acknowledgement may not expose a native operation UUID. Any
		// active operation could overlap a target mutation; refuse retirement.
		if len(entries) > 0 {
			return base, errors.New("native_operation_absence_unverified: active Incus operation present; do not retire")
		}
		if _, err = b.selectInstance(ctx, req); err != nil {
			return base, errors.New("native_operation_identity_changed: cannot retire journal")
		}
	}
	journal, err := b.loadNativeOperations()
	if err != nil {
		return base, err
	}
	for i, v := range journal.Operations {
		if v.ID != req.OperationID {
			continue
		}
		if v.State != "outcome_uncertain" || v.Project != req.Project || v.Instance != req.Instance || v.UUID != req.UUID || (v.ActorID != "" && v.ActorID != req.ActorID) {
			return base, errors.New("native_operation_identity_or_state_mismatch")
		}
		v.State = "closed_unverified"
		v.UpdatedAt = time.Now().UTC()
		v.Diagnostic = "Manually closed with current Owner approval after two verified empty native operation scans. Native outcome is NOT proven; inspect guest contents/application health. Never reuse this operation_id."
		journal.Operations[i] = v
		if err = b.saveNativeOperations(journal); err != nil {
			return base, err
		}
		base.Status = v.State
		base.Operation = &v
		base.Diagnostic = v.Diagnostic
		return base, nil
	}
	return base, errors.New("native_operation_not_found")
}

// RecoverableNativeOperations is invoked only by the trusted root Executor
// on startup. It does not create an optional Incus state directory on hosts
// that have never used managed snapshot/restore.
func (b Backend) RecoverableNativeOperations() ([]NativeOperation, error) {
	if _, err := os.Lstat(b.nativeOperationsPath()); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	result, err := b.Lock(func() (Result, error) {
		d, loadErr := b.loadNativeOperations()
		if loadErr != nil {
			return Result{}, loadErr
		}
		pending := make([]NativeOperation, 0)
		for _, op := range d.Operations {
			if !nativeTerminal(op.State) {
				pending = append(pending, op)
			}
		}
		return Result{Operations: pending}, nil
	})
	return result.Operations, err
}

// WatchNativeOperation observes a bounded slice of a durable operation. It
// returns true only when a native terminal record is persisted. A lost HTTP
// acknowledgement, temporary native API failure, or absent proof is NOT
// completion: the Executor must give this operation another observation turn.
func (b Backend) WatchNativeOperation(ctx context.Context, req Request) bool {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		result, err := b.Lock(func() (Result, error) { return b.nativeStatusLocked(ctx, req) })
		if err == nil && result.Operation != nil && nativeTerminal(result.Status) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
