package claudegen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerStagesActivatesAndRollsBackVerifiedGenerations(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	first := testManifest("claude-test-a", server.URL, archive)
	manager := New(Options{Root: root, Manifest: first, Platform: testPlatform()})

	status := manager.Status(context.Background())
	if status.State != StateInstallRequired || status.Required.ID != first.ID || status.Active != nil {
		t.Fatalf("initial status = %#v", status)
	}
	if _, err := manager.Install(context.Background(), InstallRequest{}); err == nil {
		t.Fatal("install without terms acknowledgement succeeded")
	}
	staged, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	if staged.State != StateStaged || staged.Staged == nil || staged.Staged.ID != first.ID || staged.Active != nil {
		t.Fatalf("staged status = %#v", staged)
	}
	if _, err := manager.Verify(context.Background(), "stgaed"); err == nil || !strings.Contains(PublicMessage(err), "active or staged") {
		t.Fatalf("invalid verification target = %v", err)
	}
	active, err := manager.Activate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active.State != StateActive || active.Active == nil || active.Active.ID != first.ID || active.Previous != nil {
		t.Fatalf("active status = %#v", active)
	}
	if err := manager.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	launch, err := manager.ResolveActive(context.Background())
	if err != nil {
		t.Fatalf("ResolveActive: %v", err)
	}
	if launch.Manifest.ID != first.ID || filepath.Base(launch.NodePath) != "node" || filepath.Base(launch.BridgePath) != "bridge.mjs" || strings.Contains(launch.NodePath, "/usr/") {
		t.Fatalf("active launch spec = %#v", launch)
	}

	second := testManifest("claude-test-b", server.URL, archive)
	manager = New(Options{Root: root, Manifest: second, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{}); err != nil {
		t.Fatalf("accepted terms were not reused: %v", err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := manager.Rollback(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Active == nil || rolledBack.Active.ID != first.ID || rolledBack.Previous == nil || rolledBack.Previous.ID != second.ID {
		t.Fatalf("rollback status = %#v", rolledBack)
	}
	entries, err := os.ReadDir(filepath.Join(root, "generations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("generation count = %d, want active + previous", len(entries))
	}
}

func TestActiveGenerationIsImmutableAndPreflightChecksBridgeIntegrity(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	manifest := testManifest("claude-test-immutable", server.URL, archive)
	manager := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "generations", manifest.ID, "app", "bridge.mjs")
	if err := os.Chmod(bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridge, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Preflight(context.Background()); err == nil {
		t.Fatal("Preflight accepted a damaged bridge")
	}
	if status := manager.Status(context.Background()); status.State != StateBroken {
		t.Fatalf("damaged active status = %#v", status)
	}
	if _, err := manager.Install(context.Background(), InstallRequest{}); err == nil {
		t.Fatal("install replaced an existing immutable generation")
	}
	data, err := os.ReadFile(bridge)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "damaged" {
		t.Fatalf("existing generation was overwritten: %q", data)
	}
}

func TestDamagedPreviousGenerationKeepsActiveAvailableAndExplainsRollback(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	root := t.TempDir()
	first := testManifest("claude-previous-a", server.URL, archive)
	manager := New(Options{Root: root, Manifest: first, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := testManifest("claude-previous-b", server.URL, archive)
	manager = New(Options{Root: root, Manifest: second, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	removeGeneration(filepath.Join(root, "generations", first.ID))
	status := manager.Status(context.Background())
	if status.State != StateActive || status.Active == nil || status.Previous != nil || !strings.Contains(status.Reason, "rollback is unavailable") {
		t.Fatalf("status = %#v", status)
	}
}

func TestVerifiedOrphanGenerationCanBeRestagedWithoutRedownload(t *testing.T) {
	archive := fakeNodeArchive(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manifest := testManifest("claude-orphan", server.URL, archive)
	manager := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateStaged || status.Staged == nil || requests != 1 {
		t.Fatalf("restaged status=%#v requests=%d", status, requests)
	}
}

func TestExistingGenerationPersistsNewTermsAcknowledgement(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manifest := testManifest("claude-terms", server.URL, archive)
	manager := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	manifest.TermsRevision = "new-terms"
	manager = New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	reopened := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if status := reopened.Status(context.Background()); !status.TermsAccepted {
		t.Fatalf("new terms acknowledgement was not durable: %#v", status)
	}
}

func TestStatusReportsUnreadableStateAsBroken(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(Options{Root: root, Manifest: CurrentManifest(), Platform: testPlatform()})
	status := manager.Status(context.Background())
	if status.State != StateBroken || !strings.Contains(status.Reason, "unreadable") {
		t.Fatalf("status = %#v", status)
	}
}

func TestCurrentCompatibilityRowAndSupportedPlatformsAreExact(t *testing.T) {
	manifest := CurrentManifest()
	if manifest.NodeVersion != "24.19.0" || manifest.SDKVersion != "0.3.228" || manifest.ClaudeCodeVersion != "2.1.228" || len(manifest.Platforms) != 4 {
		t.Fatalf("current manifest = %#v", manifest)
	}
	if manifest.BridgeSHA256 == "" || manifest.PackageLockSHA256 == "" || manifest.SDKIntegrity == "" {
		t.Fatalf("current manifest lacks integrity: %#v", manifest)
	}

	tests := []struct {
		name                        string
		platform                    Platform
		wantSupported               bool
		wantReason, wantAlternative string
	}{
		{"macOS arm64", ClassifyPlatform("darwin", "arm64", "macos", "14.0", ""), true, "", ""},
		{"Ubuntu x64", ClassifyPlatform("linux", "amd64", "ubuntu", "22.04", "glibc"), true, "", ""},
		{"musl", ClassifyPlatform("linux", "amd64", "alpine", "3.20", "musl"), false, "musl", "Ubuntu"},
		{"Windows", ClassifyPlatform("windows", "amd64", "windows", "11", ""), false, "Windows", "Ubuntu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.platform.Supported != tt.wantSupported || !strings.Contains(tt.platform.Reason, tt.wantReason) || !strings.Contains(tt.platform.Alternative, tt.wantAlternative) {
				t.Fatalf("platform = %#v", tt.platform)
			}
		})
	}
}

func TestCompatibilityVerificationRejectsVersionAndCapabilityMismatch(t *testing.T) {
	manifest := testManifest("claude-compatibility", "https://example.invalid", []byte("archive"))
	root := t.TempDir()
	node := filepath.Join(root, "node")
	bridge := filepath.Join(root, "bridge.mjs")
	if err := os.WriteFile(bridge, []byte("// test bridge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := `{"protocolVersion":1,"bridgeBuild":"test-bridge","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","capabilities":["interrupt"]}`
	for name, output := range map[string]string{
		"protocol version": strings.Replace(valid, `"protocolVersion":1`, `"protocolVersion":2`, 1),
		"bridge version":   strings.Replace(valid, `"bridgeBuild":"test-bridge"`, `"bridgeBuild":"wrong"`, 1),
		"node version":     strings.Replace(valid, `"nodeVersion":"24.19.0"`, `"nodeVersion":"24.18.0"`, 1),
		"sdk version":      strings.Replace(valid, `"sdkVersion":"0.3.228"`, `"sdkVersion":"0.3.227"`, 1),
		"Claude Code":      strings.Replace(valid, `"claudeCodeVersion":"2.1.228"`, `"claudeCodeVersion":"2.1.227"`, 1),
		"capabilities":     strings.Replace(valid, `["interrupt"]`, `["interrupt","extra"]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			script := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\n"
			if err := os.WriteFile(node, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := runSelfTest(context.Background(), node, bridge, manifest); err == nil {
				t.Fatal("mismatched self-test succeeded")
			}
		})
	}

	app := filepath.Join(root, "app")
	artifact := manifest.Platforms[0]
	for path, version := range map[string]string{
		filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"):      manifest.SDKVersion,
		filepath.Join(app, "node_modules", filepath.FromSlash(artifact.PackageName), "package.json"): artifact.PackageVersion,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"version":%q}`, version)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, path := range map[string]string{
		"SDK package":      filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"),
		"platform package": filepath.Join(app, "node_modules", filepath.FromSlash(artifact.PackageName), "package.json"),
	} {
		t.Run(name, func(t *testing.T) {
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"version":"0.3.227"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyPackageVersions(app, manifest, artifact); err == nil {
				t.Fatal("mismatched installed package version succeeded")
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallEnvironmentUsesOnlyPinnedNodePath(t *testing.T) {
	t.Setenv("PATH", "/system/bin:/other/bin")
	environment := installEnvironment("/pinned/node/bin", "/cache")
	if environment[0] != "PATH=/pinned/node/bin" {
		t.Fatalf("environment PATH = %q", environment[0])
	}
}

func TestActivationIsQuiescedAndConcurrentMutationIsRejected(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manager := New(Options{Root: root, Manifest: testManifest("claude-quiesced", server.URL, archive), Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	manager.quiesce = func(context.Context) error {
		close(started)
		<-release
		return nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := manager.Activate(context.Background())
		result <- err
	}()
	<-started
	if status := manager.Status(context.Background()); status.State != StateStaged {
		t.Fatalf("status blocked or changed during quiesce: %#v", status)
	}
	if _, err := manager.Activate(context.Background()); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent activation = %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedAndFailedInstallHaveNoDurableGeneration(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("not the pinned archive"))
	}))
	t.Cleanup(server.Close)
	root := filepath.Join(t.TempDir(), "runtime")
	manifest := testManifest("claude-failure", server.URL, []byte("expected"))
	unsupported := New(Options{Root: root, Manifest: manifest, Platform: Platform{OS: "windows", Arch: "x64", Reason: "unsupported"}})
	if _, err := unsupported.Install(context.Background(), InstallRequest{AcceptTerms: true}); err == nil || requests != 0 {
		t.Fatalf("unsupported install err=%v requests=%d", err, requests)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unsupported install mutated root: %v", err)
	}
	failed := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := failed.Install(context.Background(), InstallRequest{AcceptTerms: true}); err == nil {
		t.Fatal("integrity failure succeeded")
	}
	if status := failed.Status(context.Background()); status.Staged != nil || status.Active != nil {
		t.Fatalf("failed install became durable: %#v", status)
	}
}

func TestManifestPackageIntegrityMismatchFailsBeforeDownload(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(fakeNodeArchive(t))
	}))
	t.Cleanup(server.Close)
	manifest := CurrentManifest()
	manifest.Platforms[0].NodeURL = server.URL
	manifest.Platforms[0].PackageIntegrity = "sha512-wrong"
	manager := New(Options{Root: t.TempDir(), Manifest: manifest, Platform: Platform{OS: "darwin", Arch: "arm64", Supported: true}})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err == nil || requests != 0 {
		t.Fatalf("integrity mismatch err=%v requests=%d", err, requests)
	}
}

func TestPreflightReportsExactRowOrExplicitAvailability(t *testing.T) {
	missing := New(Options{Root: t.TempDir(), Manifest: testManifest("claude-preflight", "https://example.invalid", []byte("archive")), Platform: testPlatform()})
	if report := missing.InspectPreflight(context.Background()); report.Availability != "unavailable" || report.Required.ID != "claude-preflight" || report.Alternative == "" {
		t.Fatalf("missing preflight = %#v", report)
	}
	unsupported := New(Options{Root: t.TempDir(), Manifest: CurrentManifest(), Platform: Platform{OS: "windows", Arch: "x64", Reason: "Windows unsupported", Alternative: "Use Ubuntu"}})
	if report := unsupported.InspectPreflight(context.Background()); report.Availability != "unsupported" || !strings.Contains(report.Reason, "Windows") || report.Alternative == "" {
		t.Fatalf("unsupported preflight = %#v", report)
	}
}

func TestCanceledInstallLeavesNoGenerationAndManagerCanRetry(t *testing.T) {
	archive := fakeNodeArchive(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
			_, _ = w.Write(archive)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manager := New(Options{Root: root, Manifest: testManifest("claude-cancel", server.URL, archive), Platform: testPlatform()})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := manager.Install(ctx, InstallRequest{AcceptTerms: true})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; err == nil {
		t.Fatal("canceled install succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(root, "generations"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 || manager.Status(context.Background()).Staged != nil {
		t.Fatalf("canceled install left durable generation: %#v", entries)
	}
	close(release)
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
}

func testManifest(id, url string, archive []byte) Manifest {
	digest := sha256.Sum256(archive)
	bridgeDigest := sha256.Sum256(currentAssets().Bridge)
	lockDigest := sha256.Sum256(currentAssets().PackageLock)
	return Manifest{
		ID: id, Compatibility: "claude-runtime-v1", BridgeProtocol: 1, BridgeBuild: "test-bridge",
		BridgeSHA256: fmt.Sprintf("%x", bridgeDigest), NodeVersion: "24.19.0", SDKVersion: "0.3.228", ClaudeCodeVersion: "2.1.228", PackageLockSHA256: fmt.Sprintf("%x", lockDigest),
		TermsRevision: "2026-08-12", TermsURL: "https://example.test/terms",
		RequiredCapabilities: []string{"interrupt"},
		Platforms:            []PlatformArtifact{{OS: "linux", Arch: "x64", NodeURL: url, NodeSHA256: fmt.Sprintf("%x", digest), PackageName: "@anthropic-ai/claude-agent-sdk-linux-x64", PackageVersion: "0.3.228"}},
	}
}

func testPlatform() Platform {
	return Platform{OS: "linux", Arch: "x64", Distribution: "ubuntu", Version: "22.04", Libc: "glibc", Supported: true}
}

func fakeNodeArchive(t *testing.T) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	files := map[string]struct {
		mode int64
		body string
	}{
		"node-test/bin/node": {0o755, `#!/bin/sh
case "$1" in
*npm-cli.js*)
  /bin/mkdir -p node_modules/@anthropic-ai/claude-agent-sdk node_modules/@anthropic-ai/claude-agent-sdk-linux-x64
  printf '{"version":"0.3.228","claudeCodeVersion":"2.1.228"}' > node_modules/@anthropic-ai/claude-agent-sdk/package.json
  printf '{"version":"0.3.228"}' > node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/package.json
  exit 0
  ;;
esac
printf '{"protocolVersion":1,"bridgeBuild":"test-bridge","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","capabilities":["interrupt"]}\n'
`},
		"node-test/lib/node_modules/npm/bin/npm-cli.js": {0o644, "// test npm cli\n"},
	}
	for name, file := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: file.mode, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
