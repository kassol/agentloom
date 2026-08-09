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

	"github.com/yan5xu/codex-loom/internal/credentials"
)

// LarkCredentialMigration is the non-secret result of the narrow Lark
// credential migration flow. No secret material ever appears in it.
type LarkCredentialMigration struct {
	ConnectionID   string `json:"connectionId"`
	PreviousRef    string `json:"previousRef"`
	CurrentRef     string `json:"currentRef"`
	FloorRaised    bool   `json:"floorRaised"`
	DryRun         bool   `json:"dryRun"`
	AlreadyMigrated bool  `json:"alreadyMigrated"`
}

type larkCredentialMigrationRecord struct {
	Version      int    `json:"version"`
	ConnectionID string `json:"connectionId"`
	PreviousRef  string `json:"previousRef"`
	CurrentRef   string `json:"currentRef"`
	MigratedAt   string `json:"migratedAt"`
}

func validCredentialRef(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "env:") || strings.HasPrefix(value, "keychain:") {
		return true
	}
	return credentials.IsManagedRef(value)
}

func larkMigrationRecordPath(connectionID string) string {
	return filepath.Join("credentials", "lark-migration-"+connectionID+".json")
}

// MigrateLarkCredential is the narrow local operator flow for one Lark
// Connection: preflight/dry-run, floor raise, C-v1 managed Put, credential
// reference update, and a durable non-secret migration record. It is
// idempotent; failure stops in an explicit state and never changes Connection
// identity, Address, Membership, Conversation, Inbox, or Outbox history.
func (h *Hub) MigrateLarkCredential(_ context.Context, connectionID string, secret []byte, dryRun bool) (LarkCredentialMigration, error) {
	result, connection, err := h.larkMigrationBaseline(connectionID)
	if err != nil {
		return result, err
	}
	if credentials.IsManagedRef(connection.CredentialRef) {
		if _, err := h.resolveManagedCredential(connection.CredentialRef); err != nil {
			return result, errf(409, "managed credential reference is dangling: %s", err)
		}
		result.CurrentRef = connection.CredentialRef
		result.AlreadyMigrated = true
		result.FloorRaised = h.st.CredentialFloorPresent()
		return result, nil
	}
	if len(secret) == 0 {
		return result, errf(400, "credential secret is required")
	}
	result.PreviousRef = connection.CredentialRef
	if dryRun {
		result.DryRun = true
		result.FloorRaised = h.st.CredentialFloorPresent()
		return result, nil
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
	if _, err := h.updateLarkConnection(connectionID, result.CurrentRef); err != nil {
		_ = credentialStore.Delete(ref)
		return result, errf(500, "update connection credential reference: %s (managed credential deleted; credential floor stays raised)", err)
	}
	record := larkCredentialMigrationRecord{
		Version: 1, ConnectionID: connectionID, PreviousRef: result.PreviousRef,
		CurrentRef: result.CurrentRef, MigratedAt: now(),
	}
	if err := h.writeLarkMigrationRecord(record); err != nil {
		_, rollbackErr := h.updateLarkConnection(connectionID, result.PreviousRef)
		if rollbackErr == nil {
			_ = credentialStore.Delete(ref)
		}
		return result, errf(500, "persist migration record: %s (connection %s)", err, map[bool]string{true: "reverted", false: "rollback incomplete"}[rollbackErr == nil])
	}
	return result, nil
}

// VerifyLarkCredential confirms the managed credential resolves and the
// Connection references it. It never reports a healthy projection while a
// recovery latch or active Gateway attempt exists.
func (h *Hub) VerifyLarkCredential(connectionID string) (LarkCredentialMigration, error) {
	result, connection, err := h.larkMigrationBaseline(connectionID)
	if err != nil {
		return result, err
	}
	if !credentials.IsManagedRef(connection.CredentialRef) {
		return result, errf(409, "connection does not reference a managed credential")
	}
	if _, err := h.resolveManagedCredential(connection.CredentialRef); err != nil {
		return result, errf(409, "managed credential does not resolve: %s", err)
	}
	record, err := h.readLarkMigrationRecord(connectionID)
	if err != nil {
		return result, errf(409, "migration record is unavailable: %s", err)
	}
	if record.CurrentRef != connection.CredentialRef {
		return result, errf(409, "migration record does not match the connection reference")
	}
	result.PreviousRef = record.PreviousRef
	result.CurrentRef = connection.CredentialRef
	result.AlreadyMigrated = true
	result.FloorRaised = h.st.CredentialFloorPresent()
	return result, nil
}

// RollbackLarkCredential restores the previous credential reference, deletes
// the managed credential, and removes the migration record. The credential
// writer floor is never lowered. An empty previous reference is refused
// without side effects.
func (h *Hub) RollbackLarkCredential(connectionID string) (LarkCredentialMigration, error) {
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
	if strings.TrimSpace(record.PreviousRef) == "" {
		return result, errf(409, "rollback requires a non-empty previous credential reference")
	}
	if _, err := h.updateLarkConnection(connectionID, record.PreviousRef); err != nil {
		return result, errf(500, "restore previous credential reference: %s", err)
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

func (h *Hub) writeLarkMigrationRecord(record larkCredentialMigrationRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	relative := larkMigrationRecordPath(record.ConnectionID)
	return h.st.WithStableWriteRoot(func(root *os.Root) error {
		if err := root.MkdirAll("credentials", 0o700); err != nil {
			return err
		}
		temporary := filepath.Join("credentials", ".lark-migration-"+randomHubSuffix()+".tmp")
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
		directory, err := root.Open("credentials")
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
