package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yan5xu/codex-loom/internal/buildinfo"
)

func cmdRuntime(a args) {
	if len(a.positional) == 0 || a.positional[0] != "claude" {
		usage("runtime claude status|install|verify|activate|rollback")
	}
	a.positional = a.positional[1:]
	cmdRuntimeGeneration(a)
}

func cmdRuntimeGeneration(a args) {
	action := "status"
	if len(a.positional) > 0 {
		action = a.positional[0]
	}
	method, path, body := http.MethodGet, "/api/runtime-generations/claude", any(nil)
	switch action {
	case "status":
	case "install":
		method, path = http.MethodPost, path+"/install"
		body = map[string]any{"acceptTerms": a.flags["accept-terms"] == "true"}
	case "verify":
		method, path = http.MethodPost, path+"/verify"
		body = map[string]any{"target": a.flags["target"]}
	case "activate", "rollback":
		method, path = http.MethodPost, path+"/"+action
		body = map[string]any{}
	default:
		usage("runtime claude status|install|verify|activate|rollback [--accept-terms] [--target staged]")
	}
	response, err := api(method, path, body)
	if err != nil {
		fail(err)
	}
	generation, _ := response["generation"].(map[string]any)
	fmt.Print(formatRuntimeGeneration(generation))
}

func formatRuntimeGeneration(generation map[string]any) string {
	required, _ := generation["required"].(map[string]any)
	platform, _ := generation["platform"].(map[string]any)
	var text strings.Builder
	fmt.Fprintf(&text, "Claude Runtime generation · %s · developer preview\n", value(generation, "state", "unknown"))
	fmt.Fprintf(&text, "required: %s · Node %s · SDK %s · Claude Code %s\n", value(required, "id", "unknown"), value(required, "nodeVersion", "unknown"), value(required, "sdkVersion", "unknown"), value(required, "claudeCodeVersion", "unknown"))
	fmt.Fprintf(&text, "platform: %s/%s · supported %t\n", value(platform, "os", "unknown"), value(platform, "arch", "unknown"), boolean(platform, "supported"))
	for _, key := range []string{"reason", "alternative"} {
		if message := value(generation, key, ""); message != "" {
			fmt.Fprintf(&text, "%s: %s\n", key, message)
		}
	}
	return text.String()
}

func cmdVersion(a args) {
	if a.flags["running"] != "" {
		response, err := api("GET", "/api/version", nil)
		if err != nil {
			fail(err)
		}
		build, _ := response["build"].(map[string]any)
		fmt.Print(formatBuild("running", build))
		return
	}
	info := buildinfo.Current(nil, buildinfo.Runtime{})
	fmt.Print(formatBuild("cli", buildMap(info)))
}

func cmdDoctor(a args) {
	if len(a.positional) > 0 {
		usage("doctor")
	}
	versionResponse, err := api("GET", "/api/version", nil)
	if err != nil {
		fail(err)
	}
	health, err := api("GET", "/api/health", nil)
	if err != nil {
		fail(err)
	}
	running, _ := versionResponse["build"].(map[string]any)
	local := buildMap(buildinfo.Current(nil, buildinfo.Runtime{}))
	providerResponse, providerErr := api("GET", "/api/model-providers", nil)

	fmt.Printf("CodexLoom doctor\n")
	fmt.Printf("endpoint: %s\n", base)
	fmt.Print(formatBuild("running", running))
	fmt.Printf("health: ok · %.0f agents\n", num(health, "agents"))
	if providerErr != nil {
		fmt.Printf("catalog: %s\n", yellow("unavailable: "+providerErr.Error()))
	} else if catalog, ok := providerResponse["catalog"].(map[string]any); ok {
		compatibility := value(catalog, "compatibility", "unverified")
		status := compatibility
		if boolean(catalog, "restartRequired") {
			status = "restart required"
		}
		line := fmt.Sprintf("%s · Codex %s · baseline %s · %.0f models", value(catalog, "version", "unknown"), value(catalog, "codexVersion", "unknown"), value(catalog, "codexBaseline", "unknown"), num(catalog, "modelCount"))
		if compatibility == "verified" && !boolean(catalog, "restartRequired") {
			fmt.Printf("catalog: %s · %s\n", line, green(status))
		} else {
			fmt.Printf("catalog: %s · %s\n", line, yellow(status))
		}
	}
	if mismatch := buildMismatch(local, running); mismatch != "" {
		fmt.Printf("status: %s\n", yellow(mismatch))
	} else {
		fmt.Printf("status: %s\n", green("CLI and running service match"))
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err != nil {
			fmt.Printf("service proxy bypass: %s\n", yellow("cannot resolve LaunchAgent path: "+err.Error()))
		} else {
			unitPath := filepath.Join(home, "Library", "LaunchAgents", codexLoomLaunchAgentLabel+".plist")
			diagnostic := launchAgentNoProxyDiagnostic(unitPath, resolveServiceNoProxy(""))
			if diagnostic.State == "ok" {
				fmt.Printf("service proxy bypass: %s\n", green(diagnostic.Message))
			} else {
				fmt.Printf("service proxy bypass: %s\n", yellow(diagnostic.Message))
			}
		}
	}
}

func formatBuild(label string, build map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s %s (%s)\n", label, value(build, "product", "CodexLoom"), value(build, "version", "dev"), value(build, "commit", "unknown"))
	fmt.Fprintf(&b, "  built: %s · go %s · %s/%s\n", value(build, "builtAt", "unknown"), value(build, "goVersion", runtime.Version()), value(build, "os", runtime.GOOS), value(build, "arch", runtime.GOARCH))
	if label == "running" {
		fmt.Fprintf(&b, "  process: pid %.0f · started %s · mode %s · read-only %t\n", buildNumber(build, "pid"), value(build, "startedAt", "unknown"), value(build, "mode", "normal"), boolean(build, "readOnly"))
		fmt.Fprintf(&b, "  data: %s\n", value(build, "dataDir", "unknown"))
		fmt.Fprintf(&b, "  web: %s\n", value(build, "webAsset", "unknown"))
	}
	return b.String()
}

func buildMap(info buildinfo.Info) map[string]any {
	data, _ := json.Marshal(info)
	result := map[string]any{}
	_ = json.Unmarshal(data, &result)
	return result
}

func buildMismatch(local, running map[string]any) string {
	localCommit := value(local, "commit", "unknown")
	runningCommit := value(running, "commit", "unknown")
	if localCommit != "unknown" && runningCommit != "unknown" && localCommit != runningCommit {
		return fmt.Sprintf("restart required: CLI commit %s, running commit %s", localCommit, runningCommit)
	}
	localVersion := value(local, "version", "dev")
	runningVersion := value(running, "version", "dev")
	if localVersion != runningVersion {
		return fmt.Sprintf("version mismatch: CLI %s, running %s", localVersion, runningVersion)
	}
	return ""
}

func value(record map[string]any, key, fallback string) string {
	if text, ok := record[key].(string); ok && text != "" {
		return text
	}
	return fallback
}

func buildNumber(record map[string]any, key string) float64 {
	value, _ := record[key].(float64)
	return value
}

func boolean(record map[string]any, key string) bool {
	value, _ := record[key].(bool)
	return value
}
