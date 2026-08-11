package hub

import "strings"

func inspectCodexInterruptedTurn(turn nativeHistoryTurn) RuntimeInterruptionEvidence {
	evidence := RuntimeInterruptionEvidence{Status: RuntimeInterruptionClean, UnfinishedTools: []RuntimeToolEvidence{}}
	switch strings.ToLower(strings.TrimSpace(turn.Status)) {
	case "completed", "failed", "interrupted", "aborted", "cancelled", "canceled":
		evidence.Status = RuntimeInterruptionTerminal
		evidence.TerminalStatus = strings.ToLower(strings.TrimSpace(turn.Status))
		return evidence
	}
	pendingCalls := map[string]RuntimeToolEvidence{}
	callOrder := []string{}
	for _, item := range turn.Items {
		id, _ := item["id"].(string)
		if id != "" {
			evidence.LeafEntryID = id
		}
		kind, _ := item["type"].(string)
		lowerKind := strings.ToLower(kind)
		if strings.Contains(lowerKind, "function_call_output") {
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["callId"].(string)
			}
			delete(pendingCalls, callID)
			continue
		}
		if strings.Contains(lowerKind, "function_call") {
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["callId"].(string)
			}
			if callID == "" {
				callID = id
			}
			name, _ := item["name"].(string)
			if _, exists := pendingCalls[callID]; !exists {
				callOrder = append(callOrder, callID)
			}
			pendingCalls[callID] = RuntimeToolEvidence{ID: callID, Name: name}
			continue
		}
		if !strings.Contains(lowerKind, "tool") && !strings.Contains(lowerKind, "command") {
			continue
		}
		name, _ := item["name"].(string)
		command, _ := item["command"].(string)
		evidence.UnfinishedTools = append(evidence.UnfinishedTools, RuntimeToolEvidence{ID: id, Name: name, Command: command})
	}
	for _, callID := range callOrder {
		if call, exists := pendingCalls[callID]; exists {
			evidence.UnfinishedTools = append(evidence.UnfinishedTools, call)
		}
	}
	if len(evidence.UnfinishedTools) > 0 {
		evidence.Status = RuntimeInterruptionAmbiguous
	}
	return evidence
}
