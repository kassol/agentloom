package hub

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/yan5xu/codex-loom/internal/store"
)

var (
	runtimeFailureBearerPattern          = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`)
	runtimeFailureSecretPattern          = regexp.MustCompile(`(?i)(authorization|api[_-]?key|access[_-]?key|password|passwd|secret|credential|session[_-]?token|token)(["']?\s*[:=]\s*["']?)[^"'\s,;&#?]+`)
	runtimeFailurePathPattern            = regexp.MustCompile(`(^|[\s"'(])/(?:Users|home|private|tmp|var)/[^\s"'),;]+`)
	runtimeDiagnosticAuthHeaderPattern   = regexp.MustCompile(`(?i)\bauthorization\s*:\s*[a-z][a-z0-9_-]*\s+[^\s"',;]+`)
	runtimeDiagnosticCookieHeaderPattern = regexp.MustCompile(`(?i)(\bcookie\s*:\s*)[^;,"'\s]+=[^;,"'\s]+(?:\s*;\s*[^;,"'\s]+=[^;,"'\s]+)*`)
	runtimeDiagnosticCLICookiePattern    = regexp.MustCompile(`(?i)(^|\s)(--cookie|-b)(\s+)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	runtimeDiagnosticCLISecretPattern    = regexp.MustCompile(`(?i)(--?(?:authorization|api[_-]?key|access[_-]?key|password|passwd|secret|credential|session[_-]?token|token))(\s+)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	runtimeDiagnosticQuerySecretPattern  = regexp.MustCompile(`(?i)([?&](?:authorization|api[_-]?key|access[_-]?key|password|passwd|secret|credential|session[_-]?token|token)=)[^&#\s]+`)
	runtimeDiagnosticLooseSecretPattern  = regexp.MustCompile(`(?i)\b(authorization|api[_-]?key|access[_-]?key|password|passwd|secret|credential|session[_-]?token|token)(\s+(?:is\s+)?)(?:"[^"]*"|'[^']*'|[^\s,;&#?]+)`)
)

func runtimeDiagnosticEventLogID(agentID string) string {
	return "__runtime-diagnostic-" + agentID
}

func (h *Hub) appendRuntimeDiagnosticLocked(agentID, typ string, data json.RawMessage) {
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(typ) == "" {
		return
	}
	logID := runtimeDiagnosticEventLogID(agentID)
	event := store.Event{Seq: h.st.LastSeq(logID) + 1, TS: now(), Type: typ, Data: redactRuntimeDiagnostic(data)}
	if err := h.st.AppendEvent(logID, event); err != nil {
		return
	}
}

func redactRuntimeDiagnostic(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("{}")
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	value = redactRuntimeDiagnosticValue("", value)
	redacted, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	return redacted
}

func redactRuntimeDiagnosticValue(key string, value any) any {
	if runtimeDiagnosticSecretKey(key) {
		return "[redacted]"
	}
	switch typed := value.(type) {
	case map[string]any:
		projected := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			projected[childKey] = redactRuntimeDiagnosticValue(childKey, childValue)
		}
		return projected
	case []any:
		projected := make([]any, len(typed))
		for i, child := range typed {
			projected[i] = redactRuntimeDiagnosticValue(key, child)
		}
		return projected
	case string:
		redacted := redactRuntimeDiagnosticString(typed)
		if parsed, err := url.Parse(redacted); err == nil && parsed.IsAbs() {
			parsed.User = nil
			query := parsed.Query()
			for queryKey := range query {
				if runtimeDiagnosticSecretKey(queryKey) {
					query.Set(queryKey, "[redacted]")
				}
			}
			parsed.RawQuery = query.Encode()
			return redactRuntimeDiagnosticString(parsed.String())
		}
		return redacted
	}
	return value
}

func redactRuntimeDiagnosticString(value string) string {
	value = runtimeDiagnosticAuthHeaderPattern.ReplaceAllString(value, "Authorization: [redacted]")
	value = runtimeDiagnosticCookieHeaderPattern.ReplaceAllString(value, "$1[redacted]")
	value = runtimeDiagnosticCLICookiePattern.ReplaceAllString(value, "$1$2$3[redacted]")
	value = runtimeFailureBearerPattern.ReplaceAllString(value, "Bearer [redacted]")
	value = runtimeDiagnosticCLISecretPattern.ReplaceAllString(value, "$1$2[redacted]")
	value = runtimeDiagnosticQuerySecretPattern.ReplaceAllString(value, "$1[redacted]")
	value = runtimeFailureSecretPattern.ReplaceAllString(value, "$1$2[redacted]")
	value = runtimeDiagnosticLooseSecretPattern.ReplaceAllString(value, "$1$2[redacted]")
	return value
}

func runtimeDiagnosticSecretKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
	for _, fragment := range []string{"authorization", "apikey", "accesskey", "password", "passwd", "secret", "credential", "bearer", "cookie", "sessiontoken"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return normalized == "token" || strings.HasSuffix(normalized, "token")
}

func publicRuntimeFailureMessage(meta *Agent, turnID, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	if meta != nil {
		if nativeRef := strings.TrimSpace(meta.RuntimeBinding.NativeRef); nativeRef != "" {
			replacement := strings.TrimSpace(meta.ThreadID)
			if replacement == "" {
				replacement = "[runtime binding]"
			}
			message = strings.ReplaceAll(message, nativeRef, replacement)
		}
		for loomTurnID, nativeTurnID := range meta.RuntimeTurnBindings {
			if nativeTurnID != "" {
				message = strings.ReplaceAll(message, nativeTurnID, loomTurnID)
			}
		}
	}
	message = runtimeFailureBearerPattern.ReplaceAllString(message, "Bearer [redacted]")
	message = runtimeFailureSecretPattern.ReplaceAllString(message, "$1$2[redacted]")
	message = runtimeFailurePathPattern.ReplaceAllString(message, "$1[runtime path]")
	return message
}

// ReadRuntimeDiagnosticEvents is the explicit opt-in native protocol record.
// Ordinary Agent history and SSE never read this separate diagnostic log.
func (h *Hub) ReadRuntimeDiagnosticEvents(key string, since int64, tail int) ([]store.Event, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	h.mu.Unlock()
	if meta == nil {
		return nil, errf(404, "agent not found: %s", key)
	}
	events, err := h.st.ReadEvents(runtimeDiagnosticEventLogID(meta.ID), since, tail)
	if err != nil {
		return nil, err
	}
	for index := range events {
		events[index].Data = redactRuntimeDiagnostic(events[index].Data)
	}
	return events, nil
}
