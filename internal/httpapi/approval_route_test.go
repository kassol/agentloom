package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestApprovalRouteRejectsModifiedInputAsJSON(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	handler := New(h, st, fstest.MapFS{"index.html": {Data: []byte("spa")}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/agents/agent-1/thread/approvals/approval-1", strings.NewReader(`{"decision":"approve","modifiedInput":{"command":"changed"}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Header().Get("Content-Type"), "application/json") || !strings.Contains(response.Body.String(), "modified Approval input is unavailable") {
		t.Fatalf("response = %d %s %s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	exact := httptest.NewRequest(http.MethodPost, "/api/agents/agent-1/thread/approvals/approval-1", strings.NewReader(`{"decision":"approve"}`))
	exact.Header.Set("Content-Type", "application/json")
	exactResponse := httptest.NewRecorder()
	handler.ServeHTTP(exactResponse, exact)
	if exactResponse.Code != http.StatusNotFound || !strings.Contains(exactResponse.Header().Get("Content-Type"), "application/json") || strings.Contains(exactResponse.Body.String(), "spa") {
		t.Fatalf("exact route response = %d %s %s", exactResponse.Code, exactResponse.Header().Get("Content-Type"), exactResponse.Body.String())
	}
}
