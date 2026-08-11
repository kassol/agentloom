package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func cmdHistory(a args) {
	if len(a.positional) < 1 {
		usage("history <name|id>")
	}
	count := a.flags["count"]
	if count == "" {
		count = "10"
	}
	resp, err := api("GET", "/api/agents/"+url.PathEscape(a.positional[0])+"/thread/history?count="+count, nil)
	if err != nil {
		fail(err)
	}
	turns, _ := resp["turns"].([]any)
	fmt.Printf("%s %s — %d turn(s)\n\n", bold(str(resp, "name")), dim(str(resp, "threadId")), len(turns))
	for _, tv := range turns {
		t, _ := tv.(map[string]any)
		fmt.Printf("%s %s %s\n", magenta("── turn"), str(t, "turnId"), dim("["+str(t, "state")+"]"))
		content, _ := t["content"].([]any)
		for _, value := range content {
			block, _ := value.(map[string]any)
			printCanonicalContent(block)
		}
		fmt.Println()
	}
}

func cmdTurnGet(a args) {
	if len(a.positional) != 1 {
		usage("turn get <turn-id> [--json]")
	}
	resp, err := api("GET", "/api/turns/"+url.PathEscape(a.positional[0]), nil)
	if err != nil {
		fail(err)
	}
	turn, _ := resp["turn"].(map[string]any)
	if a.flags["json"] == "true" {
		encoded, marshalErr := json.MarshalIndent(turn, "", "  ")
		if marshalErr != nil {
			fail(marshalErr)
		}
		fmt.Println(string(encoded))
		return
	}

	fmt.Printf("%s %s %s\n", magenta("turn"), str(turn, "turnId"), dim("["+str(turn, "state")+"]"))
	fmt.Printf("agent: %s %s\n", bold(str(turn, "agent")), dim(str(turn, "agentId")))
	fmt.Printf("thread: %s\n", dim(str(turn, "threadId")))
	if startedAt := str(turn, "startedAt"); startedAt != "" {
		fmt.Printf("time: %s", startedAt)
		if completedAt := str(turn, "completedAt"); completedAt != "" {
			fmt.Printf(" → %s", completedAt)
		}
		fmt.Println()
	}
	if source, ok := turn["source"].(map[string]any); ok {
		fmt.Printf("source: %s", str(source, "kind"))
		if id := str(source, "id"); id != "" {
			fmt.Printf(" %s", id)
		}
		if topicID := str(source, "topicId"); topicID != "" {
			fmt.Printf(" · topic %s", topicID)
		}
		fmt.Println()
	}
	if model := str(turn, "model"); model != "" {
		fmt.Printf("model: %s", model)
		if usage, ok := turn["usage"].(map[string]any); ok {
			fmt.Printf(" · %.0f tokens", num(usage, "totalTokens"))
		}
		fmt.Println()
	}
	if message := str(turn, "error"); message != "" {
		fmt.Printf("error: %s\n", red(message))
	}
	fmt.Println()
	content, _ := turn["content"].([]any)
	for _, value := range content {
		block, _ := value.(map[string]any)
		printCanonicalContent(block)
	}
}

func printCanonicalContent(content map[string]any) {
	switch str(content, "kind") {
	case "user_text":
		fmt.Printf("  %s %s\n", cyan("user>"), oneline(str(content, "text"), 200))
	case "assistant_text":
		fmt.Printf("  %s %s\n", green("agent>"), indent(strings.TrimSpace(str(content, "text"))))
	case "reasoning":
		fmt.Printf("  %s\n", dim("think: "+oneline(str(content, "text"), 160)))
	case "tool_call":
		tool, _ := content["toolCall"].(map[string]any)
		arguments, _ := json.Marshal(tool["arguments"])
		fmt.Printf("  %s %s %s\n", yellow("$"), str(tool, "name"), dim(clip(string(arguments), 160)))
	case "tool_result":
		result, _ := content["toolResult"].(map[string]any)
		status := red("failed")
		if result["success"] == true {
			status = green("completed")
		}
		fmt.Printf("  %s %s\n", status, indent(strings.TrimSpace(str(result, "text"))))
	case "image":
		image, _ := content["image"].(map[string]any)
		fmt.Printf("  %s %s\n", magenta("image"), str(image, "ref"))
	}
}

// ---- watch (SSE) ----

func cmdWatch(a args) {
	if len(a.positional) < 1 {
		usage("watch <name|id>")
	}
	key := a.positional[0]
	tail := a.flags["tail"]
	if tail == "" {
		tail = "50"
	}
	resp, err := api("GET", "/api/agents/"+url.PathEscape(key), nil)
	if err != nil {
		fail(err)
	}
	s, _ := resp["agent"].(map[string]any)
	fmt.Println(dim(fmt.Sprintf("watching %s (%s) — status: %s — Ctrl-C detaches (task keeps running)\n",
		str(s, "name"), str(s, "id"), str(s, "status"))))

	eventsPath := "/api/agents/" + url.PathEscape(key) + "/thread/events?tail=" + tail
	req, _ := http.NewRequest("GET", base+eventsPath, nil)
	req.Header.Set("Accept", "text/event-stream")
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail(err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != 200 {
		fail(fmt.Errorf("events stream: %s", httpResp.Status))
	}

	state := &watchState{}
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Seq  int64           `json:"seq"`
			TS   string          `json:"ts"`
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			continue
		}
		renderEvent(state, ev.Seq, ev.TS, ev.Type, ev.Data)
	}
	fmt.Println(dim("\nstream closed by hub"))
}

type watchState struct {
	streamOpen  bool
	streamThink bool
}

func (st *watchState) closeStream() {
	if st.streamOpen {
		fmt.Println()
		st.streamOpen = false
	}
}

func tsShort(ts string) string {
	if len(ts) >= 19 {
		return dim(ts[11:19])
	}
	return dim(ts)
}

func renderEvent(st *watchState, seq int64, ts, typ string, data json.RawMessage) {
	var d map[string]any
	_ = json.Unmarshal(data, &d)
	t := tsShort(ts)
	st.closeStream()

	switch typ {
	case "loom/runtime-event":
		renderCanonicalWatchEvent(st, t, d)
	case "loom/live":
		fmt.Println(dim(fmt.Sprintf("─── live (replayed up to seq %d) ───", seq)))
	case "loom/agent-created":
		fmt.Printf("%s %s %v @ %v\n", t, dim("agent created:"), d["name"], d["cwd"])
	case "loom/user-message":
		fmt.Printf("%s %s %v\n", t, cyan("user>"), d["text"])
	case "loom/turn-started":
		fmt.Printf("%s %s\n", t, dim(fmt.Sprintf("turn started %v", d["turnId"])))
	case "loom/turn-completed":
		secs := 0.0
		if v, ok := d["durationMs"].(float64); ok {
			secs = v / 1000
		}
		fmt.Printf("%s %s %s\n", t, green("✔ turn completed"), dim(fmt.Sprintf("(%.0fs)", secs)))
	case "loom/turn-interrupted":
		reason, _ := d["reason"].(string)
		if reason == "" {
			reason, _ = d["error"].(string)
		}
		fmt.Printf("%s %s %s\n", t, yellow("■ turn interrupted"), dim(reason))
	case "loom/turn-failed":
		fmt.Printf("%s %s\n", t, red(fmt.Sprintf("✖ turn failed: %v", d["error"])))
	case "loom/agent-archived":
		fmt.Printf("%s %s\n", t, yellow("agent archived"))
	case "loom/approval-requested":
		params, _ := json.Marshal(d["params"])
		fmt.Printf("%s %s %v %s\n", t, red("⚠ approval requested"), d["method"], dim(clip(string(params), 120)))
		fmt.Printf("%s %s\n", t, dim(fmt.Sprintf("  resolve: %s approve <agent> %v — or via web console", commandName, d["approvalId"])))
	case "loom/approval-resolved":
		fmt.Printf("%s %s %s\n", t, green(fmt.Sprintf("approval %v", d["decision"])), dim(fmt.Sprintf("%v", d["approvalId"])))
	case "loom/error", "loom/host-error":
		fmt.Printf("%s %s %v\n", t, red("CodexLoom error:"), d["message"])
	default:
		if os.Getenv("CODEX_LOOM_DEBUG") != "" || os.Getenv("CHUB_DEBUG") != "" {
			raw, _ := json.Marshal(d)
			fmt.Printf("%s %s %s\n", t, dim(typ), dim(clip(string(raw), 160)))
		}
	}
}

func renderCanonicalWatchEvent(st *watchState, timestamp string, event map[string]any) {
	if str(event, "kind") == "terminal" {
		st.closeStream()
		return
	}
	if str(event, "kind") != "content" {
		return
	}
	content, _ := event["content"].(map[string]any)
	phase := str(event, "contentPhase")
	text := str(content, "text")
	switch str(content, "kind") {
	case "assistant_text", "reasoning":
		isThink := str(content, "kind") == "reasoning"
		if phase == "delta" {
			if !st.streamOpen || st.streamThink != isThink {
				st.closeStream()
				if isThink {
					fmt.Print(dim("think "))
				} else {
					fmt.Print(green("agent> "))
				}
				st.streamOpen, st.streamThink = true, isThink
			}
			if isThink {
				fmt.Print(dim(text))
			} else {
				fmt.Print(text)
			}
			return
		}
		st.closeStream()
		if text != "" {
			label := green("agent>")
			if isThink {
				label = dim("think")
			}
			fmt.Printf("%s %s %s\n", timestamp, label, strings.TrimSpace(text))
		}
	case "tool_call", "tool_result":
		st.closeStream()
		printCanonicalContent(content)
	}
}

// ---- text helpers ----

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func oneline(s string, n int) string {
	return clip(strings.Join(strings.Fields(s), " "), n)
}

func indent(s string) string {
	return strings.ReplaceAll(s, "\n", "\n         ")
}

func anySlice(v any) []any {
	items, _ := v.([]any)
	return items
}

func stringValues(v any) []string {
	values := []string{}
	for _, item := range anySlice(v) {
		if value, ok := item.(string); ok && value != "" {
			values = append(values, value)
		}
	}
	return values
}

func csvValues(raw string) []string {
	values := []string{}
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
