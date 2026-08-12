package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestClaudeCanonicalTurnLedgerRoundTripsRuntimeNeutralHistory(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	turn := runtimecontract.HistoryTurn{
		TurnID: "turn-loom", State: runtimecontract.LifecycleCompleted,
		Content: []runtimecontract.ContentBlock{{ID: "item-loom", Kind: runtimecontract.ContentAssistantText, Text: "done"}},
	}
	if err := st.SaveCanonicalTurnLedger("agent-loom", []runtimecontract.HistoryTurn{turn}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	history, err := reopened.LoadCanonicalTurnLedger("agent-loom", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || len(history.Turns) != 1 || history.Turns[0].TurnID != "turn-loom" || history.Turns[0].Content[0].Text != "done" {
		t.Fatalf("history = %#v", history)
	}
}

func TestClaudeCanonicalTurnLedgerReadsPreProvenanceUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "canonical-turn-ledger.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"agents":{"agent-old":[{"turnId":"turn-old","state":"completed","content":[],"usage":{"totalTokens":{"available":true,"value":9},"calls":{"available":true,"value":1}}}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	history, err := st.LoadCanonicalTurnLedger("agent-old", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	usage := history.Turns[0].Usage
	if usage == nil || usage.TotalTokens.Source != "canonical_turn_ledger" || usage.InputTokens.Available || usage.InputTokens.Source != "runtime_unavailable" {
		t.Fatalf("normalized legacy usage = %#v", usage)
	}
}
