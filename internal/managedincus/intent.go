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
	HostID      string    `json:"host_id"`
	Project     string    `json:"project"`
	Instance    string    `json:"instance"`
	WorkID      string    `json:"work_id"`
	OperationID string    `json:"operation_id"`
	Type        string    `json:"type"`
	Image       string    `json:"image"`
	StoragePool string    `json:"storage_pool,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
	State       string    `json:"state"` // submitted or confirmed; unverified submission is NEVER a negative result
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
		if x.HostID == "" || !validName(x.Project) || !validName(x.Instance) || !validName(x.OperationID) || !validName(x.WorkID) || !imagePattern.MatchString(x.Image) || seen[x.OperationID] || (x.State != "submitted" && x.State != "confirmed") {
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
			if x.Project != req.Project || x.Instance != req.Instance || x.WorkID != req.WorkID || x.Type != req.Type || x.Image != req.Image || x.StoragePool != req.StoragePool {
				return false, errors.New("create_intent_collision: stable operation ID already submitted with different parameters")
			}
			return true, nil
		}
		if x.Project == req.Project && x.Instance == req.Instance {
			return false, errors.New("create_intent_collision: a different creation operation already reserves this project/name")
		}
	}
	d.Intents = append(d.Intents, intentFor(req, b.HostID))
	return false, b.saveIntents(d)
}
func (b Backend) confirmCreate(operationID string) error {
	d, err := b.loadIntents()
	if err != nil {
		return err
	}
	for i := range d.Intents {
		if d.Intents[i].OperationID == operationID {
			if d.Intents[i].State == "confirmed" {
				return nil
			}
			d.Intents[i].State = "confirmed"
			return b.saveIntents(d)
		}
	}
	return errors.New("create_intent_missing: cannot confirm unrecorded launch")
}
func (b Backend) retireDeleted(project, instance string) error {
	// Caller must have locked, checked the exact native identity pre-delete and
	// verified absence post-delete. Removing its own operation intent enables
	// later legitimate explicit fresh creates with a different operation ID.
	d, err := b.loadIntents()
	if err != nil {
		return err
	}
	var kept []CreateIntent
	for _, x := range d.Intents {
		if x.Project != project || x.Instance != instance {
			kept = append(kept, x)
		}
	}
	if len(kept) == len(d.Intents) {
		return nil
	}
	d.Intents = kept
	return b.saveIntents(d)
}
