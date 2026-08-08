package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const foundationFileName = "runtime-foundation.json"

// S0 recognizes the empty foundation envelope only. It never creates or
// advances this file; later Runtime layers must atomically commit their state
// and writer floor together.
type foundationEnvelope struct {
	SchemaVersion int             `json:"schemaVersion"`
	MinimumWriter int             `json:"minimumWriter"`
	State         json.RawMessage `json:"state"`
}

type foundationState struct {
	Version int `json:"version"`
}

type stableDataDir struct {
	requested string
	canonical string
	identity  string
	info      os.FileInfo
	handle    *os.File
	root      *os.Root
	lock      *os.File
	once      sync.Once
	err       error
}

var processWriters = struct {
	sync.Mutex
	held map[string]struct{}
}{held: map[string]struct{}{}}

func openStableDataDir(path string, readOnly bool) (_ *stableDataDir, err error) {
	return openStableDataDirWithClaimHook(path, readOnly, nil)
}

// openStableDataDirWithClaimHook keeps the production open path identical
// while allowing a deterministic identity change immediately after the OS
// writer lock is acquired. Production always passes a nil hook.
func openStableDataDirWithClaimHook(path string, readOnly bool, afterWriterLock func()) (_ *stableDataDir, err error) {
	requested, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	canonical, exists, err := resolveDataDir(requested)
	if err != nil {
		return nil, err
	}
	if !exists {
		if readOnly {
			return nil, os.ErrNotExist
		}
		if err := createDataDirFromStableParent(canonical); err != nil {
			return nil, err
		}
	}

	handle, err := os.Open(canonical)
	if err != nil {
		return nil, fmt.Errorf("open stable data directory handle: %w", err)
	}
	defer func() {
		if err != nil {
			_ = handle.Close()
		}
	}()
	info, err := handle.Stat()
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("data directory is not a directory: %s", canonical)
		}
		return nil, err
	}
	if err = verifySupportedFilesystem(handle); err != nil {
		return nil, err
	}
	identity, err := stableFileIdentity(handle, info)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, fmt.Errorf("open stable data directory root: %w", err)
	}
	defer func() {
		if err != nil {
			_ = root.Close()
		}
	}()
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, rootInfo) {
		return nil, fmt.Errorf("data directory changed while opening stable root")
	}
	d := &stableDataDir{requested: requested, canonical: canonical, identity: identity, info: info, handle: handle, root: root}
	if err = d.verifyIdentity(); err != nil {
		return nil, err
	}
	// Validation deliberately precedes lockfile/events/migration creation.
	if err = validateFoundation(root); err != nil {
		return nil, err
	}
	if !readOnly {
		if err = d.claimWriter(afterWriterLock); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func resolveDataDir(requested string) (canonical string, exists bool, err error) {
	if _, err = os.Lstat(requested); err == nil {
		canonical, err = filepath.EvalSymlinks(requested)
		if err != nil {
			return "", false, fmt.Errorf("resolve data directory: %w", err)
		}
		return filepath.Clean(canonical), true, nil
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("inspect data directory: %w", err)
	}
	missing := []string{}
	current := requested
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", false, fmt.Errorf("resolve data directory parent: %w", resolveErr)
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), false, nil
		} else if !os.IsNotExist(statErr) {
			return "", false, fmt.Errorf("inspect data directory parent: %w", statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, fmt.Errorf("data directory has no stable parent: %s", requested)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func createDataDirFromStableParent(path string) error {
	parent := filepath.Dir(path)
	for {
		if info, err := os.Stat(parent); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("data directory parent is not a directory: %s", parent)
			}
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return fmt.Errorf("data directory has no stable parent: %s", path)
		}
		parent = next
	}
	handle, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer handle.Close()
	if err := verifySupportedFilesystem(handle); err != nil {
		return err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	handleInfo, err := handle.Stat()
	if err != nil {
		return err
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(handleInfo, rootInfo) {
		return fmt.Errorf("data directory parent changed while opening stable root")
	}
	rel, err := filepath.Rel(parent, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("data directory escaped stable parent")
	}
	return root.MkdirAll(rel, 0o755)
}

func validateFoundation(root *os.Root) error {
	f, err := root.Open(foundationFileName)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Runtime foundation: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	var envelope foundationEnvelope
	if err := dec.Decode(&envelope); err != nil {
		return fmt.Errorf("decode Runtime foundation: %w", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return fmt.Errorf("decode Runtime foundation: %w", err)
	}
	if envelope.SchemaVersion != 1 || envelope.MinimumWriter < 0 || envelope.MinimumWriter > 1 {
		return fmt.Errorf("unsupported Runtime foundation schema/floor: schema=%d floor=%d", envelope.SchemaVersion, envelope.MinimumWriter)
	}
	stateDec := json.NewDecoder(strings.NewReader(string(envelope.State)))
	stateDec.DisallowUnknownFields()
	var state foundationState
	if err := stateDec.Decode(&state); err != nil || state.Version != 1 {
		return fmt.Errorf("invalid Runtime foundation state")
	}
	if err := requireJSONEOF(stateDec); err != nil {
		return fmt.Errorf("invalid Runtime foundation state: %w", err)
	}
	return nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

func (d *stableDataDir) claimWriter(afterWriterLock func()) (err error) {
	processWriters.Lock()
	if _, exists := processWriters.held[d.identity]; exists {
		processWriters.Unlock()
		return fmt.Errorf("data directory already has a writable CodexLoom process: %s", d.canonical)
	}
	processWriters.held[d.identity] = struct{}{}
	processWriters.Unlock()
	claimComplete := false
	var lock *os.File
	lockHeld := false
	defer func() {
		if claimComplete {
			return
		}
		var cleanupErr error
		if lockHeld {
			cleanupErr = unlockWriterFile(lock)
		}
		if lock != nil {
			cleanupErr = errors.Join(cleanupErr, lock.Close())
		}
		d.lock = nil
		processWriters.Lock()
		delete(processWriters.held, d.identity)
		processWriters.Unlock()
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("release failed data directory writer claim: %w", cleanupErr))
		}
	}()
	if info, err := d.root.Lstat(".codex-loom-writer.lock"); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("data directory writer lease is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect data directory writer lease: %w", err)
	}
	lock, err = d.root.OpenFile(".codex-loom-writer.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open data directory writer lease: %w", err)
	}
	if err := lockWriterFile(lock); err != nil {
		return fmt.Errorf("data directory already has a writable CodexLoom process: %s: %w", d.canonical, err)
	}
	lockHeld = true
	d.lock = lock
	if afterWriterLock != nil {
		afterWriterLock()
	}
	if err := d.verifyIdentity(); err != nil {
		return err
	}
	claimComplete = true
	return nil
}

func (d *stableDataDir) verifyIdentity() error {
	if d == nil || d.handle == nil || d.root == nil || d.info == nil {
		return fmt.Errorf("stable data directory handle is unavailable")
	}
	handleInfo, err := d.handle.Stat()
	if err != nil || !os.SameFile(d.info, handleInfo) {
		return fmt.Errorf("data directory handle identity changed")
	}
	current, err := os.Stat(d.canonical)
	if err != nil || !os.SameFile(d.info, current) {
		return fmt.Errorf("data directory canonical identity changed: %s", d.canonical)
	}
	resolved, _, err := resolveDataDir(d.requested)
	if err != nil {
		return fmt.Errorf("data directory bootstrap path changed: %w", err)
	}
	requestedInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(d.info, requestedInfo) {
		return fmt.Errorf("data directory bootstrap path identity changed: %s", d.requested)
	}
	if err := verifySupportedFilesystem(d.handle); err != nil {
		return err
	}
	identity, err := stableFileIdentity(d.handle, handleInfo)
	if err != nil || identity != d.identity {
		return fmt.Errorf("data directory filesystem identity changed")
	}
	return nil
}

func (d *stableDataDir) close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		if d.lock != nil {
			if err := unlockWriterFile(d.lock); err != nil {
				d.err = err
			}
			if err := d.lock.Close(); d.err == nil {
				d.err = err
			}
			processWriters.Lock()
			delete(processWriters.held, d.identity)
			processWriters.Unlock()
		}
		if err := d.root.Close(); d.err == nil {
			d.err = err
		}
		if err := d.handle.Close(); d.err == nil {
			d.err = err
		}
	})
	return d.err
}
