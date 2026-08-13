package hub

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestClaudeRuntimeConfigurationRejectsEmptySettingsAndUnsupportedHelper(t *testing.T) {
	for _, test := range []struct {
		name          string
		configuration RuntimeConfiguration
		want          string
	}{
		{
			name: "empty settings",
			configuration: RuntimeConfiguration{Configured: true,
				Authentication: RuntimeAuthentication{Category: RuntimeAuthConsole, Source: "api_key"}},
			want: "at least one source",
		},
		{
			name: "credential helper",
			configuration: RuntimeConfiguration{Configured: true, SettingSources: []string{"project"},
				Authentication: RuntimeAuthentication{Category: RuntimeAuthConsole, Source: "helper"}},
			want: "must be api_key",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeRuntimeConfiguration("claude", test.configuration)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("normalize error = %v, want %q", err, test.want)
			}
		})
	}
	normalized, err := normalizeRuntimeConfiguration("claude", RuntimeConfiguration{
		Configured: true, SettingSources: []string{" local ", "user"},
		Authentication: RuntimeAuthentication{Category: " console ", Source: " api_key "},
	})
	if err != nil || !slices.Equal(normalized.SettingSources, []string{"user", "local"}) || normalized.Authentication != (RuntimeAuthentication{Category: RuntimeAuthConsole, Source: "api_key"}) {
		t.Fatalf("normalized Claude configuration = %#v, err=%v", normalized, err)
	}
	console := claudeRuntimeConfigurationDescriptor().Authentication[0]
	if console.Category != RuntimeAuthSubscription || console.Sources[0].ID != "claude_ai" || !strings.Contains(console.Description, "existing local Claude.ai login") {
		t.Fatalf("Subscription authentication descriptor = %#v", console)
	}
	console = claudeRuntimeConfigurationDescriptor().Authentication[1]
	if console.Category != RuntimeAuthConsole || !strings.Contains(console.Description, "credential helpers") || !strings.Contains(console.Description, "unavailable") {
		t.Fatalf("Console authentication descriptor = %#v", console)
	}
	subscription, err := normalizeRuntimeConfiguration("claude", RuntimeConfiguration{
		Configured: true, SettingSources: []string{"user"},
		Authentication: RuntimeAuthentication{Category: RuntimeAuthSubscription, Source: "claude_ai"},
	})
	if err != nil || subscription.Authentication != (RuntimeAuthentication{Category: RuntimeAuthSubscription, Source: "claude_ai"}) {
		t.Fatalf("normalized Claude subscription configuration = %#v, err=%v", subscription, err)
	}
}

func TestOpenMigratesLegacyClaudeAgentWithoutRuntimeConfiguration(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	legacy := `{"legacy-claude":{"id":"legacy-claude","name":"legacy-claude","cwd":"/tmp/legacy-claude","threadId":"loom-thread","runtimeBinding":{"schemaVersion":2,"kind":"claude","nativeRef":"11111111-1111-4111-8111-111111111111"},"status":"idle","createdAt":"2026-08-01T00:00:00Z","updatedAt":"2026-08-01T00:00:00Z"}}`
	if err := st.SaveAgents(json.RawMessage(legacy)); err != nil {
		t.Fatal(err)
	}
	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Shutdown)
	agent, err := h.GetAgent("legacy-claude")
	if err != nil {
		t.Fatal(err)
	}
	want := legacyClaudeRuntimeConfiguration()
	if !agent.RuntimeConfiguration.Configured || strings.Join(agent.RuntimeConfiguration.SettingSources, ",") != strings.Join(want.SettingSources, ",") || agent.RuntimeConfiguration.Authentication != want.Authentication {
		t.Fatalf("migrated Runtime configuration = %#v, want %#v", agent.RuntimeConfiguration, want)
	}
	var persisted map[string]*Agent
	if err := st.LoadAgents(&persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted["legacy-claude"].RuntimeConfiguration; !got.Configured || got.Authentication != want.Authentication {
		t.Fatalf("persisted Runtime configuration = %#v, want %#v", got, want)
	}
}

func TestOpenRejectsExplicitlyMalformedClaudeRuntimeConfiguration(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	malformed := `{"malformed-claude":{"id":"malformed-claude","name":"malformed-claude","cwd":"/tmp/malformed-claude","threadId":"loom-thread","runtimeBinding":{"schemaVersion":2,"kind":"claude","nativeRef":"11111111-1111-4111-8111-111111111111"},"runtimeConfiguration":{},"status":"idle","createdAt":"2026-08-01T00:00:00Z","updatedAt":"2026-08-01T00:00:00Z"}}`
	if err := st.SaveAgents(json.RawMessage(malformed)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(st); err == nil || !strings.Contains(err.Error(), "invalid Runtime configuration") {
		t.Fatalf("Open malformed Claude owner configuration error = %v", err)
	}
}

func TestClaudeRuntimeConfigurationTransactionPersistsAndRejectsStaleRevision(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.runtimeHostDrivers["claude"] = fakeClaudeBridgeDriver(t, st)
	agent, err := h.CreateAgent(CreateParams{Name: "configurable-claude", Cwd: t.TempDir(), RuntimeKind: "claude", RuntimeConfiguration: testClaudeRuntimeConfiguration()})
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.GetRuntimeConfiguration(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := RuntimeConfiguration{
		Configured: true, SettingSources: []string{"user", "local"},
		Authentication: RuntimeAuthentication{Category: RuntimeAuthGateway, Source: "gateway"},
	}
	updated, err := h.UpdateRuntimeConfiguration(agent.ID, RuntimeConfigurationParams{Configuration: next, ExpectedRevision: before.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(updated.Configuration.SettingSources, []string{"user", "local"}) || updated.Configuration.Authentication != next.Authentication || updated.Evidence.Authentication.Validation != "accepted" {
		t.Fatalf("updated configuration = %#v", updated)
	}
	if _, err := h.UpdateRuntimeConfiguration(agent.ID, RuntimeConfigurationParams{Configuration: testClaudeRuntimeConfiguration(), ExpectedRevision: before.Revision}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale configuration update error = %v", err)
	}
	h.Shutdown()
	if _, err := h.UpdateRuntimeConfiguration(agent.ID, RuntimeConfigurationParams{Configuration: next, ExpectedRevision: updated.Revision}); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("configuration update after shutdown error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedStore.Close()
	reopened, err := Open(reopenedStore)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Shutdown()
	view, err := reopened.GetAgent(agent.ID)
	if err != nil || !slices.Equal(view.RuntimeConfiguration.SettingSources, []string{"user", "local"}) || view.RuntimeConfiguration.Authentication != next.Authentication {
		t.Fatalf("reopened configuration = %#v, err=%v", view.RuntimeConfiguration, err)
	}
}

func TestClaudeRuntimeConfigurationStoreFailureRestoresPreviousSelection(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.runtimeHostDrivers["claude"] = fakeClaudeBridgeDriver(t, st)
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "config-store-failure", Cwd: t.TempDir(), RuntimeKind: "claude", RuntimeConfiguration: testClaudeRuntimeConfiguration()})
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.GetRuntimeConfiguration(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.saveAgentsForTest = func(any) error { return errors.New("disk full") }
	_, err = h.UpdateRuntimeConfiguration(agent.ID, RuntimeConfigurationParams{
		ExpectedRevision: before.Revision,
		Configuration:    RuntimeConfiguration{Configured: true, SettingSources: []string{"user", "local"}, Authentication: RuntimeAuthentication{Category: RuntimeAuthGateway, Source: "gateway"}},
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Store failure = %v", err)
	}
	h.saveAgentsForTest = nil
	view, err := h.GetAgent(agent.ID)
	if err != nil || !slices.Equal(view.RuntimeConfiguration.SettingSources, testClaudeRuntimeConfiguration().SettingSources) || view.RuntimeConfiguration.Authentication != testClaudeRuntimeConfiguration().Authentication {
		t.Fatalf("configuration after failed Store = %#v, err=%v", view.RuntimeConfiguration, err)
	}
}
