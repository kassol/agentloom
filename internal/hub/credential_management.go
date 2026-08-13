package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yan5xu/codex-loom/internal/credentials"
	"github.com/yan5xu/codex-loom/internal/feishu"
	githubcredential "github.com/yan5xu/codex-loom/internal/github"
	"github.com/yan5xu/codex-loom/internal/parall"
	loomslack "github.com/yan5xu/codex-loom/internal/slack"
)

const credentialReceiptDirectory = "credential-receipts"

const (
	credentialReceiptPrepared               = "prepared"
	credentialReceiptCompleted              = "completed"
	credentialReceiptRollbackPending        = "rollback_pending"
	credentialReceiptRolledBack             = "rolled_back"
	credentialReceiptManualRecoveryRequired = "manual_recovery_required"
)

// CredentialPreflight is a non-secret migration assessment for one Connection.
type CredentialPreflight struct {
	ConnectionID  string `json:"connectionId"`
	Provider      string `json:"provider"`
	CredentialRef string `json:"credentialRef"`
	Eligible      bool   `json:"eligible"`
	Reason        string `json:"reason,omitempty"`
}

// CredentialReceipt is durable, non-secret evidence for one Connection's
// migration. It intentionally survives ordinary backups while credential
// material remains excluded.
type CredentialReceipt struct {
	Version       int    `json:"version"`
	ID            string `json:"id"`
	ConnectionID  string `json:"connectionId"`
	Provider      string `json:"provider"`
	PreviousRef   string `json:"previousRef"`
	ManagedRef    string `json:"managedRef"`
	Status        string `json:"status"`
	DryRun        bool   `json:"dryRun,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

var loadKeychainCredentialForMigration = loadProviderKeychainCredential

// CredentialPreflightList enumerates every enabled, non-archived,
// Keychain-backed Connection. When connectionID is supplied it returns that
// Connection even when ineligible so env: references are explicitly reported
// as not auto-migrated.
func (h *Hub) CredentialPreflightList(connectionID string) ([]CredentialPreflight, error) {
	connectionID = strings.TrimSpace(connectionID)
	connections := h.ListConnections()
	out := make([]CredentialPreflight, 0)
	for _, connection := range connections {
		if connectionID != "" && connection.ID != connectionID {
			continue
		}
		if connection.ArchivedAt != "" || !connection.Enabled {
			if connectionID != "" {
				out = append(out, CredentialPreflight{
					ConnectionID: connection.ID, Provider: connection.Provider, CredentialRef: connection.CredentialRef,
					Reason: "connection is disabled or archived",
				})
			}
			continue
		}
		ref := strings.TrimSpace(connection.CredentialRef)
		entry := CredentialPreflight{ConnectionID: connection.ID, Provider: connection.Provider, CredentialRef: ref}
		switch {
		case strings.HasPrefix(ref, "keychain:"):
			entry.Eligible = true
		case strings.HasPrefix(ref, "env:"):
			entry.Reason = "environment references are not auto-migrated"
		case credentials.IsManagedRef(ref):
			entry.Reason = "connection already uses a managed credential"
		default:
			entry.Reason = "connection has no supported credential reference"
		}
		if connectionID != "" || entry.Eligible {
			out = append(out, entry)
		}
	}
	if connectionID != "" && len(out) == 0 {
		return nil, errf(404, "connection not found: %s", connectionID)
	}
	return out, nil
}

// PutManagedCredential writes provider credential material to the owner-only
// managed store and returns only its opaque canonical reference.
func (h *Hub) PutManagedCredential(provider string, values map[string]string) (string, error) {
	if h == nil || h.st == nil || h.passive || h.st.ReadOnly() || !h.st.HasLiveWritableOwner() {
		return "", errf(409, "managed credential writes require a live writable Hub")
	}
	payload, err := credentials.EncodePayload(provider, values)
	if err != nil {
		return "", errf(400, "%s", err)
	}
	if !h.st.CredentialFloorPresent() {
		if err := h.st.SaveCredentialFloor(); err != nil {
			return "", errf(500, "raise credential floor: %s", err)
		}
	}
	credentialStore, err := credentials.New(h.st)
	if err != nil {
		return "", errf(500, "open managed credential store: %s", err)
	}
	ref, err := credentialStore.Put(payload)
	if err != nil {
		return "", errf(500, "persist managed credential: %s", err)
	}
	return string(ref), nil
}

// ResolveManagedCredential decodes one provider payload without projecting
// credential values into an API response or durable Connection.
func (h *Hub) ResolveManagedCredential(ref, provider string) (map[string]string, error) {
	data, err := h.resolveManagedCredential(ref)
	if err != nil {
		return nil, err
	}
	return credentials.DecodePayload(data, provider)
}

// DeleteManagedCredential is compensation for a failed onboarding transaction.
func (h *Hub) DeleteManagedCredential(ref string) error {
	if !credentials.IsManagedRef(ref) {
		return fmt.Errorf("not a managed credential reference")
	}
	if h.deleteManagedCredentialForTest != nil {
		return h.deleteManagedCredentialForTest(ref)
	}
	credentialStore, err := credentials.New(h.st)
	if err != nil {
		return err
	}
	err = credentialStore.Delete(credentials.Ref(ref))
	if credentials.IsCredentialNotFound(err) {
		return nil
	}
	return err
}

// MigrateCredential atomically advances one Keychain-backed Connection to a
// managed credential with a durable idempotent receipt. Keychain remains an
// undeleted compatibility source.
func (h *Hub) MigrateCredential(_ context.Context, connectionID string, dryRun bool, confirm string) (CredentialReceipt, error) {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return CredentialReceipt{}, errf(400, "connection id is required")
	}
	unlock := h.lockCredentialConnection(connectionID)
	defer unlock()

	connection, err := h.credentialConnection(connectionID)
	if err != nil {
		return CredentialReceipt{}, err
	}
	receiptID := credentialReceiptID(connectionID)
	receipt, receiptErr := h.readCredentialReceipt(receiptID)
	if receiptErr == nil {
		switch receipt.Status {
		case credentialReceiptCompleted:
			if connection.CredentialRef != receipt.ManagedRef {
				return receipt, errf(409, "credential receipt does not match the current Connection reference")
			}
			if _, err := h.resolveManagedCredential(receipt.ManagedRef); err != nil {
				return receipt, errf(409, "managed credential is dangling: %s", err)
			}
			return receipt, nil
		case credentialReceiptPrepared:
			if connection.CredentialRef == receipt.PreviousRef {
				if _, err := h.resolveManagedCredential(receipt.ManagedRef); err != nil {
					return h.markCredentialReceiptManual(receipt, "prepared managed credential is unavailable", err)
				}
				if _, err := h.UpdateConnection(connection.ID, ConnectionParams{CredentialRef: receipt.ManagedRef}); err != nil {
					return receipt, err
				}
			} else if connection.CredentialRef != receipt.ManagedRef {
				return h.markCredentialReceiptManual(receipt, "prepared receipt conflicts with Connection reference", nil)
			}
			receipt.Status, receipt.UpdatedAt = credentialReceiptCompleted, now()
			if err := h.writeCredentialReceipt(receipt); err != nil {
				return receipt, errf(500, "complete credential receipt: %s", err)
			}
			return receipt, nil
		case credentialReceiptRollbackPending, credentialReceiptManualRecoveryRequired:
			return receipt, errf(409, "credential receipt requires rollback or manual recovery")
		case credentialReceiptRolledBack:
			// A rolled-back Connection may be migrated again using the same
			// deterministic receipt identity and a fresh immutable credential.
		}
	} else if !os.IsNotExist(receiptErr) {
		return CredentialReceipt{}, errf(409, "credential receipt is unreadable: %s", receiptErr)
	}
	if !strings.HasPrefix(strings.TrimSpace(connection.CredentialRef), "keychain:") {
		if strings.HasPrefix(strings.TrimSpace(connection.CredentialRef), "env:") {
			return CredentialReceipt{}, errf(409, "environment references are not auto-migrated")
		}
		return CredentialReceipt{}, errf(409, "connection is not Keychain-backed")
	}
	if dryRun {
		return CredentialReceipt{
			Version: 1, ID: receiptID, ConnectionID: connection.ID, Provider: connection.Provider,
			PreviousRef: connection.CredentialRef, Status: "would_migrate", DryRun: true,
		}, nil
	}
	if strings.TrimSpace(confirm) != connection.ID {
		return CredentialReceipt{}, errf(400, "confirm must exactly match connection id %s", connection.ID)
	}
	payload, err := loadKeychainCredentialForMigration(connection, h.connectionAddresses(connection.ID))
	if err != nil {
		return CredentialReceipt{}, errf(409, "read Keychain compatibility credential: %s", err)
	}
	managedRef, err := h.PutManagedCredential(managedCredentialProvider(connection.Provider), payload)
	if err != nil {
		return CredentialReceipt{}, err
	}
	ts := now()
	receipt = CredentialReceipt{
		Version: 1, ID: receiptID, ConnectionID: connection.ID, Provider: connection.Provider,
		PreviousRef: connection.CredentialRef, ManagedRef: managedRef,
		Status: credentialReceiptPrepared, CreatedAt: ts, UpdatedAt: ts,
	}
	if err := h.writeCredentialReceipt(receipt); err != nil {
		_ = h.DeleteManagedCredential(managedRef)
		return CredentialReceipt{}, errf(500, "persist credential receipt: %s", err)
	}
	if _, err := h.UpdateConnection(connection.ID, ConnectionParams{CredentialRef: managedRef}); err != nil {
		return receipt, err
	}
	receipt.Status, receipt.UpdatedAt = credentialReceiptCompleted, now()
	if err := h.writeCredentialReceipt(receipt); err != nil {
		return receipt, errf(500, "managed reference committed but receipt completion is pending: %s", err)
	}
	return receipt, nil
}

// RollbackCredential restores the compatibility reference without deleting its
// Keychain source. An incomplete rollback is durably marked
// manual_recovery_required.
func (h *Hub) RollbackCredential(receiptID string, dryRun bool, confirm string) (CredentialReceipt, error) {
	receiptID = strings.TrimSpace(receiptID)
	receipt, err := h.readCredentialReceipt(receiptID)
	if err != nil {
		if os.IsNotExist(err) {
			return CredentialReceipt{}, errf(404, "credential receipt not found: %s", receiptID)
		}
		return CredentialReceipt{}, errf(409, "credential receipt is unreadable: %s", err)
	}
	unlock := h.lockCredentialConnection(receipt.ConnectionID)
	defer unlock()
	receipt, err = h.readCredentialReceipt(receiptID)
	if err != nil {
		return CredentialReceipt{}, err
	}
	if receipt.Status == credentialReceiptRolledBack {
		return receipt, nil
	}
	if dryRun {
		receipt.DryRun = true
		return receipt, nil
	}
	if strings.TrimSpace(confirm) != receipt.ID {
		return receipt, errf(400, "confirm must exactly match receipt id %s", receipt.ID)
	}
	connection, err := h.credentialConnection(receipt.ConnectionID)
	if err != nil {
		return h.markCredentialReceiptManual(receipt, "Connection is unavailable during rollback", err)
	}
	if connection.CredentialRef != receipt.ManagedRef && connection.CredentialRef != receipt.PreviousRef {
		return h.markCredentialReceiptManual(receipt, "Connection reference conflicts with rollback receipt", nil)
	}
	receipt.Status, receipt.FailureReason, receipt.UpdatedAt = credentialReceiptRollbackPending, "", now()
	if err := h.writeCredentialReceipt(receipt); err != nil {
		return receipt, errf(500, "persist rollback intent: %s", err)
	}
	if connection.CredentialRef == receipt.ManagedRef {
		if _, err := h.UpdateConnection(connection.ID, ConnectionParams{CredentialRef: receipt.PreviousRef}); err != nil {
			return h.markCredentialReceiptManual(receipt, "restore previous credential reference", err)
		}
	}
	if err := h.DeleteManagedCredential(receipt.ManagedRef); err != nil {
		return h.markCredentialReceiptManual(receipt, "delete managed credential after restoring reference", err)
	}
	receipt.Status, receipt.FailureReason, receipt.UpdatedAt = credentialReceiptRolledBack, "", now()
	if err := h.writeCredentialReceipt(receipt); err != nil {
		return h.markCredentialReceiptManual(receipt, "persist completed rollback receipt", err)
	}
	return receipt, nil
}

func (h *Hub) credentialConnection(id string) (PlatformConnection, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	connection := h.connections[id]
	if connection == nil {
		return PlatformConnection{}, errf(404, "connection not found: %s", id)
	}
	if connection.ArchivedAt != "" || !connection.Enabled {
		return PlatformConnection{}, errf(409, "connection is disabled or archived")
	}
	return clonePlatformConnectionValue(*connection), nil
}

func (h *Hub) connectionAddresses(connectionID string) []AgentAddress {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]AgentAddress, 0)
	for _, address := range h.addresses {
		if address != nil && address.ConnectionID == connectionID && address.ArchivedAt == "" && address.DeletedAt == "" {
			out = append(out, cloneAgentAddressValue(*address))
		}
	}
	return out
}

func (h *Hub) lockCredentialConnection(connectionID string) func() {
	h.credentialLockMu.Lock()
	if h.credentialLocks == nil {
		h.credentialLocks = make(map[string]*sync.Mutex)
	}
	lock := h.credentialLocks[connectionID]
	if lock == nil {
		lock = &sync.Mutex{}
		h.credentialLocks[connectionID] = lock
	}
	h.credentialLockMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func credentialReceiptID(connectionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(connectionID)))
	return "creceipt_" + hex.EncodeToString(sum[:16])
}

func credentialReceiptPath(receiptID string) (string, error) {
	if !strings.HasPrefix(receiptID, "creceipt_") || len(receiptID) != len("creceipt_")+32 {
		return "", fmt.Errorf("invalid credential receipt id")
	}
	for _, char := range strings.TrimPrefix(receiptID, "creceipt_") {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return "", fmt.Errorf("invalid credential receipt id")
		}
	}
	return filepath.Join(credentialReceiptDirectory, receiptID+".json"), nil
}

func (h *Hub) writeCredentialReceipt(receipt CredentialReceipt) error {
	path, err := credentialReceiptPath(receipt.ID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return h.st.WithStableWriteRoot(func(root *os.Root) error {
		if err := root.MkdirAll(credentialReceiptDirectory, 0o700); err != nil {
			return err
		}
		temporary := filepath.Join(credentialReceiptDirectory, ".receipt-"+randomHubSuffix()+".tmp")
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
		if err := root.Rename(temporary, path); err != nil {
			return err
		}
		committed = true
		directory, err := root.Open(credentialReceiptDirectory)
		if err != nil {
			return err
		}
		defer directory.Close()
		return directory.Sync()
	})
}

func (h *Hub) readCredentialReceipt(receiptID string) (CredentialReceipt, error) {
	path, err := credentialReceiptPath(receiptID)
	if err != nil {
		return CredentialReceipt{}, err
	}
	data, err := h.st.ReadStableFile(path)
	if err != nil {
		return CredentialReceipt{}, err
	}
	var receipt CredentialReceipt
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return CredentialReceipt{}, fmt.Errorf("invalid credential receipt")
	}
	if receipt.Version != 1 || receipt.ID != receiptID || credentialReceiptID(receipt.ConnectionID) != receipt.ID ||
		receipt.PreviousRef == "" || !strings.HasPrefix(receipt.PreviousRef, "keychain:") ||
		!credentials.IsManagedRef(receipt.ManagedRef) {
		return CredentialReceipt{}, fmt.Errorf("invalid credential receipt")
	}
	switch receipt.Status {
	case credentialReceiptPrepared, credentialReceiptCompleted, credentialReceiptRollbackPending,
		credentialReceiptRolledBack, credentialReceiptManualRecoveryRequired:
	default:
		return CredentialReceipt{}, fmt.Errorf("invalid credential receipt status")
	}
	return receipt, nil
}

func (h *Hub) markCredentialReceiptManual(receipt CredentialReceipt, message string, cause error) (CredentialReceipt, error) {
	receipt.Status = credentialReceiptManualRecoveryRequired
	receipt.FailureReason = strings.TrimSpace(message)
	receipt.UpdatedAt = now()
	writeErr := h.writeCredentialReceipt(receipt)
	if cause == nil {
		cause = errors.New(message)
	}
	if writeErr != nil {
		cause = errors.Join(cause, writeErr)
	}
	return receipt, errf(500, "credential rollback requires manual recovery: %s", cause)
}

func loadProviderKeychainCredential(connection PlatformConnection, addresses []AgentAddress) (map[string]string, error) {
	switch strings.ToLower(strings.TrimSpace(connection.Provider)) {
	case "lark", "feishu":
		secret, err := feishu.LoadAppSecret(connection.AccountRef)
		if err != nil {
			return nil, err
		}
		if secret == "" {
			return nil, fmt.Errorf("Feishu App Secret is missing")
		}
		return map[string]string{"appSecret": secret}, nil
	case "slack":
		appID := slackAppIDFromKeychainRef(connection.CredentialRef)
		tokens, err := loomslack.LoadTokens(appID, connection.AccountRef)
		if err != nil {
			return nil, err
		}
		if tokens.Bot == "" || tokens.App == "" {
			return nil, fmt.Errorf("Slack tokens are incomplete")
		}
		return map[string]string{"botToken": tokens.Bot, "appToken": tokens.App}, nil
	case "parall":
		agentID := strings.TrimSpace(connection.ScopeRef)
		if agentID == "" {
			for _, address := range addresses {
				if value := strings.TrimPrefix(strings.TrimSpace(address.ExternalIdentity), "prll://"); value != "" {
					agentID = value
					break
				}
			}
		}
		value, err := parall.LoadAgentCredentials(connection.AccountRef, agentID)
		if err != nil {
			return nil, err
		}
		if value.APIURL == "" || value.APIKey == "" {
			return nil, fmt.Errorf("Parall Agent credentials are incomplete")
		}
		return map[string]string{"apiUrl": value.APIURL, "apiKey": value.APIKey}, nil
	case "github":
		token, err := githubcredential.LoadCredential(connection.CredentialRef)
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, fmt.Errorf("GitHub token is missing")
		}
		return map[string]string{"token": token}, nil
	default:
		return nil, fmt.Errorf("provider %s does not support automatic Keychain migration", connection.Provider)
	}
}

func slackAppIDFromKeychainRef(ref string) string {
	value := strings.TrimPrefix(strings.TrimSpace(ref), "keychain:")
	value = strings.TrimPrefix(value, "com.codexloom.slack.")
	value = strings.TrimSuffix(value, ".bot-token")
	value = strings.TrimSuffix(value, ".app-token")
	return strings.TrimSpace(value)
}

func managedCredentialProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "feishu" {
		return "lark"
	}
	return provider
}
