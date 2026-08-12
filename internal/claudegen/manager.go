// Package claudegen installs and activates the one Claude Runtime generation
// supported by this Loom build.
package claudegen

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	StateInstallRequired = "install_required"
	StateStaged          = "staged"
	StateActive          = "active"
	StateBroken          = "broken"
	StateUnsupported     = "unsupported"
	maxNodeArchive       = 256 << 20
)

type PlatformArtifact struct {
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	NodeURL          string `json:"-"`
	NodeSHA256       string `json:"nodeSha256"`
	PackageName      string `json:"packageName"`
	PackageVersion   string `json:"packageVersion"`
	PackageIntegrity string `json:"packageIntegrity,omitempty"`
}

type Manifest struct {
	ID                   string             `json:"id"`
	Compatibility        string             `json:"compatibility"`
	BridgeProtocol       int                `json:"bridgeProtocol"`
	BridgeBuild          string             `json:"bridgeBuild"`
	BridgeSHA256         string             `json:"bridgeSha256,omitempty"`
	NodeVersion          string             `json:"nodeVersion"`
	SDKVersion           string             `json:"sdkVersion"`
	SDKIntegrity         string             `json:"sdkIntegrity,omitempty"`
	ClaudeCodeVersion    string             `json:"claudeCodeVersion"`
	PackageLockSHA256    string             `json:"packageLockSha256,omitempty"`
	TermsRevision        string             `json:"termsRevision"`
	TermsURL             string             `json:"termsUrl"`
	RequiredCapabilities []string           `json:"requiredCapabilities"`
	Platforms            []PlatformArtifact `json:"platforms"`
}

type Platform struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Distribution string `json:"distribution,omitempty"`
	Version      string `json:"version,omitempty"`
	Libc         string `json:"libc,omitempty"`
	Supported    bool   `json:"supported"`
	Reason       string `json:"reason,omitempty"`
	Alternative  string `json:"alternative,omitempty"`
}

type Generation struct {
	ID                string   `json:"id"`
	Compatibility     string   `json:"compatibility"`
	BridgeProtocol    int      `json:"bridgeProtocol"`
	BridgeBuild       string   `json:"bridgeBuild"`
	NodeVersion       string   `json:"nodeVersion"`
	SDKVersion        string   `json:"sdkVersion"`
	ClaudeCodeVersion string   `json:"claudeCodeVersion"`
	Capabilities      []string `json:"capabilities"`
	VerifiedAt        string   `json:"verifiedAt"`
}

type Status struct {
	State            string      `json:"state"`
	Required         Generation  `json:"required"`
	Active           *Generation `json:"active,omitempty"`
	Staged           *Generation `json:"staged,omitempty"`
	Previous         *Generation `json:"previous,omitempty"`
	Platform         Platform    `json:"platform"`
	TermsAccepted    bool        `json:"termsAccepted"`
	TermsRevision    string      `json:"termsRevision"`
	TermsURL         string      `json:"termsUrl"`
	TermsAcceptedAt  string      `json:"termsAcceptedAt,omitempty"`
	DeveloperPreview bool        `json:"developerPreview"`
	ProductionReady  bool        `json:"productionReady"`
	Reason           string      `json:"reason,omitempty"`
	Alternative      string      `json:"alternative,omitempty"`
}

type InstallRequest struct{ AcceptTerms bool }

type PreflightReport struct {
	Availability string      `json:"availability"`
	Required     Generation  `json:"required"`
	Active       *Generation `json:"active,omitempty"`
	Reason       string      `json:"reason,omitempty"`
	Alternative  string      `json:"alternative,omitempty"`
}

type Assets struct {
	PackageJSON []byte
	PackageLock []byte
	Bridge      []byte
}

type Options struct {
	Root       string
	Manifest   Manifest
	Platform   Platform
	HTTPClient *http.Client
	Assets     Assets
	Now        func() time.Time
	Quiesce    func(context.Context) error
}

type Manager struct {
	mu       sync.Mutex
	root     string
	manifest Manifest
	platform Platform
	client   *http.Client
	assets   Assets
	now      func() time.Time
	quiesce  func(context.Context) error
}

var errOperationInProgress = errors.New("another Claude Runtime generation operation is already in progress")

type diskState struct {
	Version         int    `json:"version"`
	Active          string `json:"active,omitempty"`
	Staged          string `json:"staged,omitempty"`
	Previous        string `json:"previous,omitempty"`
	TermsRevision   string `json:"termsRevision,omitempty"`
	TermsAcceptedAt string `json:"termsAcceptedAt,omitempty"`
}

type hello struct {
	ProtocolVersion   int      `json:"protocolVersion"`
	BridgeBuild       string   `json:"bridgeBuild"`
	NodeVersion       string   `json:"nodeVersion"`
	SDKVersion        string   `json:"sdkVersion"`
	ClaudeCodeVersion string   `json:"claudeCodeVersion"`
	Capabilities      []string `json:"capabilities"`
}

type generationMetadata struct {
	Manifest   Manifest `json:"manifest"`
	Verified   hello    `json:"verified"`
	VerifiedAt string   `json:"verifiedAt"`
}

func New(options Options) *Manager {
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if len(options.Assets.Bridge) == 0 {
		options.Assets = currentAssets()
	}
	return &Manager{root: options.Root, manifest: options.Manifest, platform: options.Platform, client: options.HTTPClient, assets: options.Assets, now: options.Now, quiesce: options.Quiesce}
}

func (m *Manager) Status(ctx context.Context) Status {
	state, err := m.loadState()
	if err != nil {
		status := m.status(diskState{})
		if status.State != StateUnsupported {
			status.State, status.Reason = StateBroken, "Claude Runtime generation state is unreadable."
		}
		return status
	}
	status := m.status(state)
	for id, installed := range map[string]*Generation{state.Active: status.Active, state.Staged: status.Staged} {
		if id != "" && installed != nil {
			if _, err := m.verifyGeneration(ctx, id); err != nil {
				status.State, status.Reason = StateBroken, "The installed Claude Runtime generation failed zero-model verification."
				break
			}
		}
	}
	if status.State != StateBroken && state.Previous != "" && status.Previous != nil {
		if _, err := m.verifyGeneration(ctx, state.Previous); err != nil {
			status.Previous = nil
			status.Reason = "The retained previous Claude Runtime generation is damaged; rollback is unavailable."
			status.Alternative = "Install and activate another compatible generation before rollback is needed."
		}
	}
	return status
}

func (m *Manager) Install(ctx context.Context, request InstallRequest) (Status, error) {
	if !m.mu.TryLock() {
		return m.Status(ctx), errOperationInProgress
	}
	defer m.mu.Unlock()
	if !m.platform.Supported {
		return m.status(diskState{}), fmt.Errorf("Claude Runtime generation is unsupported: %s", m.platform.Reason)
	}
	state, err := m.loadState()
	if err != nil {
		return Status{}, err
	}
	termsChanged := state.TermsRevision != m.manifest.TermsRevision
	if termsChanged {
		if !request.AcceptTerms {
			return m.status(state), errors.New("Anthropic terms acknowledgement is required")
		}
		state.TermsRevision = m.manifest.TermsRevision
		state.TermsAcceptedAt = m.now().UTC().Format(time.RFC3339Nano)
	}
	artifact, err := m.artifact()
	if err != nil {
		return m.status(state), err
	}
	if err := verifyManifestAssets(m.manifest, artifact, m.assets); err != nil {
		return m.status(state), err
	}
	destination := m.generationDir(m.manifest.ID)
	if _, err := os.Stat(destination); err == nil {
		if _, verifyErr := m.verifyGeneration(ctx, m.manifest.ID); verifyErr != nil {
			return m.status(state), errors.New("existing immutable Claude Runtime generation is damaged")
		}
		stateChanged := termsChanged
		if state.Active != m.manifest.ID && state.Staged != m.manifest.ID {
			state.Staged = m.manifest.ID
			stateChanged = true
		}
		if stateChanged {
			if err := m.saveState(state); err != nil {
				return m.status(diskState{}), err
			}
		}
		return m.status(state), nil
	} else if !os.IsNotExist(err) {
		return m.status(state), err
	}
	if err := os.MkdirAll(filepath.Join(m.root, "generations"), 0o700); err != nil {
		return m.status(state), err
	}
	stage, err := os.MkdirTemp(m.root, ".install-")
	if err != nil {
		return m.status(state), err
	}
	defer os.RemoveAll(stage)
	archivePath := filepath.Join(stage, "node.tar.gz")
	if err := m.download(ctx, artifact.NodeURL, artifact.NodeSHA256, archivePath); err != nil {
		return m.status(state), err
	}
	if err := extractTarGzip(archivePath, stage); err != nil {
		return m.status(state), fmt.Errorf("extract verified Node archive: %w", err)
	}
	node, npm, err := findNode(stage)
	if err != nil {
		return m.status(state), err
	}
	app := filepath.Join(stage, "app")
	if err := os.MkdirAll(app, 0o700); err != nil {
		return m.status(state), err
	}
	for name, contents := range map[string][]byte{"package.json": m.assets.PackageJSON, "package-lock.json": m.assets.PackageLock, "bridge.mjs": m.assets.Bridge} {
		if err := os.WriteFile(filepath.Join(app, name), contents, 0o600); err != nil {
			return m.status(state), err
		}
	}
	cmd := exec.CommandContext(ctx, node, npm, "ci", "--ignore-scripts", "--omit=dev", "--no-audit", "--no-fund", "--registry=https://registry.npmjs.org")
	cmd.Dir = app
	cmd.Env = installEnvironment(filepath.Dir(node), filepath.Join(m.root, "npm-cache"))
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = output
		return m.status(state), fmt.Errorf("install exact Claude Runtime packages: %w", err)
	}
	if err := verifyPackageVersions(app, m.manifest, artifact); err != nil {
		return m.status(state), err
	}
	verification, err := runSelfTest(ctx, node, filepath.Join(app, "bridge.mjs"), m.manifest)
	if err != nil {
		return m.status(state), err
	}
	if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
		return m.status(state), err
	}
	metadata, _ := json.MarshalIndent(generationMetadata{Manifest: m.manifest, Verified: verification, VerifiedAt: m.now().UTC().Format(time.RFC3339Nano)}, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "generation.json"), append(metadata, '\n'), 0o600); err != nil {
		return m.status(state), err
	}
	if err := makeGenerationReadOnly(stage); err != nil {
		return m.status(state), err
	}
	if err := os.Rename(stage, destination); err != nil {
		return m.status(state), err
	}
	stage = ""
	state.Staged = m.manifest.ID
	if err := m.saveState(state); err != nil {
		removeGeneration(destination)
		return m.status(diskState{}), err
	}
	return m.status(state), nil
}

func (m *Manager) Verify(ctx context.Context, target string) (Status, error) {
	if !m.mu.TryLock() {
		return m.Status(ctx), errOperationInProgress
	}
	defer m.mu.Unlock()
	state, err := m.loadState()
	if err != nil {
		return Status{}, err
	}
	id := state.Active
	switch target {
	case "", "active":
	case "staged":
		id = state.Staged
	default:
		return m.status(state), errors.New("Claude Runtime verification target must be active or staged")
	}
	if id == "" {
		return m.status(state), errors.New("Claude Runtime generation is not installed")
	}
	_, err = m.verifyGeneration(ctx, id)
	return m.status(state), err
}

func (m *Manager) Activate(ctx context.Context) (Status, error) {
	if !m.mu.TryLock() {
		return m.Status(ctx), errOperationInProgress
	}
	defer m.mu.Unlock()
	state, err := m.loadState()
	if err != nil {
		return Status{}, err
	}
	if state.Staged == "" {
		return m.status(state), errors.New("no staged Claude Runtime generation")
	}
	if _, err := m.verifyGeneration(ctx, state.Staged); err != nil {
		return m.status(state), err
	}
	if m.quiesce != nil {
		if err := m.quiesce(ctx); err != nil {
			return m.status(state), err
		}
	}
	state.Previous, state.Active, state.Staged = state.Active, state.Staged, ""
	if err := m.saveState(state); err != nil {
		return m.status(diskState{}), err
	}
	m.prune(state)
	return m.status(state), nil
}

func (m *Manager) Rollback(ctx context.Context) (Status, error) {
	if !m.mu.TryLock() {
		return m.Status(ctx), errOperationInProgress
	}
	defer m.mu.Unlock()
	state, err := m.loadState()
	if err != nil {
		return Status{}, err
	}
	if state.Previous == "" {
		return m.status(state), errors.New("no previous Claude Runtime generation")
	}
	if _, err := m.verifyGeneration(ctx, state.Previous); err != nil {
		return m.status(state), err
	}
	if m.quiesce != nil {
		if err := m.quiesce(ctx); err != nil {
			return m.status(state), err
		}
	}
	state.Active, state.Previous = state.Previous, state.Active
	if err := m.saveState(state); err != nil {
		return m.status(diskState{}), err
	}
	m.prune(state)
	return m.status(state), nil
}

func (m *Manager) Preflight(ctx context.Context) error {
	report := m.InspectPreflight(ctx)
	if report.Availability != "available" {
		return errors.New(report.Reason)
	}
	return nil
}

func (m *Manager) InspectPreflight(ctx context.Context) PreflightReport {
	report := PreflightReport{Availability: "unavailable", Required: generationFromManifest(m.manifest, ""), Alternative: "Install, verify, and activate the required generation in Settings or with loom runtime claude."}
	if !m.platform.Supported {
		report.Availability, report.Reason, report.Alternative = "unsupported", m.platform.Reason, m.platform.Alternative
		return report
	}
	state, err := m.loadState()
	if err != nil {
		report.Reason = "Claude Runtime generation state is unreadable."
		return report
	}
	if state.Active != m.manifest.ID {
		report.Reason = "The exact Claude Runtime generation required by this Loom build is not active."
		return report
	}
	if _, err = m.verifyGeneration(ctx, state.Active); err != nil {
		report.Reason = "The active Claude Runtime generation failed zero-model verification."
		return report
	}
	report.Availability, report.Active, report.Reason, report.Alternative = "available", m.readGeneration(state.Active), "", ""
	return report
}

func (m *Manager) artifact() (PlatformArtifact, error) {
	for _, artifact := range m.manifest.Platforms {
		if artifact.OS == m.platform.OS && artifact.Arch == m.platform.Arch {
			return artifact, nil
		}
	}
	return PlatformArtifact{}, errors.New("this Loom build has no Claude Runtime artifact for the current platform")
}

func (m *Manager) status(state diskState) Status {
	status := Status{State: StateInstallRequired, Required: generationFromManifest(m.manifest, ""), Platform: m.platform, TermsAccepted: state.TermsRevision == m.manifest.TermsRevision, TermsRevision: m.manifest.TermsRevision, TermsURL: m.manifest.TermsURL, TermsAcceptedAt: state.TermsAcceptedAt, DeveloperPreview: true}
	if !m.platform.Supported {
		status.State, status.Reason, status.Alternative = StateUnsupported, m.platform.Reason, m.platform.Alternative
		return status
	}
	status.Active = m.readGeneration(state.Active)
	status.Staged = m.readGeneration(state.Staged)
	status.Previous = m.readGeneration(state.Previous)
	if state.Staged != "" && status.Staged == nil {
		status.State = StateBroken
		status.Reason = "The staged Claude Runtime generation is missing, damaged, or incompatible."
	} else if status.Staged != nil {
		status.State = StateStaged
	} else if status.Active != nil && status.Active.ID == m.manifest.ID {
		status.State = StateActive
	} else if state.Active != "" {
		status.State = StateBroken
		status.Reason = "The active Claude Runtime generation is missing, damaged, or incompatible."
	}
	if state.Previous != "" && status.Previous == nil && status.Reason == "" {
		status.Reason = "The retained previous Claude Runtime generation is missing or damaged; rollback is unavailable."
		status.Alternative = "Install and activate another compatible generation before rollback is needed."
	}
	return status
}

func (m *Manager) readGeneration(id string) *Generation {
	if id == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(m.generationDir(id), "generation.json"))
	if err != nil {
		return nil
	}
	var stored generationMetadata
	if json.Unmarshal(data, &stored) != nil || m.verifyStatic(id, stored.Manifest) != nil {
		return nil
	}
	g := generationFromManifest(stored.Manifest, stored.VerifiedAt)
	g.Capabilities = append([]string(nil), stored.Verified.Capabilities...)
	return &g
}

func (m *Manager) verifyGeneration(ctx context.Context, id string) (hello, error) {
	data, err := os.ReadFile(filepath.Join(m.generationDir(id), "generation.json"))
	if err != nil {
		return hello{}, errors.New("Claude Runtime generation is missing or unreadable")
	}
	var stored generationMetadata
	if json.Unmarshal(data, &stored) != nil || stored.Manifest.ID != id || stored.Manifest.Compatibility != m.manifest.Compatibility {
		return hello{}, errors.New("Claude Runtime generation metadata is incompatible")
	}
	if id == m.manifest.ID && !sameCompatibilityRow(stored.Manifest, m.manifest) {
		return hello{}, errors.New("Claude Runtime generation does not match the exact compatibility row")
	}
	if err := m.verifyStatic(id, stored.Manifest); err != nil {
		return hello{}, err
	}
	node, _, err := findNode(m.generationDir(id))
	if err != nil {
		return hello{}, errors.New("Claude Runtime Node executable is missing")
	}
	return runSelfTest(ctx, node, filepath.Join(m.generationDir(id), "app", "bridge.mjs"), stored.Manifest)
}

func (m *Manager) verifyStatic(id string, manifest Manifest) error {
	for path, want := range map[string]string{
		filepath.Join(m.generationDir(id), "app", "bridge.mjs"):        manifest.BridgeSHA256,
		filepath.Join(m.generationDir(id), "app", "package-lock.json"): manifest.PackageLockSHA256,
	} {
		if want == "" {
			continue
		}
		got, err := fileSHA256(path)
		if err != nil || !strings.EqualFold(got, want) {
			return errors.New("Claude Runtime generation failed integrity verification")
		}
	}
	return nil
}

func (m *Manager) generationDir(id string) string { return filepath.Join(m.root, "generations", id) }

func (m *Manager) loadState() (diskState, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "state.json"))
	if os.IsNotExist(err) {
		return diskState{Version: 1}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	var state diskState
	if err := json.Unmarshal(data, &state); err != nil || state.Version != 1 {
		return diskState{}, errors.New("Claude Runtime generation state is invalid")
	}
	return state, nil
}

func (m *Manager) saveState(state diskState) error {
	state.Version = 1
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.root, ".state-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(m.root, "state.json")); err != nil {
		return err
	}
	directory, err := os.Open(m.root)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (m *Manager) prune(state diskState) {
	keep := map[string]bool{state.Active: true, state.Previous: true, state.Staged: true}
	entries, _ := os.ReadDir(filepath.Join(m.root, "generations"))
	for _, entry := range entries {
		if !keep[entry.Name()] {
			removeGeneration(filepath.Join(m.root, "generations", entry.Name()))
		}
	}
}

func (m *Manager) download(ctx context.Context, url, want, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("download Node runtime: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download Node runtime: HTTP %d", response.StatusCode)
	}
	hash := sha256.New()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxNodeArchive+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxNodeArchive || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), want) {
		return errors.New("downloaded Node runtime failed integrity verification")
	}
	return nil
}

func extractTarGzip(archive, destination string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(header.Name)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("Node archive contains an invalid path")
		}
		path := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o755)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, reader)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
}

func findNode(root string) (string, string, error) {
	nodes, _ := filepath.Glob(filepath.Join(root, "node-*", "bin", "node"))
	if len(nodes) != 1 {
		return "", "", errors.New("verified Node archive has an unexpected layout")
	}
	npm := filepath.Join(filepath.Dir(filepath.Dir(nodes[0])), "lib", "node_modules", "npm", "bin", "npm-cli.js")
	if _, err := os.Stat(npm); err != nil {
		return "", "", errors.New("verified Node archive does not contain npm")
	}
	return nodes[0], npm, nil
}

func verifyPackageVersions(app string, manifest Manifest, artifact PlatformArtifact) error {
	for path, want := range map[string]string{
		filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"):      manifest.SDKVersion,
		filepath.Join(app, "node_modules", filepath.FromSlash(artifact.PackageName), "package.json"): artifact.PackageVersion,
	} {
		data, err := os.ReadFile(path)
		var value struct {
			Version string `json:"version"`
		}
		if err != nil || json.Unmarshal(data, &value) != nil || value.Version != want {
			return errors.New("installed Claude Runtime package versions do not match the compatibility manifest")
		}
	}
	return nil
}

func verifyManifestAssets(manifest Manifest, artifact PlatformArtifact, assets Assets) error {
	if got := fmt.Sprintf("%x", sha256.Sum256(assets.Bridge)); manifest.BridgeSHA256 != "" && !strings.EqualFold(got, manifest.BridgeSHA256) {
		return errors.New("embedded Claude Runtime bridge failed integrity verification")
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(assets.PackageLock)); manifest.PackageLockSHA256 != "" && !strings.EqualFold(got, manifest.PackageLockSHA256) {
		return errors.New("embedded Claude Runtime package lock failed integrity verification")
	}
	var lock struct {
		Packages map[string]struct {
			Version   string `json:"version"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	if json.Unmarshal(assets.PackageLock, &lock) != nil {
		return errors.New("embedded Claude Runtime package lock is invalid")
	}
	for path, expected := range map[string]struct{ version, integrity string }{
		"node_modules/@anthropic-ai/claude-agent-sdk": {manifest.SDKVersion, manifest.SDKIntegrity},
		"node_modules/" + artifact.PackageName:        {artifact.PackageVersion, artifact.PackageIntegrity},
	} {
		entry, ok := lock.Packages[path]
		if !ok || entry.Version != expected.version || expected.integrity != "" && entry.Integrity != expected.integrity {
			return errors.New("embedded Claude Runtime package lock does not match the compatibility manifest")
		}
	}
	return nil
}

func runSelfTest(ctx context.Context, node, bridge string, manifest Manifest) (hello, error) {
	command := exec.CommandContext(ctx, node, bridge, "--self-test")
	command.Env = []string{"PATH=" + filepath.Dir(node), "HOME=" + filepath.Dir(filepath.Dir(bridge))}
	output, err := command.Output()
	if err != nil {
		return hello{}, errors.New("Claude Runtime zero-model self-test failed")
	}
	var got hello
	if json.Unmarshal(bytesTrimSpace(output), &got) != nil || got.ProtocolVersion != manifest.BridgeProtocol || got.BridgeBuild != manifest.BridgeBuild || got.NodeVersion != manifest.NodeVersion || got.SDKVersion != manifest.SDKVersion || got.ClaudeCodeVersion != manifest.ClaudeCodeVersion || !sameStrings(got.Capabilities, manifest.RequiredCapabilities) {
		return hello{}, errors.New("Claude Runtime zero-model self-test did not match the compatibility manifest")
	}
	return got, nil
}

func generationFromManifest(manifest Manifest, verified string) Generation {
	return Generation{ID: manifest.ID, Compatibility: manifest.Compatibility, BridgeProtocol: manifest.BridgeProtocol, BridgeBuild: manifest.BridgeBuild, NodeVersion: manifest.NodeVersion, SDKVersion: manifest.SDKVersion, ClaudeCodeVersion: manifest.ClaudeCodeVersion, Capabilities: append([]string(nil), manifest.RequiredCapabilities...), VerifiedAt: verified}
}

func sameStrings(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

func sameCompatibilityRow(a, b Manifest) bool {
	if a.ID != b.ID || a.Compatibility != b.Compatibility || a.BridgeProtocol != b.BridgeProtocol || a.BridgeBuild != b.BridgeBuild || a.BridgeSHA256 != b.BridgeSHA256 || a.NodeVersion != b.NodeVersion || a.SDKVersion != b.SDKVersion || a.SDKIntegrity != b.SDKIntegrity || a.ClaudeCodeVersion != b.ClaudeCodeVersion || a.PackageLockSHA256 != b.PackageLockSHA256 || !sameStrings(a.RequiredCapabilities, b.RequiredCapabilities) || len(a.Platforms) != len(b.Platforms) {
		return false
	}
	for i := range a.Platforms {
		x, y := a.Platforms[i], b.Platforms[i]
		if x.OS != y.OS || x.Arch != y.Arch || x.NodeSHA256 != y.NodeSHA256 || x.PackageName != y.PackageName || x.PackageVersion != y.PackageVersion || x.PackageIntegrity != y.PackageIntegrity {
			return false
		}
	}
	return true
}

func bytesTrimSpace(value []byte) []byte { return []byte(strings.TrimSpace(string(value))) }

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func installEnvironment(nodeDir, cache string) []string {
	environment := []string{"PATH=" + nodeDir, "HOME=" + filepath.Dir(cache), "npm_config_cache=" + cache}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func makeGenerationReadOnly(root string) error {
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if info.Mode()&0o111 != 0 {
			mode = 0o555
		}
		return os.Chmod(path, mode)
	}); err != nil {
		return err
	}
	return nil
}

func removeGeneration(path string) {
	_ = filepath.WalkDir(path, func(current string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(current, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(path)
}

func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, safe := range []string{"terms acknowledgement", "unsupported", "no staged", "no previous", "not installed", "already in progress", "integrity verification", "zero-model self-test", "immutable Claude Runtime generation", "compatibility row", "verification target"} {
		if strings.Contains(message, safe) {
			return message
		}
	}
	return "Claude Runtime generation operation failed."
}
