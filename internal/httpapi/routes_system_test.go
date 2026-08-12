package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yan5xu/codex-loom/internal/buildinfo"
	"github.com/yan5xu/codex-loom/internal/claudegen"
	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestClaudeRuntimeGenerationRoutesExposeOneCanonicalLifecycle(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	manager := claudegen.New(claudegen.Options{
		Root: t.TempDir(), Manifest: claudegen.CurrentManifest(),
		Platform: claudegen.Platform{OS: "windows", Arch: "x64", Supported: false, Reason: "Windows is unsupported", Alternative: "Use a supported host."},
	})
	server := NewWithOptions(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}, Options{ClaudeGenerations: manager}).Handler()

	status := httptest.NewRecorder()
	server.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/runtime-generations/claude", nil))
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"state":"unsupported"`)) || bytes.Contains(status.Body.Bytes(), []byte("nodeUrl")) {
		t.Fatalf("GET generation = %d %s", status.Code, status.Body.String())
	}
	for _, path := range []string{"install", "verify", "activate", "rollback"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/runtime-generations/claude/"+path, bytes.NewReader([]byte(`{"acceptTerms":true,"target":"staged"}`)))
		request.RemoteAddr = "127.0.0.1:42000"
		server.ServeHTTP(response, request)
		if response.Code == http.StatusNotFound || response.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("POST %s = %d %s", path, response.Code, response.Body.String())
		}
	}
	forbidden := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/runtime-generations/claude/install", bytes.NewReader([]byte(`{"acceptTerms":true}`)))
	request.RemoteAddr = "203.0.113.9:42000"
	server.ServeHTTP(forbidden, request)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("remote install = %d %s", forbidden.Code, forbidden.Body.String())
	}
}

func TestShutdownWaitsForEnteredClaudeGenerationMutation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	downloadStarted := make(chan struct{})
	download := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(downloadStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(download.Close)
	manifest := claudegen.CurrentManifest()
	manifest.Platforms[0].NodeURL = download.URL
	manager := claudegen.New(claudegen.Options{Root: t.TempDir(), Manifest: manifest, Platform: claudegen.Platform{OS: "darwin", Arch: "arm64", Supported: true}})
	server := NewWithOptions(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}, Options{ClaudeGenerations: manager})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/runtime-generations/claude/install", bytes.NewReader([]byte(`{"acceptTerms":true}`))).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:42000"
	response := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(response, request)
		close(requestDone)
	}()
	<-downloadStarted
	shutdownDone := make(chan struct{})
	go func() {
		server.StopRuntimeGenerationOperations()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown did not wait for entered generation mutation")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	<-requestDone
	<-shutdownDone

	after := httptest.NewRecorder()
	next := httptest.NewRequest(http.MethodPost, "/api/runtime-generations/claude/activate", bytes.NewReader([]byte(`{}`)))
	next.RemoteAddr = "127.0.0.1:42000"
	server.Handler().ServeHTTP(after, next)
	if after.Code != http.StatusServiceUnavailable {
		t.Fatalf("mutation after shutdown fence = %d %s", after.Code, after.Body.String())
	}
}

func TestVersionReportsRunningArtifact(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	started := time.Date(2026, 7, 15, 2, 3, 4, 0, time.UTC)
	web := fstest.MapFS{"index.html": {Data: []byte(`<script src="/assets/index-test.js"></script>`)}}
	server := NewWithOptions(h, st, web, Options{StartedAt: started, Mode: "canary", ReadOnly: true})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Build buildinfo.Info `json:"build"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Build.Mode != "canary" || !response.Build.ReadOnly || response.Build.WebAsset != "assets/index-test.js" {
		t.Fatalf("build = %#v", response.Build)
	}
	if response.Build.StartedAt != "2026-07-15T02:03:04Z" {
		t.Fatalf("startedAt = %s", response.Build.StartedAt)
	}
}

func TestReadOnlyCanaryRejectsWritesAndExternalReads(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ro, err := st.OpenReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	h, err := hub.OpenWithOptions(ro, hub.OpenOptions{Passive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	server := NewWithOptions(h, ro, fstest.MapFS{"index.html": {Data: []byte("ok")}}, Options{Mode: "canary", ReadOnly: true}).Handler()

	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/agents"},
		{http.MethodDelete, "/api/remote/devices/example"},
		{http.MethodGet, "/api/integrations/providers/lark/discovery"},
		{http.MethodGet, "/api/remote/devices"},
	} {
		request := httptest.NewRequest(test.method, test.path, bytes.NewReader([]byte(`{}`)))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", test.method, test.path, response.Code)
		}
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/agents = %d: %s", response.Code, response.Body.String())
	}
}

func TestModelProviderMutationsRequireLocalOrAdminRequest(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/model-providers/deepseek"},
		{http.MethodDelete, "/api/model-providers/deepseek"},
		{http.MethodPost, "/api/model-providers/deepseek/verify"},
	} {
		request := httptest.NewRequest(test.method, test.path, bytes.NewReader([]byte(`{}`)))
		request.RemoteAddr = "203.0.113.9:42000"
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", test.method, test.path, response.Code)
		}
	}
}
