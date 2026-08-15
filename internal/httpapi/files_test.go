package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestHostFileMetadataIncludesHiddenEntriesAndAbsolutePaths(t *testing.T) {
	server, root, _, _ := newFileServer(t)
	defer server.Close()

	response := fileRequest(t, server, http.MethodGet, "/api/files", root)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("directory status = %d", response.StatusCode)
	}
	var listing fileListing
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != root || listing.Kind != "directory" || !listing.Readable {
		t.Fatalf("listing = %#v", listing)
	}
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
		if !strings.HasPrefix(entry.Path, root+string(os.PathSeparator)) {
			t.Fatalf("entry escaped root: %#v", entry)
		}
	}
	if !containsString(names, ".hidden") || !containsString(names, "nested") || !containsString(names, "probe.txt") {
		t.Fatalf("entry names = %#v", names)
	}

	filePath := filepath.Join(root, "nested", "..", "probe.txt")
	response = fileRequest(t, server, http.MethodGet, "/api/files", filePath)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("file status = %d", response.StatusCode)
	}
	var file fileListing
	if err := json.NewDecoder(response.Body).Decode(&file); err != nil {
		t.Fatal(err)
	}
	if file.Path != filepath.Join(root, "probe.txt") || file.Kind != "file" || file.Size != 10 || !file.Readable {
		t.Fatalf("file metadata = %#v", file)
	}
}

func TestHostFilePreviewIsBoundedAndContentSupportsRange(t *testing.T) {
	server, root, _, _ := newFileServer(t)
	defer server.Close()
	largePath := filepath.Join(root, "large.log")

	response := fileRequestWithQuery(t, server, http.MethodGet, "/api/files/preview", largePath, map[string]string{"maxBytes": "16"})
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(data) != 16 || response.Header.Get("X-Codex-Loom-Preview-Truncated") != "true" {
		t.Fatalf("preview status=%d len=%d truncated=%q", response.StatusCode, len(data), response.Header.Get("X-Codex-Loom-Preview-Truncated"))
	}
	if response.Header.Get("X-Codex-Loom-Preview-Limit") != "16" {
		t.Fatalf("preview limit = %q", response.Header.Get("X-Codex-Loom-Preview-Limit"))
	}

	response = fileRequestWithHeaders(t, server, http.MethodGet, "/api/files/content", filepath.Join(root, "probe.txt"), map[string]string{"Range": "bytes=2-5"})
	data, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPartialContent || string(data) != "2345" || response.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range status=%d body=%q content-range=%q", response.StatusCode, data, response.Header.Get("Content-Range"))
	}

	response = fileRequestWithHeaders(t, server, http.MethodGet, "/api/files/content", filepath.Join(root, "probe.txt"), map[string]string{"Range": "bytes=99-100"})
	response.Body.Close()
	if response.StatusCode != http.StatusRequestedRangeNotSatisfiable || response.Header.Get("Content-Range") != "bytes */10" {
		t.Fatalf("invalid range status=%d content-range=%q", response.StatusCode, response.Header.Get("Content-Range"))
	}

	unknownPath := filepath.Join(root, "unknown.data")
	response = fileRequestWithQuery(t, server, http.MethodGet, "/api/files/content", unknownPath, map[string]string{"download": "1"})
	data, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(data) != "unknown bytes" {
		t.Fatalf("unknown download status=%d body=%q err=%v", response.StatusCode, data, err)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("unknown disposition = %q", response.Header.Get("Content-Disposition"))
	}
}

func TestHostFileErrorsAreClassifiableAndReadOnly(t *testing.T) {
	server, root, _, _ := newFileServer(t)
	defer server.Close()

	tests := []struct {
		name     string
		path     string
		endpoint string
		status   int
		code     string
	}{
		{name: "relative", path: "probe.txt", endpoint: "/api/files", status: http.StatusBadRequest, code: "path_must_be_absolute"},
		{name: "missing", path: filepath.Join(root, "missing.txt"), endpoint: "/api/files", status: http.StatusNotFound, code: "not_found"},
		{name: "directory as content", path: root, endpoint: "/api/files/content", status: http.StatusConflict, code: "not_file"},
		{name: "file as preview limit", path: filepath.Join(root, "probe.txt"), endpoint: "/api/files/preview", status: http.StatusBadRequest, code: "invalid_preview_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query := map[string]string{}
			if test.name == "file as preview limit" {
				query["maxBytes"] = "0"
			}
			response := fileRequestWithQuery(t, server, http.MethodGet, test.endpoint, test.path, query)
			defer response.Body.Close()
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != test.status || body.Error.Code != test.code {
				t.Fatalf("status=%d code=%q, want status=%d code=%q", response.StatusCode, body.Error.Code, test.status, test.code)
			}
		})
	}

	if _, err := os.Stat(filepath.Join(root, "probe.txt")); err != nil {
		t.Fatalf("fixture disappeared after read-only requests: %v", err)
	}
}

func TestHostFileMediaPreviewUsesInlineDisposition(t *testing.T) {
	if got := fileDisposition("video/mp4", true, false); got != "inline" {
		t.Fatalf("video preview disposition = %q", got)
	}
	if got := fileDisposition("audio/mpeg", true, false); got != "inline" {
		t.Fatalf("audio preview disposition = %q", got)
	}
	if got := fileDisposition("image/svg+xml", true, false); got != "attachment" {
		t.Fatalf("SVG preview disposition = %q", got)
	}
}

func TestHostFileFenceLimitsAccessToAllowedRoots(t *testing.T) {
	server, root, agentCwd, _ := newFileServer(t)
	defer server.Close()

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/api/files", "/api/files/preview", "/api/files/content"} {
		response := fileRequest(t, server, http.MethodGet, endpoint, outsideFile)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		err := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusForbidden || body.Error.Code != "path_not_allowed" {
			t.Fatalf("%s outside root status=%d code=%q", endpoint, response.StatusCode, body.Error.Code)
		}
	}

	escape := filepath.Join(root, "escape-link")
	if err := os.Symlink(outsideFile, escape); err != nil {
		t.Fatal(err)
	}
	response := fileRequest(t, server, http.MethodGet, "/api/files/content", escape)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("symlink escape status = %d", response.StatusCode)
	}

	agentFile := filepath.Join(agentCwd, "notes.txt")
	if err := os.WriteFile(agentFile, []byte("agent notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	response = fileRequest(t, server, http.MethodGet, "/api/files/content", agentFile)
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(data) != "agent notes" {
		t.Fatalf("agent cwd read status=%d body=%q err=%v", response.StatusCode, data, err)
	}
}

func TestHostFileListingWithoutPathReturnsAllowedRoots(t *testing.T) {
	server, _, agentCwd, dataDir := newFileServer(t)
	defer server.Close()

	response, err := http.Get(server.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("roots status = %d", response.StatusCode)
	}
	var listing fileListing
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if listing.Kind != "roots" || listing.Path != "" || !listing.Readable {
		t.Fatalf("roots listing = %#v", listing)
	}
	var paths []string
	for _, entry := range listing.Entries {
		paths = append(paths, entry.Path)
	}
	if !containsString(paths, filepath.Clean(dataDir)) || !containsString(paths, filepath.Clean(agentCwd)) {
		t.Fatalf("root paths = %#v, want data dir %q and agent cwd %q", paths, dataDir, agentCwd)
	}
}

// newFileServer starts a Server whose allowed roots are the store data dir and
// one Agent cwd; the returned fixture root lives inside the data dir. Paths are
// symlink-resolved so listings compare stably on macOS temp dirs.
func newFileServer(t *testing.T) (server *httptest.Server, fixtureRoot, agentCwd, dataDir string) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agentCwd = t.TempDir()
	if err := st.SaveAgents(map[string]*hub.Agent{
		"file-agent": {ID: "file-agent", Name: "file-agent", Cwd: agentCwd, ThreadID: "loom-thread-files", RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "thread-files"}, Status: "idle"},
	}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	t.Cleanup(h.Shutdown)
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}
	server = httptest.NewServer(New(h, st, web).Handler())
	return server, fileFixture(t, st.Dir()), agentCwd, st.Dir()
}

func fileFixture(t *testing.T, base string) string {
	t.Helper()
	root := filepath.Join(base, "files-fixture")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		filepath.Join(root, "probe.txt"):    []byte("0123456789"),
		filepath.Join(root, ".hidden"):      []byte("hidden"),
		filepath.Join(root, "unknown.data"): []byte("unknown bytes"),
		filepath.Join(root, "large.log"):    bytes.Repeat([]byte("x"), defaultFilePreviewBytes+64),
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func fileRequest(t *testing.T, server *httptest.Server, method, endpoint, path string) *http.Response {
	return fileRequestWithHeaders(t, server, method, endpoint, path, nil)
}

func fileRequestWithQuery(t *testing.T, server *httptest.Server, method, endpoint, path string, extra map[string]string) *http.Response {
	values := url.Values{"path": []string{path}}
	for key, value := range extra {
		values.Set(key, value)
	}
	request, err := http.NewRequest(method, server.URL+endpoint+"?"+values.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func fileRequestWithHeaders(t *testing.T, server *httptest.Server, method, endpoint, path string, headers map[string]string) *http.Response {
	request, err := http.NewRequest(method, server.URL+endpoint+"?path="+url.QueryEscape(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
