package systemexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// An operator transaction has one durable intent record and one atomic
// active-selection commit point. The previous-good selection is only rewritten
// AFTER the active path commits. Recovery distinguishes an uncommitted intent
// from a committed selection using exact file bytes, without guessing or
// silently reverting an invalid active profile.
type adapterSelectionTransition struct {
	Name          string `json:"name"`
	BeforeActive  []byte `json:"before_active,omitempty"`
	AfterActive   []byte `json:"after_active,omitempty"`
	AfterPrevious []byte `json:"after_previous,omitempty"`
	SaveInactive  bool   `json:"save_inactive,omitempty"`
	AfterInactive []byte `json:"after_inactive,omitempty"`
}

func adapterStatePath(dir, name, suffix string) string {
	return filepath.Join(dir, name+suffix+".json")
}

func readAdapterStateBytes(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 || fi.Size() > 32<<10 {
		return nil, fmt.Errorf("adapter selection file invalid: %s", path)
	}
	return os.ReadFile(path)
}

func syncAdapterDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func putAdapterStateBytes(path string, data []byte, mode os.FileMode) error {
	if data == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncAdapterDirectory(filepath.Dir(path))
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-stage-")
	if err != nil {
		return err
	}
	stage := f.Name()
	defer os.Remove(stage)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
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
	if err = os.Rename(stage, path); err != nil {
		return err
	}
	return syncAdapterDirectory(dir)
}

func readAdapterTransition(dir, name string) (*adapterSelectionTransition, error) {
	path := adapterStatePath(dir, name, ".pending")
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() <= 0 || fi.Size() > 160<<10 {
		return nil, errors.New("adapter selection recovery record is unsafe")
	}
	// The production directory is root-controlled; worker-owned test fixtures
	// use a different path. No unprivileged runtime path can write this record.
	if dir == OptionalAdapterProfilesDir {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm() != 0600 {
			return nil, errors.New("adapter selection recovery record is not root-owned 0600")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 160<<10+1))
	dec.DisallowUnknownFields()
	var tx adapterSelectionTransition
	if err := dec.Decode(&tx); err != nil {
		return nil, fmt.Errorf("adapter selection recovery record malformed: %w", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || tx.Name != name || len(tx.BeforeActive) > 32<<10 ||
		len(tx.AfterActive) > 32<<10 || len(tx.AfterPrevious) > 32<<10 ||
		len(tx.AfterInactive) > 32<<10 || bytes.Equal(tx.BeforeActive, tx.AfterActive) ||
		(!tx.SaveInactive && tx.AfterInactive != nil) {
		return nil, errors.New("adapter selection recovery record inconsistent")
	}
	return &tx, nil
}

// reconcileAdapterSelection is called only under the root lifecycle lock by
// production callers. It is also used directly with disposable directories by
// unprivileged state-machine tests.
func reconcileAdapterSelection(dir, name string) error {
	tx, err := readAdapterTransition(dir, name)
	if err != nil || tx == nil {
		return err
	}
	active := adapterStatePath(dir, name, "")
	current, err := readAdapterStateBytes(active)
	if err != nil {
		return err
	}
	pending := adapterStatePath(dir, name, ".pending")
	if bytes.Equal(current, tx.BeforeActive) {
		// The new active selection never committed. Previous-good history was
		// deliberately untouched: discard only this interrupted intent.
		return putAdapterStateBytes(pending, nil, 0600)
	}
	if !bytes.Equal(current, tx.AfterActive) {
		return errors.New("adapter selection changed outside pending transaction; manual reconciliation required")
	}
	// The active selection committed. Complete the matching rollback history;
	// each step is atomic and safe to repeat after a crash or fsync error.
	if err := putAdapterStateBytes(adapterStatePath(dir, name, ".previous"), tx.AfterPrevious, 0644); err != nil {
		return err
	}
	if tx.SaveInactive {
		if err := putAdapterStateBytes(adapterStatePath(dir, name, ".inactive"), tx.AfterInactive, 0644); err != nil {
			return err
		}
	}
	return putAdapterStateBytes(pending, nil, 0600)
}

func switchAdapterSelection(dir, name string, tx adapterSelectionTransition) error {
	if err := reconcileAdapterSelection(dir, name); err != nil {
		return err
	}
	current, err := readAdapterStateBytes(adapterStatePath(dir, name, ""))
	if err != nil {
		return err
	}
	if !bytes.Equal(current, tx.BeforeActive) {
		return errors.New("adapter selection changed while preparing transaction")
	}
	if bytes.Equal(tx.BeforeActive, tx.AfterActive) {
		return nil // identical selection never overwrites previous-good history
	}
	tx.Name = name
	pending, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	if len(pending) > 160<<10 {
		return errors.New("adapter selection recovery record exceeds bounded size")
	}
	if err := putAdapterStateBytes(adapterStatePath(dir, name, ".pending"), pending, 0600); err != nil {
		return err
	}
	// The next rename/unlink is the commit point. A crash or error at any step
	// leaves enough information to deterministically abort or finish.
	if err := putAdapterStateBytes(adapterStatePath(dir, name, ""), tx.AfterActive, 0644); err != nil {
		return err
	}
	return reconcileAdapterSelection(dir, name)
}
