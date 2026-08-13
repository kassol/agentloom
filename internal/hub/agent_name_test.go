package hub

import (
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestCreateAgentAcceptsUnicodeNameAndPersistsTrimmedIdentity(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{
		createBinding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "fake", NativeRef: "native-unicode"},
	}
	h.runtimeHostDrivers["fake"] = &controlPlaneDriver{acquireHost: &controlPlaneHost{contract: contract, alive: true}}

	created, err := h.CreateAgent(CreateParams{Name: " 研发助手-二号 ", Cwd: " " + t.TempDir() + " ", RuntimeKind: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "研发助手-二号" {
		t.Fatalf("Agent name = %q", created.Name)
	}
	if resolved := h.resolveLocked("研发助手-二号"); resolved == nil || resolved.ID != created.ID {
		t.Fatalf("Unicode Agent name did not resolve: %#v", resolved)
	}
}

func TestAgentNameValidationRejectsUnsupportedPunctuation(t *testing.T) {
	h := testHub(nil)
	h.runtimeHostDrivers["fake"] = &controlPlaneDriver{}
	_, err := h.CreateAgent(CreateParams{Name: "研发 助手", Cwd: t.TempDir(), RuntimeKind: "fake"})
	if err == nil || !strings.Contains(err.Error(), "Unicode letters") {
		t.Fatalf("CreateAgent error = %v", err)
	}
}

func TestRenameAgentAcceptsUnicodeName(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "before", RuntimeBinding: RuntimeBinding{Kind: "pi"}, Status: "idle"}
	name := "产品负责人"
	updated, err := h.UpdateAgentConfig("agent-1", ConfigParams{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != name {
		t.Fatalf("Agent name = %q", updated.Name)
	}
}
