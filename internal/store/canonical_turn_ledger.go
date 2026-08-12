package store

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

const canonicalTurnLedgerVersion = 1

type canonicalTurnLedger struct {
	Version int                                      `json:"version"`
	Agents  map[string][]runtimecontract.HistoryTurn `json:"agents"`
}

func (s *Store) canonicalTurnLedgerFile() string {
	return filepath.Join(s.dir, "canonical-turn-ledger.json")
}

// SaveCanonicalTurnLedger atomically replaces one Agent's Loom-owned Turn
// snapshots. Callers must sanitize Runtime-native identifiers and sensitive
// output before crossing this storage boundary.
func (s *Store) SaveCanonicalTurnLedger(agentID string, turns []runtimecontract.HistoryTurn) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return fmt.Errorf("Canonical Turn Ledger Agent ID is required")
	}
	history := runtimecontract.History{Total: len(turns), Turns: turns}
	if err := history.Validate(); err != nil {
		return fmt.Errorf("invalid Canonical Turn Ledger: %w", err)
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	ledger := canonicalTurnLedger{Version: canonicalTurnLedgerVersion, Agents: map[string][]runtimecontract.HistoryTurn{}}
	if err := s.loadJSON(s.canonicalTurnLedgerFile(), &ledger); err != nil {
		return err
	}
	if ledger.Version != canonicalTurnLedgerVersion {
		return fmt.Errorf("unsupported Canonical Turn Ledger version %d", ledger.Version)
	}
	if ledger.Agents == nil {
		ledger.Agents = map[string][]runtimecontract.HistoryTurn{}
	}
	ledger.Agents[agentID] = append([]runtimecontract.HistoryTurn(nil), turns...)
	return s.saveJSON(s.canonicalTurnLedgerFile(), ledger)
}

// LoadCanonicalTurnLedger reads stable cold History without acquiring or
// starting a Runtime process.
func (s *Store) LoadCanonicalTurnLedger(agentID string, count, offset int) (runtimecontract.History, error) {
	if strings.TrimSpace(agentID) == "" {
		return runtimecontract.History{}, fmt.Errorf("Canonical Turn Ledger Agent ID is required")
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	ledger := canonicalTurnLedger{Version: canonicalTurnLedgerVersion, Agents: map[string][]runtimecontract.HistoryTurn{}}
	if err := s.loadJSON(s.canonicalTurnLedgerFile(), &ledger); err != nil {
		return runtimecontract.History{}, err
	}
	if ledger.Version != canonicalTurnLedgerVersion {
		return runtimecontract.History{}, fmt.Errorf("unsupported Canonical Turn Ledger version %d", ledger.Version)
	}
	turns := ledger.Agents[agentID]
	normalizeCanonicalTurnLedgerUsage(turns)
	total := len(turns)
	if count <= 0 {
		count = 10
	}
	if offset < 0 {
		offset = 0
	}
	end := total - offset
	if end < 0 {
		end = 0
	}
	start := end - count
	if start < 0 {
		start = 0
	}
	history := runtimecontract.History{Total: total, Turns: append([]runtimecontract.HistoryTurn(nil), turns[start:end]...)}
	if err := history.Validate(); err != nil {
		return runtimecontract.History{}, fmt.Errorf("invalid Canonical Turn Ledger: %w", err)
	}
	return history, nil
}

// Canonical Turn Ledger v1 predates per-field usage provenance. Preserve
// readable v1 data by marking omitted provenance explicitly at the cold-read
// boundary; newly written snapshots are already fully attributed.
func normalizeCanonicalTurnLedgerUsage(turns []runtimecontract.HistoryTurn) {
	for index := range turns {
		if turns[index].Usage == nil {
			continue
		}
		usage := turns[index].Usage
		for _, metric := range []*runtimecontract.UsageMetric{
			&usage.InputTokens, &usage.CachedInputTokens, &usage.OutputTokens,
			&usage.ReasoningOutputTokens, &usage.TotalTokens, &usage.Calls, &usage.CostMicros,
		} {
			if metric.Source == "" {
				if metric.Available {
					metric.Source = "canonical_turn_ledger"
				} else {
					metric.Source = "runtime_unavailable"
				}
			}
		}
	}
}
