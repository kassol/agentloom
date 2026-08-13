package hub

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestCredentialPreflightEnumeratesAllEnabledKeychainProviders(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		h.Shutdown()
		_ = st.Close()
	}()
	for _, provider := range []string{"lark", "feishu", "slack", "parall", "github"} {
		if _, err := h.CreateConnection(ConnectionParams{Provider: provider, CredentialRef: "keychain:test." + provider}); err != nil {
			t.Fatal(err)
		}
	}
	envConnection, err := h.CreateConnection(ConnectionParams{Provider: "github", CredentialRef: "env:GITHUB_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := h.CreateConnection(ConnectionParams{Provider: "slack", CredentialRef: "keychain:test.disabled", Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}

	results, err := h.CredentialPreflightList("")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 {
		t.Fatalf("preflight = %#v", results)
	}
	seen := map[string]bool{}
	for _, result := range results {
		if !result.Eligible {
			t.Fatalf("eligible Keychain result blocked: %#v", result)
		}
		seen[result.Provider] = true
	}
	for _, provider := range []string{"lark", "feishu", "slack", "parall", "github"} {
		if !seen[provider] {
			t.Fatalf("provider %s omitted from preflight", provider)
		}
	}
	specific, err := h.CredentialPreflightList(envConnection.ID)
	if err != nil || len(specific) != 1 || specific[0].Eligible || !strings.Contains(specific[0].Reason, "not auto-migrated") {
		t.Fatalf("env preflight = %#v err=%v", specific, err)
	}
}

func TestCredentialMigrationIsIdempotentAcrossRestartAndRollbackKeepsKeychain(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := h.CreateConnection(ConnectionParams{Provider: "github", CredentialRef: "keychain:test.github"})
	if err != nil {
		t.Fatal(err)
	}
	secret := "github-secret-never-project"
	oldLoader := loadKeychainCredentialForMigration
	loadKeychainCredentialForMigration = func(PlatformConnection, []AgentAddress) (map[string]string, error) {
		return map[string]string{"token": secret}, nil
	}
	t.Cleanup(func() { loadKeychainCredentialForMigration = oldLoader })

	receipt, err := h.MigrateCredential(context.Background(), connection.ID, false, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != credentialReceiptCompleted || !strings.HasPrefix(receipt.ManagedRef, "managed:") {
		t.Fatalf("receipt = %#v", receipt)
	}
	again, err := h.MigrateCredential(context.Background(), connection.ID, false, connection.ID)
	if err != nil || again.ID != receipt.ID || again.ManagedRef != receipt.ManagedRef {
		t.Fatalf("idempotent result = %#v err=%v", again, err)
	}
	integrations, err := os.ReadFile(filepath.Join(dir, "integrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	receiptBytes, err := os.ReadFile(filepath.Join(dir, credentialReceiptDirectory, receipt.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(integrations, []byte(secret)) || bytes.Contains(receiptBytes, []byte(secret)) {
		t.Fatal("secret leaked into durable public state")
	}

	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err = Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		h.Shutdown()
		_ = st.Close()
	}()
	afterRestart, err := h.MigrateCredential(context.Background(), connection.ID, false, connection.ID)
	if err != nil || afterRestart.ID != receipt.ID {
		t.Fatalf("restart idempotency = %#v err=%v", afterRestart, err)
	}
	rolledBack, err := h.RollbackCredential(receipt.ID, false, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Status != credentialReceiptRolledBack {
		t.Fatalf("rollback = %#v", rolledBack)
	}
	connections := h.ListConnections()
	if len(connections) != 1 || connections[0].CredentialRef != "keychain:test.github" {
		t.Fatalf("Keychain compatibility reference was not restored: %#v", connections)
	}
}

func TestManagedConnectionReferenceMustResolveAndRollbackMarksIncompleteFailure(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		h.Shutdown()
		_ = st.Close()
	}()
	dangling := "managed:" + strings.Repeat("a", 64)
	if _, err := h.CreateConnection(ConnectionParams{Provider: "github", CredentialRef: dangling}); err == nil {
		t.Fatal("dangling managed reference was accepted")
	}
	connection, err := h.CreateConnection(ConnectionParams{Provider: "github", CredentialRef: "keychain:test.github"})
	if err != nil {
		t.Fatal(err)
	}
	oldLoader := loadKeychainCredentialForMigration
	loadKeychainCredentialForMigration = func(PlatformConnection, []AgentAddress) (map[string]string, error) {
		return map[string]string{"token": "secret"}, nil
	}
	defer func() { loadKeychainCredentialForMigration = oldLoader }()
	receipt, err := h.MigrateCredential(context.Background(), connection.ID, false, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.deleteManagedCredentialForTest = func(string) error { return errors.New("injected delete failure") }
	result, err := h.RollbackCredential(receipt.ID, false, receipt.ID)
	if err == nil || result.Status != credentialReceiptManualRecoveryRequired {
		t.Fatalf("incomplete rollback = %#v err=%v", result, err)
	}
	stored, readErr := h.readCredentialReceipt(receipt.ID)
	if readErr != nil || stored.Status != credentialReceiptManualRecoveryRequired {
		t.Fatalf("durable rollback status = %#v err=%v", stored, readErr)
	}
}

func TestCredentialMigrationFailureIsIsolatedPerConnection(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		h.Shutdown()
		_ = st.Close()
	}()
	failing, err := h.CreateConnection(ConnectionParams{Provider: "slack", CredentialRef: "keychain:test.fail"})
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := h.CreateConnection(ConnectionParams{Provider: "github", CredentialRef: "keychain:test.ok"})
	if err != nil {
		t.Fatal(err)
	}
	oldLoader := loadKeychainCredentialForMigration
	loadKeychainCredentialForMigration = func(connection PlatformConnection, _ []AgentAddress) (map[string]string, error) {
		if connection.ID == failing.ID {
			return nil, errors.New("injected Keychain read failure")
		}
		return map[string]string{"token": "healthy-secret"}, nil
	}
	defer func() { loadKeychainCredentialForMigration = oldLoader }()
	if _, err := h.MigrateCredential(context.Background(), failing.ID, false, failing.ID); err == nil {
		t.Fatal("failing Connection unexpectedly migrated")
	}
	receipt, err := h.MigrateCredential(context.Background(), healthy.ID, false, healthy.ID)
	if err != nil || receipt.Status != credentialReceiptCompleted {
		t.Fatalf("healthy Connection migration = %#v err=%v", receipt, err)
	}
	connections := h.ListConnections()
	refs := map[string]string{}
	for _, connection := range connections {
		refs[connection.ID] = connection.CredentialRef
	}
	if refs[failing.ID] != "keychain:test.fail" || !strings.HasPrefix(refs[healthy.ID], "managed:") {
		t.Fatalf("failure crossed Connection boundary: %#v", refs)
	}
}
