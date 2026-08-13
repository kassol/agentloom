package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	githubapi "github.com/yan5xu/codex-loom/internal/github"
	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
	"github.com/zalando/go-keyring"
)

func TestGenericCredentialAPIHandlesPreflightMigrationAndRollbackWithoutSecrets(t *testing.T) {
	keyring.MockInit()
	secret := "github-api-secret-must-not-project"
	keychainRef, err := githubapi.SaveScopedToken("owner", "*", secret)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		h.Shutdown()
		_ = st.Close()
	}()
	connection, err := h.CreateConnection(hub.ConnectionParams{
		Provider: "github", AccountRef: "owner", ScopeRef: "*", CredentialRef: keychainRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	preflight := serveCredentialJSON(t, handler, http.MethodGet, "/api/integrations/credentials/preflight", "", http.StatusOK)
	values := preflight["connections"].([]any)
	if len(values) != 1 || values[0].(map[string]any)["connectionId"] != connection.ID || values[0].(map[string]any)["eligible"] != true {
		t.Fatalf("preflight = %#v", preflight)
	}
	dryRun := serveCredentialJSON(t, handler, http.MethodPost, "/api/integrations/credentials/migrate",
		`{"connectionId":"`+connection.ID+`","dryRun":true}`, http.StatusOK)
	if dryRun["receipt"].(map[string]any)["status"] != "would_migrate" {
		t.Fatalf("dry-run = %#v", dryRun)
	}
	migrated := serveCredentialJSON(t, handler, http.MethodPost, "/api/integrations/credentials/migrations",
		`{"connectionId":"`+connection.ID+`","confirm":"`+connection.ID+`"}`, http.StatusOK)
	receipt := migrated["receipt"].(map[string]any)
	receiptID := receipt["id"].(string)
	if receipt["status"] != "completed" || !strings.HasPrefix(receipt["managedRef"].(string), "managed:") {
		t.Fatalf("migration = %#v", migrated)
	}
	encoded, _ := json.Marshal(migrated)
	if bytes.Contains(encoded, []byte(secret)) {
		t.Fatal("credential API leaked secret material")
	}
	rolledBack := serveCredentialJSON(t, handler, http.MethodPost, "/api/integrations/credentials/receipts/"+receiptID+"/rollback",
		`{"confirm":"`+receiptID+`"}`, http.StatusOK)
	if rolledBack["receipt"].(map[string]any)["status"] != "rolled_back" {
		t.Fatalf("rollback = %#v", rolledBack)
	}
	legacy, err := githubapi.LoadCredential(keychainRef)
	if err != nil || legacy != secret {
		t.Fatalf("Keychain compatibility source was changed: %q err=%v", legacy, err)
	}
}

func serveCredentialJSON(t *testing.T, handler http.Handler, method, path, body string, status int) map[string]any {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("%s %s = %d: %s", method, path, response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}
