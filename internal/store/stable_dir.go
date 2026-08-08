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

const runtimeFoundationMaxBytes = 1 << 20

const (
	runtimeFoundationSchemaVersion = 1
	runtimeWriterFloorS0           = 1
	runtimeWriterFloorGatewayState = 2
)

// runtimeFoundationEnvelope is private Runtime persistence shared by Store
// and Hub. It is not an API or provider wire contract. S0 recognizes version
// 1 as the empty foundation. R0b uses version 2 only when an internal caller
// explicitly creates the first dormant Gateway control; that same write raises
// the minimum writer floor atomically.
type runtimeFoundationEnvelope struct {
	SchemaVersion int             `json:"schemaVersion"`
	MinimumWriter int             `json:"minimumWriter"`
	State         json.RawMessage `json:"state"`
}

type foundationState struct {
	Version      int             `json:"version"`
	GatewayState json.RawMessage `json:"gatewayState,omitempty"`
}

type gatewayFoundationStateShape struct {
	Version      int                                           `json:"version"`
	Controls     map[string]*gatewayFoundationControlShape     `json:"controls"`
	Observations map[string]*gatewayFoundationObservationShape `json:"observations"`
}

type gatewayFoundationControlShape struct {
	ConnectionID string                        `json:"connectionId"`
	Epoch        uint64                        `json:"epoch"`
	Lifecycle    string                        `json:"lifecycle"`
	Recovery     string                        `json:"recovery"`
	Reason       string                        `json:"reason,omitempty"`
	Binding      gatewayFoundationBindingShape `json:"binding"`
	UpdatedAt    string                        `json:"updatedAt"`
}

type gatewayFoundationBindingShape struct {
	Connection gatewayFoundationConnectionShape `json:"connection"`
	Addresses  []gatewayFoundationAddressShape  `json:"addresses"`
}

type gatewayFoundationConnectionShape struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	AccountRef    string   `json:"accountRef,omitempty"`
	ScopeRef      string   `json:"scopeRef,omitempty"`
	CredentialRef string   `json:"credentialRef,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Enabled       bool     `json:"enabled"`
	SupersededBy  string   `json:"supersededBy,omitempty"`
	ArchivedAt    string   `json:"archivedAt,omitempty"`
	CreatedAt     string   `json:"createdAt"`
}

type gatewayFoundationAddressShape struct {
	ID                 string   `json:"id"`
	AgentID            string   `json:"agentId"`
	ConnectionID       string   `json:"connectionId"`
	ExternalIdentity   string   `json:"externalIdentity"`
	DisplayName        string   `json:"displayName,omitempty"`
	TriggerPolicy      string   `json:"triggerPolicy"`
	ReplyPolicy        string   `json:"replyPolicy"`
	DMPolicy           string   `json:"dmPolicy,omitempty"`
	TrustDomain        string   `json:"trustDomain"`
	AllowActors        []string `json:"allowActors,omitempty"`
	AllowConversations []string `json:"allowConversations,omitempty"`
	BlockActors        []string `json:"blockActors,omitempty"`
	BlockConversations []string `json:"blockConversations,omitempty"`
	Enabled            bool     `json:"enabled"`
	SupersededBy       string   `json:"supersededBy,omitempty"`
	ArchivedAt         string   `json:"archivedAt,omitempty"`
	DeletedAt          string   `json:"deletedAt,omitempty"`
	Version            int      `json:"version"`
	CreatedAt          string   `json:"createdAt"`
}

type gatewayFoundationObservationShape struct {
	ConnectionID         string   `json:"connectionId"`
	Sequence             uint64   `json:"sequence"`
	Status               string   `json:"status"`
	Error                string   `json:"error,omitempty"`
	Cursor               string   `json:"cursor,omitempty"`
	LastEventAt          string   `json:"lastEventAt,omitempty"`
	ObservedCapabilities []string `json:"observedCapabilities,omitempty"`
	HeartbeatAt          string   `json:"heartbeatAt,omitempty"`
	ObservedAt           string   `json:"observedAt"`
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
	dec := json.NewDecoder(io.LimitReader(f, runtimeFoundationMaxBytes))
	dec.DisallowUnknownFields()
	var envelope runtimeFoundationEnvelope
	if err := dec.Decode(&envelope); err != nil {
		return fmt.Errorf("decode Runtime foundation: %w", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return fmt.Errorf("decode Runtime foundation: %w", err)
	}
	if envelope.SchemaVersion != runtimeFoundationSchemaVersion {
		return fmt.Errorf("unsupported Runtime foundation schema/floor: schema=%d floor=%d", envelope.SchemaVersion, envelope.MinimumWriter)
	}
	stateDec := json.NewDecoder(strings.NewReader(string(envelope.State)))
	stateDec.DisallowUnknownFields()
	var state foundationState
	if err := stateDec.Decode(&state); err != nil {
		return fmt.Errorf("invalid Runtime foundation state")
	}
	if err := requireJSONEOF(stateDec); err != nil {
		return fmt.Errorf("invalid Runtime foundation state: %w", err)
	}
	switch state.Version {
	case 1:
		if envelope.MinimumWriter < 0 || envelope.MinimumWriter > runtimeWriterFloorS0 || len(state.GatewayState) != 0 {
			return fmt.Errorf("unsupported Runtime foundation schema/floor: schema=%d floor=%d", envelope.SchemaVersion, envelope.MinimumWriter)
		}
	case 2:
		if envelope.MinimumWriter != runtimeWriterFloorGatewayState || len(state.GatewayState) == 0 {
			return fmt.Errorf("unsupported Runtime foundation schema/floor: schema=%d floor=%d", envelope.SchemaVersion, envelope.MinimumWriter)
		}
		if err := validateGatewayFoundationState(state.GatewayState); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid Runtime foundation state version %d", state.Version)
	}
	return nil
}

func validateGatewayFoundationState(raw json.RawMessage) error {
	gatewayDec := json.NewDecoder(strings.NewReader(string(raw)))
	gatewayDec.DisallowUnknownFields()
	var gateway gatewayFoundationStateShape
	if err := gatewayDec.Decode(&gateway); err != nil {
		return fmt.Errorf("invalid Runtime Gateway foundation state")
	}
	if err := requireJSONEOF(gatewayDec); err != nil {
		return fmt.Errorf("invalid Runtime Gateway foundation state: %w", err)
	}
	if gateway.Version != 1 || gateway.Controls == nil || gateway.Observations == nil {
		return fmt.Errorf("invalid Runtime Gateway foundation state version")
	}
	for id, control := range gateway.Controls {
		if id == "" || control == nil || control.ConnectionID != id || control.Epoch == 0 ||
			(control.Lifecycle != "provisioning" && control.Lifecycle != "adopted") ||
			(control.Recovery != "none" && control.Recovery != "needs_reconcile" && control.Recovery != "manual_recovery_required") ||
			control.Binding.Connection.ID != id || control.Binding.Connection.Provider == "" ||
			!foundationStringsCanonical(control.Binding.Connection.Capabilities, true) {
			return fmt.Errorf("invalid Runtime Gateway control %q", id)
		}
		previousAddressID := ""
		for _, address := range control.Binding.Addresses {
			if address.ID == "" || address.ID <= previousAddressID || address.ConnectionID != id || address.AgentID == "" || address.ExternalIdentity == "" || address.Version < 1 ||
				!foundationStringsCanonical(address.AllowActors, false) || !foundationStringsCanonical(address.AllowConversations, false) ||
				!foundationStringsCanonical(address.BlockActors, false) || !foundationStringsCanonical(address.BlockConversations, false) {
				return fmt.Errorf("invalid Runtime Gateway Address binding %q", address.ID)
			}
			previousAddressID = address.ID
		}
	}
	for id, observation := range gateway.Observations {
		if id == "" || observation == nil || observation.ConnectionID != id || observation.Sequence == 0 || gateway.Controls[id] == nil ||
			(observation.Status != "disconnected" && observation.Status != "connecting" && observation.Status != "connected" && observation.Status != "degraded") ||
			!foundationStringsCanonical(observation.ObservedCapabilities, true) {
			return fmt.Errorf("invalid Runtime Gateway observation %q", id)
		}
	}
	return nil
}

func foundationStringsCanonical(values []string, lower bool) bool {
	previous := ""
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		if lower {
			normalized = strings.ToLower(normalized)
		}
		if value != normalized || value == "" || value <= previous {
			return false
		}
		previous = value
	}
	return true
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
