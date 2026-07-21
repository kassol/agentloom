package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func cmdTopic(a args) {
	if len(a.positional) == 0 {
		usage("topic create|list|get|send|update|link|participant|intervene|read|resolve|archive ...")
	}
	switch a.positional[0] {
	case "create":
		cmdTopicCreate(a)
	case "list":
		query := url.Values{}
		if value := strings.TrimSpace(a.flags["status"]); value != "" {
			query.Set("status", value)
		}
		if value := strings.TrimSpace(a.flags["agent"]); value != "" {
			query.Set("agent", value)
		}
		path := "/api/topics"
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		resp, err := api("GET", path, nil)
		if err != nil {
			fail(err)
		}
		values := anySlice(resp["topics"])
		if len(values) == 0 {
			fmt.Println("no Topics")
			return
		}
		for _, value := range values {
			topic, _ := value.(map[string]any)
			printTopicLine(topic)
		}
	case "get":
		printTopicDetail(getTopicArgument(a))
	case "send":
		cmdTopicSend(a)
	case "update", "resolve", "archive":
		cmdTopicUpdate(a)
	case "link":
		cmdTopicLink(a)
	case "participant":
		cmdTopicParticipant(a)
	case "intervene":
		cmdTopicIntervene(a)
	case "read":
		if len(a.positional) < 2 {
			usage("topic read <topic-id>")
		}
		resp, err := api("POST", "/api/topics/"+url.PathEscape(a.positional[1])+"/read", map[string]any{})
		if err != nil {
			fail(err)
		}
		topic, _ := resp["topic"].(map[string]any)
		printTopicLine(topic)
	default:
		usage("topic create|list|get|send|update|link|participant|intervene|read|resolve|archive ...")
	}
}

func cmdTopicCreate(a args) {
	title := strings.TrimSpace(a.flags["title"])
	responsible := strings.TrimSpace(a.flags["responsible"])
	purpose := topicFlagText(a, "purpose")
	completion := topicFlagText(a, "completion")
	summary := topicFlagText(a, "summary")
	if title == "" || responsible == "" || strings.TrimSpace(purpose) == "" || strings.TrimSpace(completion) == "" {
		usage("topic create --title TEXT --responsible AGENT --purpose TEXT --completion TEXT [--summary TEXT] [--participant AGENT::RESPONSIBILITY]")
	}
	participants := []map[string]any{}
	for _, raw := range a.flagValues["participant"] {
		parts := strings.SplitN(raw, "::", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			fail(fmt.Errorf("--participant must be AGENT::RESPONSIBILITY"))
		}
		participants = append(participants, map[string]any{"agent": strings.TrimSpace(parts[0]), "responsibility": strings.TrimSpace(parts[1])})
	}
	createdFrom := map[string]any(nil)
	if ref := strings.TrimSpace(a.flags["from-ref"]); ref != "" {
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) != 2 {
			fail(fmt.Errorf("--from-ref must be TYPE:ID"))
		}
		createdFrom = map[string]any{"type": parts[0], "id": parts[1], "relation": "source", "label": a.flags["from-label"]}
	}
	payload := map[string]any{
		"title": title, "purpose": purpose, "completionBoundary": completion, "responsibleAgent": responsible,
		"participants": participants, "createdBy": defaultValue(a.flags["from"], "owner"), "initialBrief": map[string]any{
			"summary": summary, "currentState": topicFlagText(a, "state"), "nextStep": topicFlagText(a, "next"), "limitations": topicFlagText(a, "limitations"),
		},
	}
	if createdFrom != nil {
		payload["createdFrom"] = createdFrom
	}
	resp, err := api("POST", "/api/topics", payload)
	if err != nil {
		fail(err)
	}
	topic, _ := resp["topic"].(map[string]any)
	fmt.Printf("%s %s\n", green("Topic created"), str(topic, "id"))
	printTopicDetail(topic)
}

func cmdTopicSend(a args) {
	if len(a.positional) < 2 {
		usage("topic send <topic-id> [body] [--body-file PATH]")
	}
	body, err := readMsgBody(a, a.positional[2:])
	if err != nil {
		fail(err)
	}
	if strings.TrimSpace(body) == "" {
		usage("topic send <topic-id> [body] [--body-file PATH]")
	}
	payload := map[string]any{"text": body}
	if raw := strings.TrimSpace(a.flags["timeout"]); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			fail(err)
		}
		payload["timeoutSec"] = value
	}
	resp, err := api("POST", "/api/topics/"+url.PathEscape(a.positional[1])+"/send", payload)
	if err != nil {
		fail(err)
	}
	fmt.Printf("%s %s turn=%s\n", green("Topic input dispatched"), str(resp, "agentId"), str(resp, "turnId"))
}

func cmdTopicUpdate(a args) {
	if len(a.positional) < 2 {
		usage("topic update <topic-id> --from owner|RESPONSIBLE [--if-version N] [--summary TEXT] [--state TEXT] [--next TEXT] [--limitations TEXT] [--result] [--status STATUS] [--waiting TEXT --waiting-kind KIND --waiting-ref ID --resume-action TEXT] [--clear-waiting]")
	}
	actor := defaultValue(a.flags["from"], "owner")
	payload := map[string]any{"actor": actor}
	if raw := strings.TrimSpace(a.flags["if-version"]); raw != "" {
		version, err := strconv.Atoi(raw)
		if err != nil {
			fail(err)
		}
		payload["expectedVersion"] = version
	}
	command := a.positional[0]
	status := strings.TrimSpace(a.flags["status"])
	if command == "resolve" {
		status = "resolved"
	}
	if command == "archive" {
		status = "archived"
	}
	if status != "" {
		payload["status"] = status
	}
	for flag, key := range map[string]string{"title": "title", "purpose": "purpose", "completion": "completionBoundary"} {
		if topicFlagPresent(a, flag) {
			payload[key] = topicFlagText(a, flag)
		}
	}
	if _, ok := a.flags["clear-waiting"]; ok {
		payload["clearWaiting"] = true
	}
	if waiting := topicFlagText(a, "waiting"); strings.TrimSpace(waiting) != "" {
		payload["waitingOn"] = map[string]any{"kind": defaultValue(a.flags["waiting-kind"], "external"), "refId": a.flags["waiting-ref"], "summary": waiting, "resumeAction": topicFlagText(a, "resume-action")}
	}
	summary := topicFlagText(a, "summary")
	_, publishResult := a.flags["result"]
	if publishResult && strings.TrimSpace(summary) == "" {
		fail(fmt.Errorf("--result requires --summary or --summary-file"))
	}
	if strings.TrimSpace(summary) != "" {
		payload["brief"] = map[string]any{"summary": summary, "currentState": topicFlagText(a, "state"), "nextStep": topicFlagText(a, "next"), "limitations": topicFlagText(a, "limitations")}
		if publishResult {
			payload["publishResult"] = true
		}
	}
	resp, err := api("PATCH", "/api/topics/"+url.PathEscape(a.positional[1]), payload)
	if err != nil {
		fail(err)
	}
	topic, _ := resp["topic"].(map[string]any)
	printTopicDetail(topic)
}

func cmdTopicLink(a args) {
	if len(a.positional) < 4 {
		usage("topic link <topic-id> <type> <object-id> [--relation evidence] [--label TEXT] [--from owner|RESPONSIBLE]")
	}
	resp, err := api("POST", "/api/topics/"+url.PathEscape(a.positional[1])+"/links", map[string]any{"actor": defaultValue(a.flags["from"], "owner"), "type": a.positional[2], "id": a.positional[3], "relation": defaultValue(a.flags["relation"], "evidence"), "label": a.flags["label"]})
	if err != nil {
		fail(err)
	}
	topic, _ := resp["topic"].(map[string]any)
	printTopicLine(topic)
}

func cmdTopicParticipant(a args) {
	if len(a.positional) < 4 || a.positional[1] != "add" && a.positional[1] != "remove" {
		usage("topic participant add|remove <topic-id> <agent> [--responsibility TEXT] [--from owner|RESPONSIBLE]")
	}
	action, topicID, agent := a.positional[1], a.positional[2], a.positional[3]
	var resp map[string]any
	var err error
	if action == "add" {
		responsibility := topicFlagText(a, "responsibility")
		if strings.TrimSpace(responsibility) == "" {
			fail(fmt.Errorf("--responsibility is required"))
		}
		resp, err = api("POST", "/api/topics/"+url.PathEscape(topicID)+"/participants", map[string]any{"actor": defaultValue(a.flags["from"], "owner"), "agent": agent, "responsibility": responsibility})
	} else {
		query := url.Values{"actor": []string{defaultValue(a.flags["from"], "owner")}}
		resp, err = api("DELETE", "/api/topics/"+url.PathEscape(topicID)+"/participants/"+url.PathEscape(agent)+"?"+query.Encode(), nil)
	}
	if err != nil {
		fail(err)
	}
	topic, _ := resp["topic"].(map[string]any)
	printTopicDetail(topic)
}

func cmdTopicIntervene(a args) {
	if len(a.positional) < 2 || strings.TrimSpace(a.flags["agent"]) == "" || strings.TrimSpace(a.flags["action"]) == "" {
		usage("topic intervene <topic-id> --agent AGENT --action steer|interrupt [--text TEXT] [--reason TEXT]")
	}
	resp, err := api("POST", "/api/topics/"+url.PathEscape(a.positional[1])+"/interventions", map[string]any{"agent": a.flags["agent"], "action": a.flags["action"], "text": topicFlagText(a, "text"), "reason": topicFlagText(a, "reason")})
	if err != nil {
		fail(err)
	}
	value, _ := resp["intervention"].(map[string]any)
	fmt.Printf("%s %s turn=%s action=%s\n", green("intervention recorded"), str(value, "agent"), str(value, "turnId"), str(value, "action"))
}

func getTopicArgument(a args) map[string]any {
	if len(a.positional) < 2 {
		usage("topic get <topic-id>")
	}
	resp, err := api("GET", "/api/topics/"+url.PathEscape(a.positional[1]), nil)
	if err != nil {
		fail(err)
	}
	topic, _ := resp["topic"].(map[string]any)
	return topic
}

func printTopicLine(topic map[string]any) {
	attention := ""
	if num(topic, "needsMeCount") > 0 {
		attention = yellow(" needs-me")
	} else if topic["resultsReady"] == true {
		attention = green(" result-ready")
	}
	fmt.Printf("%s %-8s %-22s %s%s\n", bold(str(topic, "id")), str(topic, "status"), str(topic, "responsibleAgent"), str(topic, "title"), attention)
}

func printTopicDetail(topic map[string]any) {
	printTopicLine(topic)
	fmt.Printf("  purpose: %s\n  completion: %s\n  version: %.0f\n", str(topic, "purpose"), str(topic, "completionBoundary"), num(topic, "version"))
	if brief, ok := topic["currentBrief"].(map[string]any); ok {
		fmt.Printf("  brief v%.0f: %s\n", num(brief, "version"), str(brief, "summary"))
		if value := str(brief, "currentState"); value != "" {
			fmt.Printf("  state: %s\n", value)
		}
		if value := str(brief, "nextStep"); value != "" {
			fmt.Printf("  next: %s\n", value)
		}
		if value := str(brief, "limitations"); value != "" {
			fmt.Printf("  limitations: %s\n", value)
		}
	}
	if waiting, ok := topic["waitingOn"].(map[string]any); ok {
		fmt.Printf("  waiting: %s %s — %s\n", str(waiting, "kind"), str(waiting, "refId"), str(waiting, "summary"))
		if value := str(waiting, "resumeAction"); value != "" {
			fmt.Printf("  resume: %s\n", value)
		}
	}
	if participants := anySlice(topic["participants"]); len(participants) > 0 {
		fmt.Println("  participants:")
		for _, value := range participants {
			participant, _ := value.(map[string]any)
			fmt.Printf("    %s — %s\n", str(participant, "agent"), str(participant, "responsibility"))
		}
	}
	if turns := anySlice(topic["activeTurns"]); len(turns) > 0 {
		fmt.Println("  active Turns:")
		for _, value := range turns {
			turn, _ := value.(map[string]any)
			fmt.Printf("    %s %s — %s\n", str(turn, "agent"), str(turn, "turnId"), str(turn, "task"))
		}
	}
	if links := anySlice(topic["links"]); len(links) > 0 {
		printed := false
		for _, value := range links {
			link, _ := value.(map[string]any)
			if str(link, "relation") == "activity" {
				continue
			}
			if !printed {
				fmt.Println("  evidence:")
				printed = true
			}
			label := str(link, "label")
			if label == "" {
				label = str(link, "id")
			}
			fmt.Printf("    %s:%s [%s] — %s\n", str(link, "type"), str(link, "id"), str(link, "relation"), label)
		}
	}
}

func topicFlagText(a args, name string) string {
	if path := strings.TrimSpace(a.flags[name+"-file"]); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		return string(data)
	}
	return a.flags[name]
}

func topicFlagPresent(a args, name string) bool {
	_, direct := a.flags[name]
	_, file := a.flags[name+"-file"]
	return direct || file
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
