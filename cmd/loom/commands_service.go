package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const codexLoomLaunchAgentLabel = "com.pinix.codex-loom"

type serviceNoProxy struct {
	Upper  string
	Lower  string
	Source string
}

type launchAgentConfig struct {
	Label      string
	Executable string
	Cwd        string
	LogPath    string
	Path       string
	NoProxy    serviceNoProxy
}

type launchAgentInstall struct {
	Home   string
	UID    string
	Config launchAgentConfig
	Run    func(string, ...string) error
}

type launchAgentInstallResult struct {
	UnitPath string
}

type launchAgentDiagnostic struct {
	State   string
	Message string
}

func cmdService(a args) {
	if len(a.positional) != 1 || a.positional[0] != "install" {
		usage("service install [--no-proxy HOSTS] [--path PATH] [--cwd DIR] [--exe PATH]")
	}
	if runtime.GOOS != "darwin" {
		fail(fmt.Errorf("automatic service installation is currently supported only on macOS"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fail(err)
	}
	executable, err := resolveServiceExecutable(a.flags["exe"])
	if err != nil {
		fail(err)
	}
	cwd := strings.TrimSpace(a.flags["cwd"])
	if cwd == "" {
		cwd = filepath.Dir(executable)
	}
	pathValue := strings.TrimSpace(a.flags["path"])
	if pathValue == "" {
		pathValue = strings.TrimSpace(os.Getenv("PATH"))
	}
	if pathValue == "" {
		pathValue = "/usr/local/bin:/usr/bin:/bin"
	}
	noProxy := resolveServiceNoProxy(a.flags["no-proxy"])
	result, err := installLaunchAgent(launchAgentInstall{
		Home: home,
		UID:  fmt.Sprint(os.Getuid()),
		Config: launchAgentConfig{
			Label:      codexLoomLaunchAgentLabel,
			Executable: executable,
			Cwd:        cwd,
			LogPath:    "/tmp/codex-loom.log",
			Path:       pathValue,
			NoProxy:    noProxy,
		},
		Run: runServiceCommand,
	})
	if err != nil {
		fail(err)
	}
	fmt.Printf("installed %s\n", result.UnitPath)
	if noProxy.Upper == "" && noProxy.Lower == "" {
		fmt.Println("proxy bypass: not configured")
	} else {
		fmt.Printf("proxy bypass: %s (written as NO_PROXY and no_proxy)\n", noProxy.Source)
	}
}

func resolveServiceExecutable(explicit string) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return validateServiceExecutable(explicit)
	}
	current, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve loom executable: %w", err)
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return "", fmt.Errorf("resolve loom executable symlinks: %w", err)
	}
	return validateServiceExecutable(filepath.Join(filepath.Dir(current), "codex-loom"))
}

func validateServiceExecutable(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect CodexLoom executable %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("CodexLoom executable is not an executable regular file: %s", path)
	}
	return path, nil
}

func resolveServiceNoProxy(explicit string) serviceNoProxy {
	if value := strings.TrimSpace(explicit); value != "" {
		return serviceNoProxy{Upper: value, Lower: value, Source: "flag"}
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_LOOM_NO_PROXY")); value != "" {
		return serviceNoProxy{Upper: value, Lower: value, Source: "CODEX_LOOM_NO_PROXY"}
	}
	upper := strings.TrimSpace(os.Getenv("NO_PROXY"))
	lower := strings.TrimSpace(os.Getenv("no_proxy"))
	switch {
	case upper != "" && lower != "":
		return serviceNoProxy{Upper: upper, Lower: lower, Source: "environment"}
	case upper != "":
		return serviceNoProxy{Upper: upper, Lower: upper, Source: "NO_PROXY"}
	case lower != "":
		return serviceNoProxy{Upper: lower, Lower: lower, Source: "no_proxy"}
	default:
		return serviceNoProxy{}
	}
}

func buildLaunchAgentPlist(config launchAgentConfig) ([]byte, error) {
	for label, value := range map[string]string{
		"label": config.Label, "executable": config.Executable, "working directory": config.Cwd,
		"log path": config.LogPath, "PATH": config.Path,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s is required", label)
		}
	}
	escape := func(value string) string {
		var out bytes.Buffer
		_ = xml.EscapeText(&out, []byte(value))
		return out.String()
	}
	var environment strings.Builder
	writeEnvironment := func(key, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&environment, "    <key>%s</key>\n    <string>%s</string>\n", escape(key), escape(value))
	}
	writeEnvironment("PATH", config.Path)
	writeEnvironment("NO_PROXY", config.NoProxy.Upper)
	writeEnvironment("no_proxy", config.NoProxy.Lower)
	payload := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
  </array>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
%s  </dict>
</dict>
</plist>
`, escape(config.Label), escape(config.Executable), escape(config.Cwd), escape(config.LogPath), escape(config.LogPath), environment.String())
	return []byte(payload), nil
}

func installLaunchAgent(install launchAgentInstall) (launchAgentInstallResult, error) {
	if install.Home == "" || install.UID == "" || install.Run == nil {
		return launchAgentInstallResult{}, errors.New("home, uid, and command runner are required")
	}
	payload, err := buildLaunchAgentPlist(install.Config)
	if err != nil {
		return launchAgentInstallResult{}, err
	}
	unitDir := filepath.Join(install.Home, "Library", "LaunchAgents")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		return launchAgentInstallResult{}, fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	unitPath := filepath.Join(unitDir, install.Config.Label+".plist")
	previous, readErr := os.ReadFile(unitPath)
	hadPrevious := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return launchAgentInstallResult{}, fmt.Errorf("read existing LaunchAgent: %w", readErr)
	}
	if err := writeOwnerFileAtomic(unitPath, payload); err != nil {
		return launchAgentInstallResult{}, fmt.Errorf("write LaunchAgent: %w", err)
	}
	domain := "gui/" + install.UID
	target := domain + "/" + install.Config.Label
	rollback := func() error {
		_ = install.Run("launchctl", "bootout", target)
		if !hadPrevious {
			return os.Remove(unitPath)
		}
		if err := writeOwnerFileAtomic(unitPath, previous); err != nil {
			return fmt.Errorf("restore previous LaunchAgent: %w", err)
		}
		if err := install.Run("launchctl", "bootstrap", domain, unitPath); err != nil {
			return fmt.Errorf("reload previous LaunchAgent: %w", err)
		}
		if err := install.Run("launchctl", "kickstart", "-k", target); err != nil {
			return fmt.Errorf("restart previous LaunchAgent: %w", err)
		}
		return nil
	}
	_ = install.Run("launchctl", "bootout", target)
	if err := install.Run("launchctl", "bootstrap", domain, unitPath); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return launchAgentInstallResult{}, fmt.Errorf("launchctl bootstrap: %w (rollback failed: %v)", err, rollbackErr)
		}
		return launchAgentInstallResult{}, fmt.Errorf("launchctl bootstrap: %w (previous service restored)", err)
	}
	if err := install.Run("launchctl", "kickstart", "-k", target); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return launchAgentInstallResult{}, fmt.Errorf("launchctl kickstart: %w (rollback failed: %v)", err, rollbackErr)
		}
		return launchAgentInstallResult{}, fmt.Errorf("launchctl kickstart: %w (previous service restored)", err)
	}
	return launchAgentInstallResult{UnitPath: unitPath}, nil
}

func writeOwnerFileAtomic(path string, payload []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func runServiceCommand(name string, arguments ...string) error {
	output, err := exec.Command(name, arguments...).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
	}
	return err
}

func launchAgentNoProxyDiagnostic(unitPath string, terminal serviceNoProxy) launchAgentDiagnostic {
	payload, err := os.ReadFile(unitPath)
	if os.IsNotExist(err) {
		return launchAgentDiagnostic{State: "missing", Message: "LaunchAgent is not installed; run `loom service install`"}
	}
	if err != nil {
		return launchAgentDiagnostic{State: "error", Message: fmt.Sprintf("cannot read LaunchAgent: %v", err)}
	}
	environment, err := parsePlistEnvironment(payload)
	if err != nil {
		return launchAgentDiagnostic{State: "error", Message: fmt.Sprintf("cannot inspect LaunchAgent: %v", err)}
	}
	service := serviceNoProxy{Upper: environment["NO_PROXY"], Lower: environment["no_proxy"]}
	if service.Upper == terminal.Upper && service.Lower == terminal.Lower {
		return launchAgentDiagnostic{State: "ok", Message: "LaunchAgent proxy bypass matches this terminal"}
	}
	return launchAgentDiagnostic{
		State:   "mismatch",
		Message: "LaunchAgent NO_PROXY/no_proxy differs from this terminal; run `loom service install` to refresh it",
	}
}

func parsePlistEnvironment(payload []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return nil, err
		}
		if key != "EnvironmentVariables" {
			continue
		}
		for {
			token, err = decoder.Token()
			if err != nil {
				return nil, err
			}
			if start, ok = token.(xml.StartElement); ok && start.Name.Local == "dict" {
				return decodeStringDict(decoder)
			}
		}
	}
}

func decodeStringDict(decoder *xml.Decoder) (map[string]string, error) {
	values := map[string]string{}
	currentKey := ""
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch typed := token.(type) {
		case xml.EndElement:
			if typed.Name.Local == "dict" {
				return values, nil
			}
		case xml.StartElement:
			switch typed.Name.Local {
			case "key":
				if err := decoder.DecodeElement(&currentKey, &typed); err != nil {
					return nil, err
				}
			case "string":
				var value string
				if err := decoder.DecodeElement(&value, &typed); err != nil {
					return nil, err
				}
				if currentKey != "" {
					values[currentKey] = value
					currentKey = ""
				}
			}
		}
	}
}
