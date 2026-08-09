package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestLarkMigrateCLIEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := h.CreateConnection(hub.ConnectionParams{Provider: "lark", CredentialRef: "keychain:com.codexloom.lark"})
	if err != nil {
		t.Fatal(err)
	}
	h.Shutdown()
	_ = st.Close()

	secretPath := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("cli-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmdLarkMigrate(args{
		positional: []string{"migrate"},
		flags:      map[string]string{"data": dir, "connection": connection.ID, "source": secretPath},
	})

	migratedRef := openConnectionRef(t, dir, connection.ID)
	if !strings.HasPrefix(migratedRef, "managed:") {
		t.Fatalf("connection not migrated: %q", migratedRef)
	}
	cmdLarkMigrate(args{
		positional: []string{"verify"},
		flags:      map[string]string{"data": dir, "connection": connection.ID},
	})
	cmdLarkMigrate(args{
		positional: []string{"rollback"},
		flags:      map[string]string{"data": dir, "connection": connection.ID},
	})
	restoredRef := openConnectionRef(t, dir, connection.ID)
	if restoredRef != "keychain:com.codexloom.lark" {
		t.Fatalf("rollback did not restore the previous reference: %q", restoredRef)
	}
}

func openConnectionRef(t *testing.T, dir, connectionID string) string {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	defer func() {
		h.Shutdown()
		_ = st.Close()
	}()
	for _, candidate := range h.ListConnections() {
		if candidate.ID == connectionID {
			return candidate.CredentialRef
		}
	}
	t.Fatalf("connection %s not found", connectionID)
	return ""
}
