package hub

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/credentials"
)

func newLarkFixture(t *testing.T) r0bFixture {
	t.Helper()
	fixture := newR0bFixture(t)
	connection, err := fixture.h.CreateConnection(ConnectionParams{
		Provider: "lark", AccountRef: "app", ScopeRef: "org",
		CredentialRef: "keychain:com.codexloom.lark", Capabilities: []string{"receive_events"},
	})
	if err != nil {
		t.Fatal(err)
	}
	address, err := fixture.h.CreateAddress(AddressParams{
		Agent: "r0b-agent", ConnectionID: connection.ID, ExternalIdentity: "external-lark",
		TriggerPolicy: "all", ReplyPolicy: "final_answer", TrustDomain: "workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.connection = connection
	fixture.address = address
	return fixture
}

func TestLarkMigrateVerifyRollbackPreservesIdentity(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	secret := []byte("lark-app-secret-勿-泄露")
	result, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, secret, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FloorRaised || !strings.HasPrefix(result.CurrentRef, "managed:") || result.PreviousRef != "keychain:com.codexloom.lark" {
		t.Fatalf("migration result = %#v", result)
	}
	connections := fixture.h.ListConnections()
	var migrated PlatformConnection
	for _, candidate := range connections {
		if candidate.ID == fixture.connection.ID {
			migrated = candidate
		}
	}
	if migrated.ID != fixture.connection.ID || migrated.CredentialRef != result.CurrentRef {
		t.Fatalf("connection identity/reference changed incorrectly: %#v", migrated)
	}
	addresses, err := fixture.h.ListAddresses("r0b-agent")
	if err != nil || len(addresses) != 2 {
		t.Fatalf("addresses changed: %#v err=%v", addresses, err)
	}
	found := false
	for _, address := range addresses {
		if address.ID == fixture.address.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("original Lark address identity lost")
	}
	integrations, err := os.ReadFile(filepath.Join(fixture.dir, "integrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(integrations, secret) {
		t.Fatal("secret leaked into integrations durable state")
	}
	foundation, err := fixture.st.ReadStableFile("runtime-foundation.json")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(foundation, secret) {
		t.Fatal("secret leaked into the runtime foundation")
	}
	verified, err := fixture.h.VerifyLarkCredential(fixture.connection.ID)
	if err != nil || !verified.AlreadyMigrated {
		t.Fatalf("verify failed: %#v err=%v", verified, err)
	}
	rollback, err := fixture.h.RollbackLarkCredential(fixture.connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.CurrentRef != "keychain:com.codexloom.lark" {
		t.Fatalf("rollback result = %#v", rollback)
	}
	if !fixture.st.CredentialFloorPresent() {
		t.Fatal("rollback lowered the credential floor")
	}
	after := fixture.h.ListConnections()
	for _, candidate := range after {
		if candidate.ID == fixture.connection.ID && candidate.CredentialRef != "keychain:com.codexloom.lark" {
			t.Fatalf("connection reference not restored: %#v", candidate)
		}
	}
}

func TestLarkMigrateDryRunZeroWrites(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	result, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun {
		t.Fatalf("dry run not marked: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, credentials.DirectoryName)); !os.IsNotExist(err) {
		t.Fatalf("dry run created credential store: %v", err)
	}
	if fixture.st.CredentialFloorPresent() {
		t.Fatal("dry run raised the credential floor")
	}
	if connectionRefByID(t, fixture.h, fixture.connection.ID) != "keychain:com.codexloom.lark" {
		t.Fatal("dry run changed the connection reference")
	}
}

func TestLarkMigrateIdempotent(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	first, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("different"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyMigrated || second.CurrentRef != first.CurrentRef {
		t.Fatalf("second migrate not idempotent: %#v vs %#v", second, first)
	}
}

func TestLarkMigrateRejectsNonLarkAndArchived(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	other, err := fixture.h.CreateConnection(ConnectionParams{Provider: "slack", CredentialRef: "keychain:slack"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), other.ID, []byte("x"), false); err == nil {
		t.Fatal("non-Lark connection migrated")
	}
	fixture.h.mu.Lock()
	fixture.h.connections[fixture.connection.ID].ArchivedAt = now()
	fixture.h.mu.Unlock()
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("x"), false); err == nil {
		t.Fatal("archived connection migrated")
	}
}

func TestLarkMigrateBlockedByManualLatch(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	initializeAdoptedR0bControl(t, &fixture, gatewayRecoveryManual, "manual required")
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false); err == nil {
		t.Fatal("migration bypassed a manual recovery latch")
	}
	var view PlatformConnection
	for _, candidate := range fixture.h.ListConnections() {
		if candidate.ID == fixture.connection.ID {
			view = candidate
		}
	}
	if view.Status != "degraded" || view.LastError != "manual required" {
		t.Fatalf("manual latch cleared or projection turned green: %#v", view)
	}
}

func TestLarkMigrateUpdateFailureDeletesCredential(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	fixture.h.larkUpdateConnectionForTest = func(string, string) (PlatformConnection, error) {
		return PlatformConnection{}, errf(500, "injected persist failure")
	}
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false); err == nil {
		t.Fatal("migration succeeded despite injected update failure")
	}
	fixture.h.larkUpdateConnectionForTest = nil
	if connectionRefByID(t, fixture.h, fixture.connection.ID) != "keychain:com.codexloom.lark" {
		t.Fatal("connection reference changed despite update failure")
	}
	credentialStore, err := credentials.New(fixture.st)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.st.Dir(), credentials.DirectoryName))
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				t.Fatalf("managed credential file survived update failure: %s", entry.Name())
			}
		}
	}
	_ = credentialStore
}

func TestLarkVerifyDanglingRefFails(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	result, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false)
	if err != nil {
		t.Fatal(err)
	}
	credentialStore, err := credentials.New(fixture.st)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.Delete(credentials.Ref(result.CurrentRef)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.h.VerifyLarkCredential(fixture.connection.ID); err == nil {
		t.Fatal("verify succeeded with a dangling managed reference")
	}
}

func TestLarkMigrateSecretNeverInErrors(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	secret := []byte("super-secret-value")
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, secret, false); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.h.VerifyLarkCredential(fixture.connection.ID); err != nil && strings.Contains(err.Error(), string(secret)) {
		t.Fatal("secret leaked into an error")
	}
}

func connectionRefByID(t *testing.T, h *Hub, connectionID string) string {
	t.Helper()
	for _, candidate := range h.ListConnections() {
		if candidate.ID == connectionID {
			return candidate.CredentialRef
		}
	}
	t.Fatalf("connection %s not found", connectionID)
	return ""
}
