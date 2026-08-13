package hub

import (
	"encoding/json"
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
