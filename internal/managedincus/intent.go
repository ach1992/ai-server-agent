package managedincus

// This tiny durable native-create intent journal is not an instance database.
// Incus remains authoritative for actual guest state. The journal records
// submitted creates which could continue after a CLI timeout or Agent restart,
// preventing a second launch based on a transient empty Incus listing.
import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type CreateIntent struct {
	HostID      string     `json:"host_id"`
	Project     string     `json:"project"`
	Instance    string     `json:"instance"`
	WorkID      string     `json:"work_id"`
	OperationID string     `json:"operation_id"`
	Type        string     `json:"type"`
	Image       string     `json:"image"`
	StoragePool string     `json:"storage_pool,omitempty"`
	SubmittedAt time.Time  `json:"submitted_at"`
	State       string     `json:"state"`                 // submitted, confirmed, or explicitly abandoned
	NativeUUID  string     `json:"native_uuid,omitempty"` // verified native UUID, immutable after confirmation
	AbandonedAt *time.Time `json:"abandoned_at,omitempty"`
}
type intentJournal struct {
	Version int            `json:"version"`
	Intents []CreateIntent `json:"intents"`
}

func (b Backend) intentPath() string {
	return filepath.Join(b.StateDir, "managed-incus", "create-intents.json")
}
func (b Backend) loadIntents() (intentJournal, error) {
	d := intentJournal{Version: 1}
	path := b.intentPath()
	f, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	file := os.NewFile(uintptr(f), path)
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return d, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || sys.Nlink != 1 || int(sys.Uid) != os.Geteuid() {
		return d, errors.New("unsafe Incus create intent file identity")
	}
	bytes, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return d, err
	}
	if len(bytes) > 1<<20 {
		return d, errors.New("Incus create intents exceed size bound")
	}
	if err := json.Unmarshal(bytes, &d); err != nil {
		return d, errors.New("corrupt Incus create intent journal")
	}
	if d.Version != 1 {
		return d, errors.New("unsupported Incus create intent journal version")
	}
	seen := map[string]bool{}
	for _, x := range d.Intents {
		if x.HostID == "" || !validName(x.Project) || !validName(x.Instance) || !validName(x.OperationID) || !validName(x.WorkID) || !imagePattern.MatchString(x.Image) || !validName(x.StoragePool) || seen[x.OperationID] || (x.State != "submitted" && x.State != "confirmed" && x.State != "abandoned") || (x.NativeUUID != "" && !uuidPattern.MatchString(x.NativeUUID)) || (x.State == "submitted" && (x.NativeUUID != "" || x.AbandonedAt != nil)) || (x.State == "abandoned" && (x.AbandonedAt == nil || x.NativeUUID != "")) || (x.State == "confirmed" && x.AbandonedAt != nil) {
			return d, errors.New("invalid/duplicate Incus create intent")
		}
		seen[x.OperationID] = true
	}
	return d, nil
}
func (b Backend) saveIntents(d intentJournal) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("Incus create intent journal exceeded limit")
	}
	var r [12]byte
	if _, err = rand.Read(r[:]); err != nil {
		return err
	}
	tmp := filepath.Join(b.StateDir, "managed-incus", ".intent-"+hex.EncodeToString(r[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
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
	if err = os.Rename(tmp, b.intentPath()); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(b.intentPath()))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func intentFor(req Request, host string) CreateIntent {
	return CreateIntent{HostID: host, Project: req.Project, Instance: req.Instance, WorkID: req.WorkID, OperationID: req.OperationID, Type: req.Type, Image: req.Image, StoragePool: req.StoragePool, SubmittedAt: time.Now().UTC(), State: "submitted"}
}

// Run/create is always called under Backend.Lock. Save before launch.
func (b Backend) reserveCreate(req Request) (bool, error) {
	d, err := b.loadIntents()
	if err != nil {
		return false, err
	}
	for _, x := range d.Intents {
		if x.HostID != b.HostID {
			return false, errors.New("create_intent_host_mismatch")
		}
		if x.OperationID == req.OperationID {
			if x.State == "abandoned" {
				return false, errors.New("create_intent_abandoned_operation_id: use a fresh id for a new authorized launch")
			}
			if x.Project != req.Project || x.Instance != req.Instance || x.WorkID != req.WorkID || x.Type != req.Type || x.Image != req.Image || x.StoragePool != req.StoragePool {
				return false, errors.New("create_intent_collision: stable operation ID already submitted with different parameters")
			}
			return true, nil
		}
		if x.State != "abandoned" && x.Project == req.Project && x.Instance == req.Instance {
			return false, errors.New("create_intent_collision: a different creation operation already reserves this project/name")
		}
	}
	d.Intents = append(d.Intents, intentFor(req, b.HostID))
	return false, b.saveIntents(d)
}

// confirmCreate binds the first verified native UUID durably. A later
// same-name/tag replacement cannot silently inherit the original operation.
func (b Backend) confirmCreate(operationID, nativeUUID string) error {
	if !uuidPattern.MatchString(nativeUUID) {
		return errors.New("native_uuid_unverified: malformed native guest UUID")
	}
	d, err := b.loadIntents()
	if err != nil {
		return err
	}
	for i := range d.Intents {
		x := &d.Intents[i]
		if x.OperationID != operationID {
			continue
		}
		if x.HostID != b.HostID {
			return errors.New("create_intent_host_mismatch")
		}
		switch x.State {
		case "confirmed":
			if x.NativeUUID == "" {
				return errors.New("create_intent_uuid_unavailable: confirmed record lacks original UUID; manual reconciliation required")
			}
			if x.NativeUUID != nativeUUID {
				return errors.New("create_intent_native_uuid_conflict: native guest UUID differs from the originally confirmed instance")
			}
			return nil
		case "submitted":
			x.State = "confirmed"
			x.NativeUUID = nativeUUID
			return b.saveIntents(d)
		default:
			return errors.New("create_intent_abandoned: cannot silently reinstate retired operation")
		}
	}
	return errors.New("create_intent_missing: cannot confirm unrecorded launch")
}

// Abandonment retains a non-secret tombstone to prohibit reuse of the old
// operation ID while releasing the project/name for a new explicit create.
// Caller must hold b.Lock and have proved native absence and approval.
func (b Backend) abandonIntent(req Request) (CreateIntent, error) {
	d, err := b.loadIntents()
	if err != nil {
		return CreateIntent{}, err
	}
	for i := range d.Intents {
		x := &d.Intents[i]
		if x.OperationID != req.OperationID {
			continue
		}
		if x.HostID != b.HostID || x.Project != req.Project || x.Instance != req.Instance || x.WorkID != req.WorkID {
			return CreateIntent{}, errors.New("create_intent_identity_mismatch: full host/project/name/work/operation identity required")
		}
		if x.State != "submitted" || x.NativeUUID != "" {
			return CreateIntent{}, errors.New("create_intent_not_pending: confirmed or previously abandoned operations cannot be retired as uncertain")
		}
		now := time.Now().UTC()
		x.State = "abandoned"
		x.AbandonedAt = &now
		if err := b.saveIntents(d); err != nil {
			return CreateIntent{}, err
		}
		return *x, nil
	}
	return CreateIntent{}, errors.New("create_intent_missing: no matching submitted create operation")
}

// A journaled native identity owns a project/name regardless of what a
// replacement guest claims in its operation/work tags. Call under Backend.Lock
// immediately before name-based native guest operations.
func (b Backend) checkConfirmedIdentity(x Summary) error {
	d, err := b.loadIntents()
	if err != nil {
		return err
	}
	for _, intent := range d.Intents {
		if intent.HostID != b.HostID {
			return errors.New("create_intent_host_mismatch: local journal belongs to a different Agent identity")
		}
		sameName := intent.Project == x.Project && intent.Instance == x.Name
		sameOperation := intent.OperationID == x.OperationID
		if intent.State == "abandoned" {
			// An abandoned operation ID is permanently retired, but its
			// former project/name may be legitimately reused with a fresh ID.
			if sameOperation {
				return errors.New("create_intent_abandoned_identity_conflict: retired native operation reappeared")
			}
			continue
		}
		if !sameName && !sameOperation {
			continue
		}
		if !sameName {
			return errors.New("create_intent_identity_mismatch: native operation tag is bound to a different project/name")
		}
		// Prioritize immutable provider identity even when another native
		// actor removed or changed the operation/work tags.
		if intent.State == "confirmed" {
			if intent.NativeUUID == "" {
				return errors.New("create_intent_uuid_unavailable: confirmed record lacks original UUID")
			}
			if intent.NativeUUID != x.UUID {
				return errors.New("create_intent_native_uuid_conflict: guest was replaced after original confirmation")
			}
		}
		if !sameOperation || intent.WorkID != x.WorkID {
			return errors.New("create_intent_identity_mismatch: native work/operation tags differ from the protected project/name binding")
		}
		// A submitted intent can be reconciled through create, not silently
		// taken over by a mutation while its native identity is still uncertain.
		if intent.State == "submitted" {
			return errors.New("create_intent_unconfirmed: finish create reconciliation before mutation")
		}
	}
	return nil
}

func (b Backend) retireDeleted(project, instance, operationID, nativeUUID string) error {
	// Caller is holding the lifecycle lock and verified that the exact guest
	// was deleted. Retire only the matching confirmed journal entry, preserving
	// old abandoned operation IDs as permanent anti-replay tombstones.
	d, err := b.loadIntents()
	if err != nil {
		return err
	}
	kept := make([]CreateIntent, 0, len(d.Intents))
	removed := false
	for _, x := range d.Intents {
		if x.Project != project || x.Instance != instance || x.OperationID != operationID {
			kept = append(kept, x)
			continue
		}
		if x.State != "confirmed" || x.NativeUUID != nativeUUID {
			return errors.New("create_intent_delete_identity_conflict: confirmed journal identity disagrees with native deleted guest")
		}
		removed = true
	}
	if !removed {
		return nil
	} // native guest predates the optional intent journal
	d.Intents = kept
	return b.saveIntents(d)
}
