package claudegen

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func CurrentManifest() Manifest {
	assets := currentAssets()
	bridgeHash := sha256.Sum256(assets.Bridge)
	lockHash := sha256.Sum256(assets.PackageLock)
	const nodeVersion = "24.19.0"
	return Manifest{
		ID:                   "claude-runtime-v2-node24.19.0-sdk0.3.228",
		Compatibility:        "claude-runtime-v1",
		BridgeProtocol:       1,
		BridgeBuild:          "claude-bridge-v1",
		BridgeSHA256:         fmt.Sprintf("%x", bridgeHash),
		NodeVersion:          nodeVersion,
		SDKVersion:           "0.3.228",
		SDKIntegrity:         "sha512-OOaME54VCoBLjKMqWqFmHkZGyL/x/FHUA0snhyolmyEhVoeBM0Ub5mrnV2Gx3d5/RcVlk2BnEVvPqu0SpZ9VFw==",
		ClaudeCodeVersion:    "2.1.228",
		PackageLockSHA256:    fmt.Sprintf("%x", lockHash),
		TermsRevision:        "anthropic-commercial-terms-2025-06-17",
		TermsURL:             "https://www.anthropic.com/legal/commercial-terms",
		RequiredCapabilities: []string{"interrupt", "approval", "hooks", "mcp", "session_resume"},
		Platforms: []PlatformArtifact{
			{OS: "darwin", Arch: "arm64", NodeURL: "https://nodejs.org/dist/v24.19.0/node-v24.19.0-darwin-arm64.tar.gz", NodeSHA256: "8294b7aa9b03997481c06babf1e8b270c859358f27da57a11509afe537ac381d", PackageName: "@anthropic-ai/claude-agent-sdk-darwin-arm64", PackageVersion: "0.3.228", PackageIntegrity: "sha512-HuCsV3/5XuYYaWuCbksX+e0JkDDUG/AlFJ8wKhDL3PBW/3hHNd6xBYx88kEWk1Z6B1GLxwHht9624lcmscpsyw=="},
			{OS: "darwin", Arch: "x64", NodeURL: "https://nodejs.org/dist/v24.19.0/node-v24.19.0-darwin-x64.tar.gz", NodeSHA256: "d1b5e999db158c62fe8f7267a4476b035d8bd93b1a605bac24a3f0dd166e3316", PackageName: "@anthropic-ai/claude-agent-sdk-darwin-x64", PackageVersion: "0.3.228", PackageIntegrity: "sha512-jSUYY5Nd3efvbLZPU+i0tRBaFXskHu8M+4LMGBEw6A0PaklZ3YfGvKlTOWtJGRw6vMc6LzfOFts024xPNm6OrQ=="},
			{OS: "linux", Arch: "arm64", NodeURL: "https://nodejs.org/dist/v24.19.0/node-v24.19.0-linux-arm64.tar.gz", NodeSHA256: "d28c8a5bf0a808f0ed434a1dce8c54ae98f0371c0bd86ac58abc613f73e6643f", PackageName: "@anthropic-ai/claude-agent-sdk-linux-arm64", PackageVersion: "0.3.228", PackageIntegrity: "sha512-0Wjv6TiWwGlBZINAmNJX07jN359jKwB/4Sr/uWgQkdjuVIOhe/M8ydk7JL2EPqCsbiW1lc15NjE5MWpZiYqooA=="},
			{OS: "linux", Arch: "x64", NodeURL: "https://nodejs.org/dist/v24.19.0/node-v24.19.0-linux-x64.tar.gz", NodeSHA256: "f625d97cd707df4ff96254916fbc5ff014f09c09effe5a1e0ca8f6d41a8789d4", PackageName: "@anthropic-ai/claude-agent-sdk-linux-x64", PackageVersion: "0.3.228", PackageIntegrity: "sha512-LmGplObceqMOu5mlrlhTZL/VSrEWdZagF0Bl8awglMu6WeQcNe7StORYkCznZ0BuzV4CwuC3ipV4q8Jrs66wSg=="},
		},
	}
}

func Default() *Manager {
	return New(Options{Root: DefaultRoot(), Manifest: CurrentManifest(), Platform: DetectPlatform()})
}

func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex-loom-claude-runtime"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "CodexLoom", "claude-runtime")
	}
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "codex-loom", "claude-runtime")
	}
	return filepath.Join(home, ".local", "share", "codex-loom", "claude-runtime")
}

func DetectPlatform() Platform {
	arch := runtime.GOARCH
	if runtime.GOOS == "darwin" {
		version, _ := exec.Command("sw_vers", "-productVersion").Output()
		return ClassifyPlatform("darwin", arch, "macos", strings.TrimSpace(string(version)), "")
	}
	if runtime.GOOS != "linux" {
		return ClassifyPlatform(runtime.GOOS, arch, runtime.GOOS, "", "")
	}
	values := readOSRelease("/etc/os-release")
	libc := ""
	if output, err := exec.Command("ldd", "--version").CombinedOutput(); err == nil && strings.Contains(strings.ToLower(string(output)), "glibc") {
		libc = "glibc"
	}
	return ClassifyPlatform("linux", arch, strings.ToLower(values["ID"]), values["VERSION_ID"], libc)
}

func ClassifyPlatform(goos, goarch, distribution, version, libc string) Platform {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	platform := Platform{OS: goos, Arch: arch, Distribution: distribution, Version: version, Libc: libc}
	platform.Alternative = "Use macOS 14+ or Ubuntu 22.04+ with glibc on arm64/x64."
	switch {
	case arch == "":
		platform.Reason = "This CPU architecture is not supported by the Claude Runtime developer preview."
	case goos == "darwin" && versionAtLeast(version, 14, 0):
		platform.Supported = true
	case goos == "darwin":
		platform.Reason = "macOS 14 or newer is required by the Claude Runtime developer preview."
	case goos == "linux" && distribution == "ubuntu" && libc == "glibc" && versionAtLeast(version, 22, 4):
		platform.Supported = true
	case goos == "linux" && libc != "glibc":
		platform.Reason = "musl and other non-glibc Linux systems are not supported by the Claude Runtime developer preview."
	case goos == "linux":
		platform.Reason = "Only Ubuntu 22.04 or newer with glibc is supported by the Claude Runtime developer preview."
	case goos == "windows":
		platform.Reason = "Windows is not supported by the Claude Runtime developer preview."
	default:
		platform.Reason = "This operating system is not supported by the Claude Runtime developer preview."
	}
	if platform.Supported {
		platform.Alternative = ""
	}
	return platform
}

func versionAtLeast(value string, major, minor int) bool {
	parts := strings.SplitN(value, ".", 3)
	if len(parts) < 1 {
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor := 0
	if len(parts) > 1 {
		gotMinor, _ = strconv.Atoi(parts[1])
	}
	return gotMajor > major || gotMajor == major && gotMinor >= minor
}

func readOSRelease(path string) map[string]string {
	values := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.Trim(value, `"'`)
		}
	}
	return values
}
