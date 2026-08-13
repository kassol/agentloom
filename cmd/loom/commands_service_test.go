package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveServiceNoProxyUsesExplicitOverrideAndMirrorsCase(t *testing.T) {
	t.Setenv("CODEX_LOOM_NO_PROXY", "dedicated.internal")
	t.Setenv("NO_PROXY", "upper.internal")
	t.Setenv("no_proxy", "lower.internal")

	got := resolveServiceNoProxy("flag.internal")
	want := serviceNoProxy{Upper: "flag.internal", Lower: "flag.internal", Source: "flag"}
	if got != want {
		t.Fatalf("resolveServiceNoProxy() = %#v, want %#v", got, want)
	}

	got = resolveServiceNoProxy("")
	want = serviceNoProxy{Upper: "dedicated.internal", Lower: "dedicated.internal", Source: "CODEX_LOOM_NO_PROXY"}
	if got != want {
		t.Fatalf("dedicated resolveServiceNoProxy() = %#v, want %#v", got, want)
	}
}

func TestResolveServiceNoProxyPreservesCaseSpecificTerminalValues(t *testing.T) {
	t.Setenv("CODEX_LOOM_NO_PROXY", "")
	t.Setenv("NO_PROXY", "upper.internal")
	t.Setenv("no_proxy", "lower.internal")
	got := resolveServiceNoProxy("")
	want := serviceNoProxy{Upper: "upper.internal", Lower: "lower.internal", Source: "environment"}
	if got != want {
		t.Fatalf("resolveServiceNoProxy() = %#v, want %#v", got, want)
	}

	t.Setenv("no_proxy", "")
	got = resolveServiceNoProxy("")
	want = serviceNoProxy{Upper: "upper.internal", Lower: "upper.internal", Source: "NO_PROXY"}
	if got != want {
		t.Fatalf("single-case resolveServiceNoProxy() = %#v, want %#v", got, want)
	}
}

func TestBuildLaunchAgentPlistCarriesProxyEnvironmentWithoutLeakingXML(t *testing.T) {
	payload, err := buildLaunchAgentPlist(launchAgentConfig{
		Label:      "com.pinix.codex-loom",
		Executable: "/tmp/Codex & Loom/bin/codex-loom",
		Cwd:        "/tmp/project <main>",
		LogPath:    "/tmp/codex-loom.log",
		Path:       "/opt/bin:/usr/bin:/bin",
		NoProxy:    serviceNoProxy{Upper: "api.internal,10.0.0.0/8&local", Lower: "localhost,<local>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, fragment := range []string{
		"<string>/tmp/Codex &amp; Loom/bin/codex-loom</string>",
		"<string>/tmp/project &lt;main&gt;</string>",
		"<key>NO_PROXY</key>",
		"<string>api.internal,10.0.0.0/8&amp;local</string>",
		"<key>no_proxy</key>",
		"<string>localhost,&lt;local&gt;</string>",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("plist missing %q:\n%s", fragment, text)
		}
	}
}

func TestInstallLaunchAgentWritesUnitBeforeReloadingService(t *testing.T) {
	home := t.TempDir()
	var commands [][]string
	run := func(name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}
	result, err := installLaunchAgent(launchAgentInstall{
		Home: home,
		UID:  "501",
		Config: launchAgentConfig{
			Label:      "com.pinix.codex-loom",
			Executable: "/opt/codexloom/codex-loom",
			Cwd:        "/opt/codexloom",
			LogPath:    "/tmp/codex-loom.log",
			Path:       "/usr/local/bin:/usr/bin:/bin",
			NoProxy:    serviceNoProxy{Upper: "provider.internal", Lower: "provider.internal"},
		},
		Run: run,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(home, "Library", "LaunchAgents", "com.pinix.codex-loom.plist")
	if result.UnitPath != wantPath {
		t.Fatalf("unit path = %q, want %q", result.UnitPath, wantPath)
	}
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unit mode = %o, want 600", info.Mode().Perm())
	}
	wantCommands := [][]string{
		{"launchctl", "bootout", "gui/501/com.pinix.codex-loom"},
		{"launchctl", "bootstrap", "gui/501", wantPath},
		{"launchctl", "kickstart", "-k", "gui/501/com.pinix.codex-loom"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", commands, wantCommands)
	}
}

func TestInstallLaunchAgentRestoresPreviousServiceWhenReloadFails(t *testing.T) {
	home := t.TempDir()
	unitPath := filepath.Join(home, "Library", "LaunchAgents", "com.pinix.codex-loom.plist")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := []byte("previous plist")
	if err := os.WriteFile(unitPath, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	bootstrapCalls := 0
	run := func(name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		if name == "launchctl" && len(args) > 0 && args[0] == "bootstrap" {
			bootstrapCalls++
			if bootstrapCalls == 1 {
				return errors.New("invalid replacement")
			}
		}
		return nil
	}
	_, err := installLaunchAgent(launchAgentInstall{
		Home: home,
		UID:  "501",
		Config: launchAgentConfig{
			Label: "com.pinix.codex-loom", Executable: "/opt/codexloom/codex-loom", Cwd: "/opt/codexloom",
			LogPath: "/tmp/codex-loom.log", Path: "/usr/bin:/bin",
			NoProxy: serviceNoProxy{Upper: "provider.internal", Lower: "provider.internal"},
		},
		Run: run,
	})
	if err == nil || !strings.Contains(err.Error(), "previous service restored") {
		t.Fatalf("install error = %v, want restored-service failure", err)
	}
	restored, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(restored) != string(previous) {
		t.Fatalf("restored unit = %q, want %q", restored, previous)
	}
	wantCommands := [][]string{
		{"launchctl", "bootout", "gui/501/com.pinix.codex-loom"},
		{"launchctl", "bootstrap", "gui/501", unitPath},
		{"launchctl", "bootout", "gui/501/com.pinix.codex-loom"},
		{"launchctl", "bootstrap", "gui/501", unitPath},
		{"launchctl", "kickstart", "-k", "gui/501/com.pinix.codex-loom"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", commands, wantCommands)
	}
}

func TestLaunchAgentNoProxyDiagnosticDetectsTerminalMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.plist")
	payload, err := buildLaunchAgentPlist(launchAgentConfig{
		Label: "com.pinix.codex-loom", Executable: "/tmp/codex-loom", Cwd: "/tmp",
		LogPath: "/tmp/codex-loom.log", Path: "/usr/bin:/bin",
		NoProxy: serviceNoProxy{Upper: "service.internal", Lower: "service.internal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	got := launchAgentNoProxyDiagnostic(path, serviceNoProxy{Upper: "terminal.internal", Lower: "terminal.internal"})
	if got.State != "mismatch" || !strings.Contains(got.Message, "loom service install") {
		t.Fatalf("diagnostic = %#v", got)
	}
}
