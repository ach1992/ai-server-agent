package serviceaccess

// Package serviceaccess owns the Agent's intentionally small, root-private
// target-service credential store. It is distinct from inbound MCP bearers,
// audit, worktrees and general managed runtime state.
import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	version       = 1
	defaultTTL    = 30 * 24 * time.Hour
	maxStoreBytes = 16 << 20
)

type Record struct {
	ID         string     `json:"id"`
	HostID     string     `json:"host_id"`
	Project    string     `json:"project,omitempty"`
	Guest      string     `json:"guest,omitempty"`
	GuestUUID  string     `json:"guest_uuid,omitempty"`
	Service    string     `json:"service"`
	Purpose    string     `json:"purpose,omitempty"`
	Endpoint   string     `json:"endpoint"`
	Username   string     `json:"username,omitempty"`
	Kind       string     `json:"kind,omitempty"` // Nonsecret kind label; no provider or consumer allowlist.
	Secret     string     `json:"secret,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}
type Metadata struct {
	ID         string     `json:"id"`
	HostID     string     `json:"host_id"`
	Project    string     `json:"project,omitempty"`
	Guest      string     `json:"guest,omitempty"`
	GuestUUID  string     `json:"guest_uuid,omitempty"`
	Service    string     `json:"service"`
	Purpose    string     `json:"purpose,omitempty"`
	Endpoint   string     `json:"endpoint"`
	Username   string     `json:"username,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Status     string     `json:"status"`
}
type Registration struct {
	HostID    string `json:"host_id"`
	Project   string `json:"project,omitempty"`
	Guest     string `json:"guest,omitempty"`
	GuestUUID string `json:"guest_uuid,omitempty"`
	Service   string `json:"service"`
	Purpose   string `json:"purpose,omitempty"`
	Endpoint  string `json:"endpoint"`
	Username  string `json:"username,omitempty"`
}
type diskStore struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}
type Store struct {
	dir string
	now func() time.Time
}

func New(stateDir string) *Store {
	return &Store{dir: filepath.Join(stateDir, "service-access"), now: func() time.Time { return time.Now().UTC() }}
}
func (s *Store) Path() string { return s.dir }

// ValidServiceTarget validates a target's non-secret identity only, not an
// executable network operation. Records support application-specific network
// schemes (SSH, database, API, etc.) without allocating a Browser session or
// granting arbitrary code a way to retrieve the credential. Each authorized
// consumer must independently validate its supported protocol and destination.
func ValidServiceTarget(raw string) error {
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return errors.New("service target must be a bounded network URI without control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("service target must be an absolute network URI without userinfo, query or fragment")
	}
	// A local file or code-like URL is not a service destination, even when
	// it syntactically includes an authority/host. Target URI never serves
	// as a command, filesystem path, or a caller-selected secret sink.
	switch strings.ToLower(u.Scheme) {
	case "file", "data", "javascript", "about", "blob":
		return errors.New("service target must use a network-service URI scheme")
	}
	return nil
}

// ValidEndpoint is the narrower HTTP(S) Browser/API endpoint contract.
// Accepting a generic target record must never automatically authorize
// credential delivery through an HTTP-based consumer.
func ValidEndpoint(raw string) error {
	if err := ValidServiceTarget(raw); err != nil {
		return errors.New("endpoint must be an HTTP(S) URL without userinfo, query or fragment")
	}
	u, _ := url.Parse(raw)
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("endpoint must be an HTTP(S) URL without userinfo, query or fragment")
	}
	return nil
}
func Origin(raw string) string {
	u, _ := url.Parse(raw)
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}
func validateRegistration(r Registration) error {
	if strings.TrimSpace(r.HostID) == "" || strings.TrimSpace(r.Service) == "" || len(r.HostID) > 160 || len(r.Service) > 120 || len(r.Purpose) > 500 || len(r.Username) > 256 {
		return errors.New("host_id, service and bounded metadata are required")
	}
	if (r.Guest == "") != (r.GuestUUID == "") || (r.Guest != "" && r.Project == "") || (r.Guest == "" && r.Project != "") || len(r.Guest) > 120 || len(r.Project) > 120 || len(r.GuestUUID) > 120 {
		return errors.New("guest requires project and verified native UUID")
	}
	if err := ValidServiceTarget(r.Endpoint); err != nil {
		return err
	}
	for _, x := range []string{r.HostID, r.Service, r.Purpose, r.Username, r.Guest, r.GuestUUID, r.Project} {
		if strings.IndexFunc(x, func(c rune) bool { return c < 32 || c == 127 }) >= 0 {
			return errors.New("invalid control character in metadata")
		}
	}
	return nil
}
func (r Record) Meta(now time.Time) Metadata {
	status := "unenrolled"
	switch {
	case r.RevokedAt != nil:
		status = "revoked"
	case r.ExpiresAt != nil && !now.Before(*r.ExpiresAt):
		status = "expired"
	case r.Secret == "":
		status = "unenrolled"
	default:
		status = "ready"
	}
	return Metadata{ID: r.ID, HostID: r.HostID, Project: r.Project, Guest: r.Guest, GuestUUID: r.GuestUUID, Service: r.Service, Purpose: r.Purpose, Endpoint: r.Endpoint, Username: r.Username, Kind: r.Kind, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt, Status: status}
}
func validID(id string) bool {
	if !strings.HasPrefix(id, "svc_") || len(id) != 36 {
		return false
	}
	for _, r := range id[4:] {
		if !((r >= 'a' && r <= 'f') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
func id() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "svc_" + hex.EncodeToString(b[:]), nil
}
func (s *Store) ensureDir(create bool) error {
	fi, err := os.Lstat(s.dir)
	if os.IsNotExist(err) && create {
		if err := os.Mkdir(s.dir, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		fi, err = os.Lstat(s.dir)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0700 {
		return errors.New("service access directory must be a private non-symlink directory (0700)")
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("service access directory is not owned by the executor identity")
	}
	return nil
}
func readProtected(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || sys.Nlink != 1 || int(sys.Uid) != os.Geteuid() {
		return nil, errors.New("credential store file has unsafe identity or permissions")
	}
	return io.ReadAll(io.LimitReader(f, maxStoreBytes+1))
}
func (s *Store) load() (diskStore, error) {
	d := diskStore{Version: version}
	if err := s.ensureDir(false); err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return d, err
	}
	data, err := readProtected(filepath.Join(s.dir, "records.json"))
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if len(data) > maxStoreBytes {
		return d, errors.New("credential store too large")
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, errors.New("credential store has invalid JSON")
	}
	if d.Version != version {
		return d, errors.New("unsupported credential store version")
	}
	seen := map[string]bool{}
	for _, r := range d.Records {
		if !validID(r.ID) || seen[r.ID] || validateRegistration(Registration{HostID: r.HostID, Project: r.Project, Guest: r.Guest, GuestUUID: r.GuestUUID, Service: r.Service, Purpose: r.Purpose, Endpoint: r.Endpoint, Username: r.Username}) != nil {
			return d, errors.New("invalid or duplicate stored service identity")
		}
		seen[r.ID] = true
	}
	return d, nil
}
func (s *Store) withLock(fn func(*diskStore) (bool, error)) error {
	if err := s.ensureDir(true); err != nil {
		return err
	}
	lockPath := filepath.Join(s.dir, ".lock")
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), lockPath)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || sys.Nlink != 1 || int(sys.Uid) != os.Geteuid() {
		return errors.New("unsafe credential store lock identity")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	d, err := s.load()
	if err != nil {
		return err
	}
	changed, err := fn(&d)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(data) > maxStoreBytes {
		return errors.New("credential store size limit reached")
	}
	tmp, err := os.OpenFile(filepath.Join(s.dir, ".tmp-"+mustRandom()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, filepath.Join(s.dir, "records.json")); err != nil {
		return err
	}
	dirfd, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dirfd.Close()
	return dirfd.Sync()
}
func mustRandom() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
func (s *Store) List() ([]Metadata, error) {
	d, err := s.load()
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]Metadata, 0, len(d.Records))
	for _, r := range d.Records {
		out = append(out, r.Meta(now))
	}
	return out, nil
}
func (s *Store) Register(r Registration) (Metadata, error) {
	return s.register(r, "", "", 0, false)
}

// RegisterWithSecret provides the operator-local one-command path. The
// registration and protected secret are committed in the SAME fsynced store
// transaction; a failed import cannot leave a newly registered empty ref.
func (s *Store) RegisterWithSecret(r Registration, kind, secret string, ttlDays int, never bool) (Metadata, error) {
	if err := validateSecret(kind, secret); err != nil {
		return Metadata{}, err
	}
	return s.register(r, kind, secret, ttlDays, never)
}

func (s *Store) register(r Registration, kind, secret string, ttlDays int, never bool) (Metadata, error) {
	if err := validateRegistration(r); err != nil {
		return Metadata{}, err
	}
	var expires *time.Time
	if secret != "" {
		var err error
		expires, err = expiration(s.now(), ttlDays, never)
		if err != nil {
			return Metadata{}, err
		}
	}
	newID, err := id()
	if err != nil {
		return Metadata{}, err
	}
	now := s.now()
	rec := Record{ID: newID, HostID: r.HostID, Project: r.Project, Guest: r.Guest, GuestUUID: r.GuestUUID, Service: r.Service, Purpose: r.Purpose, Endpoint: r.Endpoint, Username: r.Username, Kind: kind, Secret: secret, ExpiresAt: expires, CreatedAt: now, UpdatedAt: now}
	err = s.withLock(func(d *diskStore) (bool, error) {
		for _, existing := range d.Records {
			if existing.HostID == r.HostID && existing.Project == r.Project && existing.Guest == r.Guest && existing.GuestUUID == r.GuestUUID && existing.Service == r.Service && existing.Endpoint == r.Endpoint && existing.Username == r.Username && existing.Purpose == r.Purpose && existing.RevokedAt == nil {
				return false, errors.New("service_already_registered: inspect existing references before creating duplicates")
			}
		}
		d.Records = append(d.Records, rec)
		return true, nil
	})
	return rec.Meta(now), err
}
func (s *Store) Update(id string, fn func(*Record) error) (Metadata, error) {
	var meta Metadata
	if !validID(id) {
		return meta, errors.New("invalid service reference")
	}
	err := s.withLock(func(d *diskStore) (bool, error) {
		for i := range d.Records {
			if d.Records[i].ID != id {
				continue
			}
			if err := fn(&d.Records[i]); err != nil {
				return false, err
			}
			d.Records[i].UpdatedAt = s.now()
			meta = d.Records[i].Meta(s.now())
			return true, nil
		}
		return false, errors.New("service_reference_not_found")
	})
	return meta, err
}
func expiration(now time.Time, ttlDays int, never bool) (*time.Time, error) {
	if ttlDays < 0 || (ttlDays > 0 && never) {
		return nil, errors.New("specify positive ttl_days or never, not both")
	}
	if never {
		return nil, nil
	}
	if ttlDays == 0 {
		ttlDays = 30
	}
	if ttlDays > 36500 {
		return nil, errors.New("ttl exceeds 100 years")
	}
	at := now.Add(time.Duration(ttlDays) * 24 * time.Hour)
	return &at, nil
}

// Kind is an operator/application label, not a credential encoding or a
// permission grant. A consumer must interpret only the format it supports.
func ValidKind(kind string) bool {
	if kind == "" || len(kind) > 64 {
		return false
	}
	for i, r := range kind {
		if (r >= 'a' && r <= 'z') || (i > 0 && ((r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.')) {
			continue
		}
		return false
	}
	return true
}

func validateSecret(kind, secret string) error {
	if !ValidKind(kind) {
		return errors.New("credential kind must be a short lowercase label")
	}
	// Textual multi-line keys (e.g. PEM) are supported for private file
	// enrollment. NUL and invalid UTF-8 cannot be safely delivered by all
	// current consumers; binary keys need a dedicated adapter/encoding.
	if secret == "" || len(secret) > 4096 || strings.IndexByte(secret, 0) >= 0 || !utf8.ValidString(secret) {
		return errors.New("credential must be nonempty UTF-8 text of at most 4096 bytes")
	}
	return nil
}

func (s *Store) Enroll(id, kind, username, secret string, ttlDays int, never bool) (Metadata, error) {
	if err := validateSecret(kind, secret); err != nil {
		return Metadata{}, err
	}
	if len(username) > 256 || strings.IndexFunc(username, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return Metadata{}, errors.New("invalid username metadata")
	}
	exp, err := expiration(s.now(), ttlDays, never)
	if err != nil {
		return Metadata{}, err
	}
	return s.Update(id, func(r *Record) error {
		if r.RevokedAt != nil {
			return errors.New("service_revoked: create a new registration")
		}
		r.Kind = kind
		if username != "" {
			r.Username = username
		}
		r.Secret = secret
		r.ExpiresAt = exp
		r.LastUsedAt = nil
		return nil
	})
}
func (s *Store) Generate(id, kind, username string, ttlDays int, never bool) (Metadata, error) {
	// A random 256-bit hex string is appropriate as an Agent-generated
	// password/token, not a provider-issued API key, SSH keypair, or OTP.
	if kind != "password" && kind != "token" {
		return Metadata{}, errors.New("generate supports password or token only; import externally issued credential material")
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Metadata{}, err
	}
	secret := hex.EncodeToString(b[:])
	return s.Enroll(id, kind, username, secret, ttlDays, never)
}
func (s *Store) Renew(id string, ttlDays int, never bool) (Metadata, error) {
	exp, err := expiration(s.now(), ttlDays, never)
	if err != nil {
		return Metadata{}, err
	}
	return s.Update(id, func(r *Record) error {
		if r.RevokedAt != nil {
			return errors.New("service_revoked")
		}
		if r.Secret == "" {
			return errors.New("service_not_enrolled")
		}
		r.ExpiresAt = exp
		return nil
	})
}
func (s *Store) Revoke(id string) (Metadata, error) {
	return s.Update(id, func(r *Record) error {
		if r.RevokedAt != nil {
			return errors.New("service_already_revoked")
		}
		now := s.now()
		r.RevokedAt = &now
		r.Secret = ""
		return nil
	})
}
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return errors.New("invalid service reference")
	}
	return s.withLock(func(d *diskStore) (bool, error) {
		for i, r := range d.Records {
			if r.ID == id {
				d.Records = append(d.Records[:i], d.Records[i+1:]...)
				return true, nil
			}
		}
		return false, errors.New("service_reference_not_found")
	})
}

// Resolve is executor-local only. Never return its secret to an MCP tool, audit
// entry, JSON metadata, log, shell argv or a browser trace.
func (s *Store) Resolve(id, host string) (Record, error) {
	if !validID(id) {
		return Record{}, errors.New("invalid service reference")
	}
	d, err := s.load()
	if err != nil {
		return Record{}, err
	}
	for _, r := range d.Records {
		if r.ID != id {
			continue
		}
		if r.HostID != host {
			return Record{}, errors.New("service_host_mismatch")
		}
		switch r.Meta(s.now()).Status {
		case "ready":
			return r, nil
		case "expired":
			return Record{}, errors.New("service_credential_expired")
		case "revoked":
			return Record{}, errors.New("service_credential_revoked")
		default:
			return Record{}, errors.New("service_credential_unavailable")
		}
	}
	return Record{}, errors.New("service_reference_not_found")
}

// MarkUsed is a best-effort metadata-only touch after a successful privileged
// use. It deliberately does not renew expiration.
func (s *Store) MarkUsed(id string) error {
	_, err := s.Update(id, func(r *Record) error { now := s.now(); r.LastUsedAt = &now; return nil })
	return err
}

// CleanupExpired erases expired *secrets* but retains non-secret service
// metadata for recovery/diagnosis. On an offline host, cleanup runs at next
// explicit maintenance call; expiry enforcement is immediate on every Resolve.
func (s *Store) CleanupExpired() (int, error) {
	removed := 0
	err := s.withLock(func(d *diskStore) (bool, error) {
		for i := range d.Records {
			r := &d.Records[i]
			if r.Secret != "" && r.ExpiresAt != nil && !s.now().Before(*r.ExpiresAt) {
				r.Secret = ""
				r.UpdatedAt = s.now()
				removed++
			}
		}
		return removed > 0, nil
	})
	return removed, err
}

// Input is the non-secret operational request. No MCP/Executor request or
// ordinary tool argument is permitted to contain a credential value.
type Input struct {
	Operation    string       `json:"operation"`
	Ref          string       `json:"ref,omitempty"`
	Registration Registration `json:"registration,omitempty"`
	Kind         string       `json:"kind,omitempty"`
	Username     string       `json:"username,omitempty"`
	TTLDays      int          `json:"ttl_days,omitempty"`
	NeverExpires bool         `json:"never_expires,omitempty"`
}

// Modify updates service metadata without replacing the credential or native
// guest identity. Cross-origin changes need the same explicit Owner approval
// as other potentially credential-disclosing destination changes.
func (s *Store) Modify(ref string, change Registration, allowOriginChange bool) (Metadata, error) {
	var result Metadata
	if !validID(ref) {
		return result, errors.New("invalid service reference")
	}
	err := s.withLock(func(d *diskStore) (bool, error) {
		for i := range d.Records {
			current := d.Records[i]
			if current.ID != ref {
				continue
			}
			if current.RevokedAt != nil {
				return false, errors.New("service_credential_revoked")
			}
			if change.HostID != "" && change.HostID != current.HostID {
				return false, errors.New("service_host_mismatch")
			}
			for _, pair := range [][2]string{{change.Project, current.Project}, {change.Guest, current.Guest}, {change.GuestUUID, current.GuestUUID}} {
				if pair[0] != "" && pair[0] != pair[1] {
					return false, errors.New("guest_binding_immutable: register a new reference for a different native guest")
				}
			}
			next := current
			if change.Endpoint != "" {
				if err := ValidServiceTarget(change.Endpoint); err != nil {
					return false, err
				}
				if Origin(change.Endpoint) != Origin(current.Endpoint) && !allowOriginChange {
					return false, errors.New("approval_required: changing credential destination origin requires explicit Owner authorization")
				}
				next.Endpoint = change.Endpoint
			}
			if change.Service != "" {
				next.Service = change.Service
			}
			if change.Username != "" {
				next.Username = change.Username
			}
			if change.Purpose != "" {
				next.Purpose = change.Purpose
			}
			if err := validateRegistration(Registration{HostID: next.HostID, Project: next.Project, Guest: next.Guest, GuestUUID: next.GuestUUID, Service: next.Service, Purpose: next.Purpose, Endpoint: next.Endpoint, Username: next.Username}); err != nil {
				return false, err
			}
			for j, r := range d.Records {
				if i != j && r.RevokedAt == nil && r.HostID == next.HostID && r.Project == next.Project && r.Guest == next.Guest && r.GuestUUID == next.GuestUUID && r.Service == next.Service && r.Endpoint == next.Endpoint && r.Username == next.Username && r.Purpose == next.Purpose {
					return false, errors.New("service_already_registered: duplicate service destination")
				}
			}
			if next.Endpoint == current.Endpoint && next.Service == current.Service && next.Username == current.Username && next.Purpose == current.Purpose {
				result = current.Meta(s.now())
				return false, nil
			}
			next.UpdatedAt = s.now()
			d.Records[i] = next
			result = next.Meta(s.now())
			return true, nil
		}
		return false, errors.New("service_reference_not_found")
	})
	return result, err
}
