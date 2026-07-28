package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func cmdContext(a args) {
	if len(a.positional) < 1 {
		usage("context prompt|get|explain|coverage ...")
	}
	switch a.positional[0] {
	case "prompt":
		cmdContextPrompt(a)
	case "explain":
		if len(a.positional) < 2 {
			usage("context explain <agent> [--json]")
		}
		resp, err := api("GET", "/api/agents/"+url.PathEscape(a.positional[1])+"/context/explain", nil)
		if err != nil {
			fail(err)
		}
		printContextValue(resp["context"], a.flags["json"] == "true")
	case "coverage":
		if len(a.positional) < 2 {
			usage("context coverage <agent> [--json]")
		}
		resp, err := api("GET", "/api/agents/"+url.PathEscape(a.positional[1])+"/context/coverage", nil)
		if err != nil {
			fail(err)
		}
		printContextValue(resp["coverage"], true)
	default:
		usage("context prompt|explain|coverage ...")
	}
}

func cmdContextPrompt(a args) {
	if len(a.positional) < 2 {
		usage("context prompt get|set|clear ...")
	}
	switch a.positional[1] {
	case "get":
		resp, err := api("GET", "/api/context/agent-prompt", nil)
		if err != nil {
			fail(err)
		}
		printContextPrompt(resp["prompt"], a.flags["json"] == "true")
	case "set":
		content := strings.TrimSpace(strings.Join(a.positional[2:], " "))
		if path := strings.TrimSpace(a.flags["file"]); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				fail(err)
			}
			content = strings.TrimSpace(string(data))
		}
		if content == "" {
			usage("context prompt set [TEXT|--file PATH] [--expected-version N]")
		}
		payload := map[string]any{"content": content}
		if expected := strings.TrimSpace(a.flags["expected-version"]); expected != "" {
			version, err := strconv.Atoi(expected)
			if err != nil || version < 0 {
				fail(fmt.Errorf("--expected-version must be a non-negative integer"))
			}
			payload["expectedVersion"] = version
		}
		resp, err := api("PUT", "/api/context/agent-prompt", payload)
		if err != nil {
			fail(err)
		}
		printContextPrompt(resp["prompt"], a.flags["json"] == "true")
	case "clear":
		path := "/api/context/agent-prompt"
		if expected := strings.TrimSpace(a.flags["expected-version"]); expected != "" {
			version, err := strconv.Atoi(expected)
			if err != nil || version < 0 {
				fail(fmt.Errorf("--expected-version must be a non-negative integer"))
			}
			path += "?expectedVersion=" + strconv.Itoa(version)
		}
		resp, err := api("DELETE", path, nil)
		if err != nil {
			fail(err)
		}
		printContextPrompt(resp["prompt"], a.flags["json"] == "true")
	default:
		usage("context prompt get|set|clear ...")
	}
}

func printContextPrompt(value any, asJSON bool) {
	prompt, _ := value.(map[string]any)
	if asJSON {
		printContextValue(prompt, true)
		return
	}
	fmt.Printf("Loom Agent Prompt v%.0f [%s]\n%s\n", num(prompt, "version"), str(prompt, "source"), str(prompt, "content"))
}

func printContextValue(value any, asJSON bool) {
	if asJSON {
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			fail(err)
		}
		fmt.Println(string(encoded))
		return
	}
	context, _ := value.(map[string]any)
	epoch, _ := context["epoch"].(map[string]any)
	fmt.Printf("%s · epoch %s\n", bold(str(context, "agentName")), str(epoch, "id"))
	for _, value := range anySlice(context["sources"]) {
		source, _ := value.(map[string]any)
		state := yellow("missing")
		if covered, _ := source["covered"].(bool); covered {
			state = green("covered")
		}
		fmt.Printf("  %-28s %-12s %s\n", str(source, "key"), state, str(source, "revision"))
	}
	if limitation := str(context, "limitation"); limitation != "" {
		fmt.Printf("\n%s\n", dim(limitation))
	}
}
