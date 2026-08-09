package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

// cmdLarkMigrate is the narrow local operator flow for one Lark Connection.
// It runs in-process against an isolated data directory (maintenance mode:
// the codex-loom server must be stopped so this process is the single
// writer). No secret ever enters arguments, output, logs, ordinary backups,
// or integrations durable state.
func cmdLarkMigrate(a args) {
	if len(a.positional) == 0 {
		usage("lark-migrate preflight|dry-run|migrate|verify|rollback --data DIR --connection ID [--source PATH]")
	}
	action := strings.ToLower(strings.TrimSpace(a.positional[0]))
	dataDir := strings.TrimSpace(a.flags["data"])
	if dataDir == "" {
		dataDir = store.DefaultDir()
	}
	connectionID := strings.TrimSpace(a.flags["connection"])
	if connectionID == "" {
		usage("lark-migrate ... --connection ID")
	}
	st, err := store.Open(dataDir)
	if err != nil {
		fail(fmt.Errorf("open data directory (is the server stopped?): %w", err))
	}
	h, err := hub.Open(st)
	if err != nil {
		_ = st.Close()
		fail(fmt.Errorf("open Hub: %w", err))
	}
	defer func() {
		h.Shutdown()
		_ = st.Close()
	}()
	ctx := context.Background()
	switch action {
	case "preflight", "dry-run":
		result, err := h.MigrateLarkCredential(ctx, connectionID, nil, true)
		if err != nil {
			fail(err)
		}
		printJSON(map[string]any{
			"action": action, "connectionId": result.ConnectionID, "currentRef": result.CurrentRef,
			"floorRaised": result.FloorRaised, "wouldRaiseFloor": !result.FloorRaised,
			"alreadyMigrated": result.AlreadyMigrated,
		})
	case "migrate":
		source := strings.TrimSpace(a.flags["source"])
		if source == "" {
			usage("lark-migrate migrate --source PATH")
		}
		secretText, err := readOwnerOnlySecretFile(source)
		if err != nil {
			fail(err)
		}
		result, err := h.MigrateLarkCredential(ctx, connectionID, []byte(secretText), false)
		if err != nil {
			fail(err)
		}
		fmt.Printf("%s Lark connection %s now uses %s\n", green("migrated"), bold(result.ConnectionID), result.CurrentRef)
		if result.FloorRaised {
			fmt.Println("  credential writer floor raised; old builds are blocked from this data directory")
		}
	case "verify":
		result, err := h.VerifyLarkCredential(connectionID)
		if err != nil {
			fail(err)
		}
		fmt.Printf("%s Lark connection %s credential reference %s resolves\n", green("verified"), bold(result.ConnectionID), result.CurrentRef)
	case "rollback":
		result, err := h.RollbackLarkCredential(connectionID)
		if err != nil {
			fail(err)
		}
		fmt.Printf("%s Lark connection %s restored to %s\n", green("rolled back"), bold(result.ConnectionID), result.CurrentRef)
	default:
		usage("lark-migrate preflight|dry-run|migrate|verify|rollback ...")
	}
}
