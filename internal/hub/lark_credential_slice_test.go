package hub

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Without an accepted exact R1 proof, Verify must fail closed.
	if _, err := fixture.h.VerifyLarkCredential(fixture.connection.ID); err == nil {
		t.Fatal("verify succeeded without an accepted Gateway exact proof")
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

func TestLarkMigrateVerifyAfterExactProof(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeGatewayServiceAdapter{applyResult: gatewayServiceEffectResult{Outcome: gatewayServiceEffectApplied}}
	fixture.h.gatewayServiceAdapterForTest = func(gatewayLaunchPlan) (gatewayServiceAdapter, error) { return adapter, nil }
	executable := filepath.Join(fixture.dir, "loom-feishu-gateway")
	if err := os.WriteFile(executable, []byte("accepted gateway binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.ConfigureLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.FinishLarkGatewayLaunchPlan(fixture.connection.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.RestartGatewayProcesses(); err != nil {
		t.Fatal(err)
	}
	fixture.h.mu.Lock()
	attempt := fixture.h.gatewayState.Attempts[fixture.connection.ID]
	fixture.h.mu.Unlock()
	if attempt == nil || attempt.Phase != gatewayAttemptAwaitingTargetProof {
		t.Fatalf("startup did not consume the typed plan: %#v", attempt)
	}
	proof := &GatewayProcessHeartbeatParams{
		AttemptID: attempt.ID, Generation: attempt.TargetGeneration, Build: attempt.Plan.Target.Build,
		ExecutableDigest: attempt.Plan.Target.ExecutableDigest,
	}
	if _, err := fixture.h.HeartbeatConnection(fixture.connection.ID, ConnectionHeartbeatParams{Status: "connected", GatewayProcess: proof}); err != nil {
		t.Fatal(err)
	}
	verified, err := fixture.h.VerifyLarkCredential(fixture.connection.ID)
	if err != nil || !verified.AlreadyMigrated || verified.RequiresProof {
		t.Fatalf("verify after exact target proof failed: %#v err=%v", verified, err)
	}
}

func TestLarkVerifyRejectsRecoveryProofAfterTargetFailure(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeGatewayServiceAdapter{
		applyResult:   gatewayServiceEffectResult{Outcome: gatewayServiceEffectFailed, Err: context.DeadlineExceeded},
		restoreResult: gatewayServiceEffectResult{Outcome: gatewayServiceEffectApplied},
	}
	fixture.h.gatewayServiceAdapterForTest = func(gatewayLaunchPlan) (gatewayServiceAdapter, error) { return adapter, nil }
	executable := filepath.Join(fixture.dir, "loom-feishu-gateway")
	if err := os.WriteFile(executable, []byte("accepted gateway binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.ConfigureLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.FinishLarkGatewayLaunchPlan(fixture.connection.ID); err != nil {
		t.Fatal(err)
	}
	type launchResult struct {
		attempt gatewayTransitionAttempt
		err     error
	}
	result := make(chan launchResult, 1)
	spec := l2aLaunchSpec(t, &fixture, gatewayServiceManagerFake)
	go func() {
		attempt, err := fixture.h.startLarkGatewayLaunch(context.Background(), spec, gatewayAttemptMigration)
		result <- launchResult{attempt: attempt, err: err}
	}()
	var attempt gatewayTransitionAttempt
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fixture.h.mu.Lock()
		if current := fixture.h.gatewayState.Attempts[fixture.connection.ID]; current != nil {
			attempt = *current
		}
		fixture.h.mu.Unlock()
		if attempt.Phase == gatewayAttemptAwaitingRecoveryProof {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if attempt.Phase != gatewayAttemptAwaitingRecoveryProof {
		t.Fatalf("target failure did not reach recovery proof: %#v", attempt)
	}
	recoveryProof := &GatewayProcessHeartbeatParams{
		AttemptID: attempt.ID, Generation: attempt.RecoveryGeneration, Build: attempt.Plan.Anchor.Descriptor.Build,
		ExecutableDigest: attempt.Plan.Anchor.Descriptor.ExecutableDigest,
	}
	if _, err := fixture.h.HeartbeatConnection(fixture.connection.ID, ConnectionHeartbeatParams{Status: "connected", GatewayProcess: recoveryProof}); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-result:
		if completed.err == nil || completed.attempt.Phase != gatewayAttemptRecovered {
			t.Fatalf("recovery result = %#v err=%v", completed.attempt, completed.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovery attempt did not terminate")
	}
	if _, err := fixture.h.VerifyLarkCredential(fixture.connection.ID); err == nil {
		t.Fatal("verify accepted a legacy recovery proof as managed target success")
	}
}

func TestLarkMigratePlanPendingDurableAndReentrant(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	first, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !first.PlanPending || first.AlreadyMigrated {
		t.Fatalf("fresh migration must end durable in plan_pending: %#v", first)
	}
	record, err := fixture.h.readLarkMigrationRecord(fixture.connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != larkMigrationPhasePlanPending {
		t.Fatalf("record phase = %q", record.Phase)
	}
	// Re-entry before the plan is configured stays durable and does not error.
	second, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("different"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !second.PlanPending || second.CurrentRef != first.CurrentRef {
		t.Fatalf("re-entered migrate did not stay plan_pending: %#v", second)
	}
	fixture.h.gatewayServiceAdapterForTest = func(gatewayLaunchPlan) (gatewayServiceAdapter, error) {
		return &fakeGatewayServiceAdapter{applyResult: gatewayServiceEffectResult{Outcome: gatewayServiceEffectApplied}}, nil
	}
	executable := filepath.Join(fixture.dir, "loom-feishu-gateway")
	if err := os.WriteFile(executable, []byte("accepted gateway binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.ConfigureLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.FinishLarkGatewayLaunchPlan(fixture.connection.ID); err != nil {
		t.Fatal(err)
	}
	record, err = fixture.h.readLarkMigrationRecord(fixture.connection.ID)
	if err != nil || record.Phase != larkMigrationPhaseCompleted {
		t.Fatalf("record did not complete: %#v err=%v", record, err)
	}
}

func timeNow() time.Time { return time.Now().UTC() }

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
	fixture.h.gatewayServiceAdapterForTest = func(gatewayLaunchPlan) (gatewayServiceAdapter, error) {
		return &fakeGatewayServiceAdapter{applyResult: gatewayServiceEffectResult{Outcome: gatewayServiceEffectApplied}}, nil
	}
	executable := filepath.Join(fixture.dir, "loom-feishu-gateway")
	if err := os.WriteFile(executable, []byte("accepted gateway binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.ConfigureLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.FinishLarkGatewayLaunchPlan(fixture.connection.ID); err != nil {
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

func TestLarkRollbackReentrantAfterRefRestoredCrash(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false); err != nil {
		t.Fatal(err)
	}
	fixture.h.gatewayServiceAdapterForTest = func(gatewayLaunchPlan) (gatewayServiceAdapter, error) {
		return &fakeGatewayServiceAdapter{applyResult: gatewayServiceEffectResult{Outcome: gatewayServiceEffectApplied}}, nil
	}
	executable := filepath.Join(fixture.dir, "loom-feishu-gateway")
	if err := os.WriteFile(executable, []byte("accepted gateway binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.ConfigureLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.FinishLarkGatewayLaunchPlan(fixture.connection.ID); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the reference restore but before plan revoke,
	// credential deletion, and record removal.
	if _, err := fixture.h.UpdateConnection(fixture.connection.ID, ConnectionParams{CredentialRef: "keychain:com.codexloom.lark"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.advanceLarkMigrationPhase(fixture.connection.ID, larkMigrationPhaseRefRestored); err != nil {
		t.Fatal(err)
	}
	if ref := fixture.h.LarkGatewayLaunchPlanRef(fixture.connection.ID); ref == "" {
		t.Fatal("crash simulation did not leave a typed plan in place")
	}
	rollback, err := fixture.h.RollbackLarkCredential(fixture.connection.ID)
	if err != nil {
		t.Fatalf("re-entered rollback could not finish: %v", err)
	}
	if rollback.CurrentRef != "keychain:com.codexloom.lark" {
		t.Fatalf("rollback result = %#v", rollback)
	}
	if ref := fixture.h.LarkGatewayLaunchPlanRef(fixture.connection.ID); ref != "" {
		t.Fatalf("typed launch plan survived rollback: %q", ref)
	}
	if _, err := fixture.h.readLarkMigrationRecord(fixture.connection.ID); !os.IsNotExist(err) {
		t.Fatalf("migration record survived rollback: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.st.Dir(), credentials.DirectoryName))
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				t.Fatalf("managed credential file survived rollback: %s", entry.Name())
			}
		}
	}
}

func TestLarkMigrateIdempotentAfterCutover(t *testing.T) {
	fixture := newLarkFixture(t)
	defer fixture.close(t)
	if _, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("secret"), false); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeGatewayServiceAdapter{applyResult: gatewayServiceEffectResult{Outcome: gatewayServiceEffectApplied}}
	fixture.h.gatewayServiceAdapterForTest = func(gatewayLaunchPlan) (gatewayServiceAdapter, error) { return adapter, nil }
	executable := filepath.Join(fixture.dir, "loom-feishu-gateway")
	if err := os.WriteFile(executable, []byte("accepted gateway binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.ConfigureLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.FinishLarkGatewayLaunchPlan(fixture.connection.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.h.RestartGatewayProcesses(); err != nil {
		t.Fatal(err)
	}
	fixture.h.mu.Lock()
	attempt := fixture.h.gatewayState.Attempts[fixture.connection.ID]
	fixture.h.mu.Unlock()
	proof := &GatewayProcessHeartbeatParams{
		AttemptID: attempt.ID, Generation: attempt.TargetGeneration, Build: attempt.Plan.Target.Build,
		ExecutableDigest: attempt.Plan.Target.ExecutableDigest,
	}
	if _, err := fixture.h.HeartbeatConnection(fixture.connection.ID, ConnectionHeartbeatParams{Status: "connected", GatewayProcess: proof}); err != nil {
		t.Fatal(err)
	}
	fixture.h.Shutdown()
	fixture.h = nil
	reopened, err := Open(fixture.st)
	if err != nil {
		t.Fatal(err)
	}
	fixture.h = reopened
	reopened.gatewayServiceAdapterForTest = func(gatewayLaunchPlan) (gatewayServiceAdapter, error) { return adapter, nil }
	if err := fixture.h.PreflightLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatalf("preflight after successful cutover failed: %v", err)
	}
	if err := fixture.h.ConfigureLarkGatewayLaunch(fixture.connection.ID, executable); err != nil {
		t.Fatalf("configure after successful cutover was not a no-op: %v", err)
	}
	again, err := fixture.h.MigrateLarkCredential(context.Background(), fixture.connection.ID, []byte("different"), false)
	if err != nil || !again.AlreadyMigrated {
		t.Fatalf("migrate after successful cutover was not idempotent: %#v err=%v", again, err)
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
