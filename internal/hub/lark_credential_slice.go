package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/credentials"
)

// LarkCredentialMigration is the non-secret result of the narrow Lark
// credential migration flow. No secret material ever appears in it.
type LarkCredentialMigration struct {
	ConnectionID    string `json:"connectionId"`
	PreviousRef     string `json:"previousRef"`
	CurrentRef      string `json:"currentRef"`
	FloorRaised     bool   `json:"floorRaised"`
	DryRun          bool   `json:"dryRun"`
	AlreadyMigrated bool   `json:"alreadyMigrated"`
	RequiresProof   bool   `json:"requiresProof"`
}

type larkCredentialMigrationRecord struct {
	Version      int    `json:"version"`
	ConnectionID string `json:"connectionId"`
	PreviousRef  string `json:"previousRef"`
	CurrentRef   string `json:"currentRef"`
	Phase        string `json:"phase"` // prepared | completed | ref_restored
	MigratedAt   string `json:"migratedAt"`
}

const (
	larkMigrationPhasePrepared    = "prepared"
	larkMigrationPhaseCompleted   = "completed"
	larkMigrationPhaseRefRestored = "ref_restored"
)

func validCredentialRef(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "env:") || strings.HasPrefix(value, "keychain:") {
		return true
	}
	return credentials.IsManagedRef(value)
}

// larkMigrationRecordPath is deliberately outside the backup-excluded
// credentials directory: the non-secret record must survive ordinary backups
// so a restored data directory can still roll back to the legacy reference.
func larkMigrationRecordPath(connectionID string) string {
	return filepath.Join("lark-migrations", connectionID+".json")
}

// MigrateLarkCredential is the narrow local operator flow for one Lark
// Connection. A durable per-Connection record with a phase is written before
// the credential reference effect, so a crash at any step is reconcilable and
// idempotent; failure stops in an explicit state and never changes Connection
// identity, Address, Membership, Conversation, Inbox, or Outbox history.
func (h *Hub) MigrateLarkCredential(_ context.Context, connectionID string, secret []byte, dryRun bool) (LarkCredentialMigration, error) {
	result, connection, err := h.larkMigrationBaseline(connectionID)
	if err != nil {
		return result, err
	}
	record, recordErr := h.readLarkMigrationRecord(connectionID)
	recordFound := recordErr == nil
	if recordErr != nil && !os.IsNotExist(recordErr) {
		return result, errf(409, "migration record is unreadable: %s", recordErr)
	}
	if credentials.IsManagedRef(connection.CredentialRef) {
		if !recordFound || record.CurrentRef != connection.CredentialRef {
			return result, errf(409, "managed credential reference has no durable migration record; run rollback guidance or re-migrate")
		}
		switch record.Phase {
		case larkMigrationPhaseCompleted:
			if _, err := h.resolveManagedCredential(connection.CredentialRef); err != nil {
				return result, errf(409, "managed credential is dangling: %s", err)
			}
			result.CurrentRef = connection.CredentialRef
			result.AlreadyMigrated = true
			result.FloorRaised = h.st.CredentialFloorPresent()
			return result, nil
		case larkMigrationPhasePrepared:
			// Crash after the reference effect before the phase advanced.
			if err := h.advanceLarkMigrationPhase(connectionID, larkMigrationPhaseCompleted); err != nil {
				return result, errf(500, "reconcile migration record: %s", err)
			}
			result.CurrentRef = connection.CredentialRef
			result.AlreadyMigrated = true
			result.FloorRaised = h.st.CredentialFloorPresent()
			return result, nil
		default:
			return result, errf(409, "migration record phase %q is inconsistent with the connection reference", record.Phase)
		}
	}
	if recordFound && record.Phase == larkMigrationPhaseRefRestored {
		return result, errf(409, "migration rollback cleanup is pending; run rollback to finish")
	}
	if recordFound && record.Phase == larkMigrationPhasePrepared && connection.CredentialRef == record.PreviousRef {
		// Crash after Put and record write but before the reference effect.
		if _, err := h.updateLarkConnection(connectionID, record.CurrentRef); err != nil {
			return result, errf(500, "resume migration reference update: %s", err)
		}
		if err := h.advanceLarkMigrationPhase(connectionID, larkMigrationPhaseCompleted); err != nil {
			return result, errf(500, "reconcile migration record: %s", err)
		}
		result.PreviousRef = record.PreviousRef
		result.CurrentRef = record.CurrentRef
		result.AlreadyMigrated = true
		result.FloorRaised = h.st.CredentialFloorPresent()
		return result, nil
	}
	if err := h.larkGatewayMigrationBlocker(connectionID); err != nil {
		return result, err
	}
	result.PreviousRef = connection.CredentialRef
	if dryRun {
		result.DryRun = true
		result.FloorRaised = h.st.CredentialFloorPresent()
		return result, nil
	}
	if len(secret) == 0 {
		return result, errf(400, "credential secret is required")
	}
	if !h.st.CredentialFloorPresent() {
		if err := h.st.SaveCredentialFloor(); err != nil {
			return result, errf(500, "raise credential floor: %s", err)
		}
		result.FloorRaised = true
	}
	credentialStore, err := credentials.New(h.st)
	if err != nil {
		return result, errf(500, "open managed credential store: %s", err)
	}
	ref, err := credentialStore.Put(secret)
	if err != nil {
		return result, errf(500, "persist managed credential: %s", err)
	}
	result.CurrentRef = string(ref)
	record = larkCredentialMigrationRecord{
		Version: 1, ConnectionID: connectionID, PreviousRef: result.PreviousRef,
		CurrentRef: result.CurrentRef, Phase: larkMigrationPhasePrepared, MigratedAt: now(),
	}
	if err := h.writeLarkMigrationRecord(record); err != nil {
		_ = credentialStore.Delete(ref)
		return result, errf(500, "persist migration record: %s (managed credential deleted; credential floor stays raised)", err)
	}
	if _, err := h.updateLarkConnection(connectionID, result.CurrentRef); err != nil {
		_ = credentialStore.Delete(ref)
		_ = h.removeLarkMigrationRecord(connectionID)
		return result, errf(500, "update connection credential reference: %s (managed credential and record removed; credential floor stays raised)", err)
	}
	if err := h.advanceLarkMigrationPhase(connectionID, larkMigrationPhaseCompleted); err != nil {
		return result, fmt.Errorf("migrated but migration record phase is pending: %w", err)
	}
	return result, nil
}

// VerifyLarkCredential confirms the managed credential resolves and the
// Connection references it, and that the R1 Gateway lifecycle has accepted an
// exact fresh process proof. Without proof it fails closed instead of printing
// a false green.
func (h *Hub) VerifyLarkCredential(connectionID string) (LarkCredentialMigration, error) {
	result, connection, err := h.larkMigrationBaseline(connectionID)
	if err != nil {
		return result, err
	}
	if !credentials.IsManagedRef(connection.CredentialRef) {
		return result, errf(409, "connection does not reference a managed credential")
	}
	record, err := h.readLarkMigrationRecord(connectionID)
	if err != nil {
		return result, errf(409, "migration record is unavailable: %s", err)
	}
	if record.CurrentRef != connection.CredentialRef {
		return result, errf(409, "migration record does not match the connection reference")
	}
	if record.Phase != larkMigrationPhaseCompleted {
		return result, errf(409, "migration is not complete (phase %q); run migrate", record.Phase)
	}
	if _, err := h.resolveManagedCredential(connection.CredentialRef); err != nil {
		return result, errf(409, "managed credential does not resolve: %s", err)
	}
	if err := h.requireAcceptedGatewayProof(connectionID); err != nil {
		result.RequiresProof = true
		return result, errf(409, "managed credential resolves but the Gateway lifecycle has no accepted fresh exact proof: %s", err)
	}
	result.PreviousRef = record.PreviousRef
	result.CurrentRef = connection.CredentialRef
	result.AlreadyMigrated = true
	result.FloorRaised = h.st.CredentialFloorPresent()
	return result, nil
}

// RollbackLarkCredential restores the previous credential reference, deletes
// the managed credential, and removes the migration record. Every step is
// idempotent and resumable; the credential writer floor is never lowered.
func (h *Hub) RollbackLarkCredential(connectionID string) (LarkCredentialMigration, error) {
	result, connection, err := h.larkMigrationBaseline(connectionID)
	if err != nil {
		return result, err
	}
	record, err := h.readLarkMigrationRecord(connectionID)
	if err != nil {
		if os.IsNotExist(err) {
			if credentials.IsManagedRef(connection.CredentialRef) {
				return result, errf(409, "managed credential reference has no migration record; previous reference is unrecoverable")
			}
			return result, errf(409, "connection is not migrated")
		}
		return result, errf(409, "migration record is unreadable: %s", err)
	}
	if record.CurrentRef == "" || record.PreviousRef == "" {
		return result, errf(409, "migration record is incomplete")
	}
	if connection.CredentialRef == record.CurrentRef {
		if strings.TrimSpace(record.PreviousRef) == "" {
			return result, errf(409, "rollback requires a non-empty previous credential reference")
		}
		if _, err := h.updateLarkConnection(connectionID, record.PreviousRef); err != nil {
			return result, errf(500, "restore previous credential reference: %s", err)
		}
		if err := h.advanceLarkMigrationPhase(connectionID, larkMigrationPhaseRefRestored); err != nil {
			return result, errf(500, "persist rollback phase: %s", err)
		}
	}
	credentialStore, err := credentials.New(h.st)
	if err != nil {
		return result, errf(500, "open managed credential store: %s", err)
	}
	if err := credentialStore.Delete(credentials.Ref(record.CurrentRef)); err != nil {
		return result, errf(500, "credential reference restored but managed credential deletion failed: %s", err)
	}
	if err := h.removeLarkMigrationRecord(connectionID); err != nil {
		return result, errf(500, "managed credential deleted but migration record removal failed: %s", err)
	}
	result.PreviousRef = record.PreviousRef
	result.CurrentRef = record.PreviousRef
	result.FloorRaised = h.st.CredentialFloorPresent()
	return result, nil
}

func (h *Hub) larkMigrationBaseline(connectionID string) (LarkCredentialMigration, PlatformConnection, error) {
	if h == nil || h.st == nil || h.passive || h.st.ReadOnly() || !h.st.HasLiveWritableOwner() {
		return LarkCredentialMigration{}, PlatformConnection{}, errf(409, "managed credential migration requires a live writable Hub")
	}
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return LarkCredentialMigration{}, PlatformConnection{}, errf(400, "connection id is required")
	}
	h.mu.Lock()
	connection := h.connections[connectionID]
	h.mu.Unlock()
	if connection == nil {
		return LarkCredentialMigration{}, PlatformConnection{}, errf(404, "connection not found: %s", connectionID)
	}
	provider := strings.ToLower(strings.TrimSpace(connection.Provider))
	if provider != "lark" && provider != "feishu" {
		return LarkCredentialMigration{}, PlatformConnection{}, errf(409, "connection %s is not a Lark connection", connectionID)
	}
	if connection.ArchivedAt != "" {
		return LarkCredentialMigration{}, PlatformConnection{}, errf(409, "connection is archived")
	}
	return LarkCredentialMigration{ConnectionID: connectionID}, *connection, nil
}

func (h *Hub) larkGatewayMigrationBlocker(connectionID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	control := h.gatewayState.Controls[connectionID]
	if control == nil {
		return nil
	}
	if control.ActiveAttemptID != "" {
		return errf(409, "Gateway process attempt is active")
	}
	if control.Recovery != gatewayRecoveryNone {
		return errf(409, "Gateway recovery is required before migration")
	}
	binding, err := h.gatewayBindingLocked(connectionID)
	if err != nil {
		return errf(409, "Gateway binding is unavailable")
	}
	if !gatewayBindingsEqual(binding, control.Binding) {
		return errf(409, "Gateway binding drift requires reconciliation before migration")
	}
	return nil
}

func (h *Hub) requireAcceptedGatewayProof(connectionID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	control := h.gatewayState.Controls[connectionID]
	if control == nil {
		return fmt.Errorf("connection has no adopted Gateway lifecycle")
	}
	if control.ActiveAttemptID != "" {
		return fmt.Errorf("Gateway process attempt is active")
	}
	if control.Recovery != gatewayRecoveryNone {
		return fmt.Errorf("Gateway recovery is %q", control.Recovery)
	}
	attempt := h.gatewayState.Attempts[connectionID]
	if attempt == nil || attempt.AcceptedProof == nil || !gatewayAttemptTerminal(attempt.Phase) {
		return fmt.Errorf("no accepted exact process proof")
	}
	observedAt, err := time.Parse(time.RFC3339Nano, attempt.AcceptedProof.ObservedAt)
	if err != nil || time.Since(observedAt) > gatewayProcessProofFreshness {
		return fmt.Errorf("accepted process proof is stale")
	}
	return nil
}

func (h *Hub) resolveManagedCredential(ref string) ([]byte, error) {
	if !credentials.IsManagedRef(ref) {
		return nil, fmt.Errorf("not a managed credential reference")
	}
	credentialStore, err := credentials.New(h.st)
	if err != nil {
		return nil, err
	}
	return credentialStore.Resolve(credentials.Ref(ref))
}

func (h *Hub) updateLarkConnection(connectionID, credentialRef string) (PlatformConnection, error) {
	if h.larkUpdateConnectionForTest != nil {
		return h.larkUpdateConnectionForTest(connectionID, credentialRef)
	}
	return h.UpdateConnection(connectionID, ConnectionParams{CredentialRef: credentialRef})
}

func (h *Hub) advanceLarkMigrationPhase(connectionID, phase string) error {
	record, err := h.readLarkMigrationRecord(connectionID)
	if err != nil {
		return err
	}
	record.Phase = phase
	return h.writeLarkMigrationRecord(record)
}

func (h *Hub) writeLarkMigrationRecord(record larkCredentialMigrationRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	relative := larkMigrationRecordPath(record.ConnectionID)
	return h.st.WithStableWriteRoot(func(root *os.Root) error {
		if err := root.MkdirAll("lark-migrations", 0o700); err != nil {
			return err
		}
		temporary := filepath.Join("lark-migrations", ".lark-migration-"+randomHubSuffix()+".tmp")
		file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			_ = file.Close()
			if !committed {
				_ = root.Remove(temporary)
			}
		}()
		if _, err := file.Write(payload); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := root.Rename(temporary, relative); err != nil {
			return err
		}
		committed = true
		directory, err := root.Open("lark-migrations")
		if err != nil {
			return err
		}
		defer directory.Close()
		return directory.Sync()
	})
}

func (h *Hub) readLarkMigrationRecord(connectionID string) (larkCredentialMigrationRecord, error) {
	data, err := h.st.ReadStableFile(larkMigrationRecordPath(connectionID))
	if os.IsNotExist(err) {
		return larkCredentialMigrationRecord{}, err
	}
	if err != nil {
		return larkCredentialMigrationRecord{}, err
	}
	var record larkCredentialMigrationRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return larkCredentialMigrationRecord{}, fmt.Errorf("invalid migration record")
	}
	if record.Version != 1 || record.ConnectionID != connectionID || record.CurrentRef == "" {
		return larkCredentialMigrationRecord{}, fmt.Errorf("invalid migration record")
	}
	switch record.Phase {
	case larkMigrationPhasePrepared, larkMigrationPhaseCompleted, larkMigrationPhaseRefRestored:
	default:
		return larkCredentialMigrationRecord{}, fmt.Errorf("invalid migration record phase")
	}
	return record, nil
}

func (h *Hub) removeLarkMigrationRecord(connectionID string) error {
	return h.st.WithStableWriteRoot(func(root *os.Root) error {
		if err := root.Remove(larkMigrationRecordPath(connectionID)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

func randomHubSuffix() string {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(random)
}
