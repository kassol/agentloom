package hub

import (
	"encoding/json"
)

// deliverTestNativeNotification exercises the adapter-private decoder before
// entering the Hub through the same typed event boundary used in production.
func deliverTestNativeNotification(h *Hub, rt *runtime, method string, params json.RawMessage) {
	events := (&codexAgentRuntime{}).NormalizeEvent(method, params)
	emitted := 0
	for _, native := range events {
		turnID := testLoomTurnIDForNative(h, rt, native)
		if turnID == "" {
			continue
		}
		h.onCanonicalRuntimeEvent(rt, runtimeContractEvent(native, runtimeTurnCorrelation{turnID: turnID}))
		emitted++
	}
	h.onCodexNativeNotification(rt, method, params, emitted > 0)
}

func deliverTestNativeEvent(h *Hub, rt *runtime, native nativeEvent) {
	turnID := testLoomTurnIDForNative(h, rt, native)
	if turnID == "" {
		return
	}
	h.onCanonicalRuntimeEvent(rt, runtimeContractEvent(native, runtimeTurnCorrelation{turnID: turnID}))
}

func testLoomTurnIDForNative(h *Hub, rt *runtime, native nativeEvent) string {
	if native.LoomTurnID != "" {
		return native.LoomTurnID
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if meta := h.agents[rt.agentID]; meta != nil {
		for loomTurnID, nativeTurnID := range meta.RuntimeTurnBindings {
			if nativeTurnID == native.NativeTurnID {
				return loomTurnID
			}
		}
	}
	if rt.activeTurn != nil && rt.activeTurn.nativeTurnID == native.NativeTurnID {
		return rt.activeTurn.turnID
	}
	if native.Kind == nativeTurnStarted && rt.activeTurn != nil && !rt.activeTurn.startedConfirmed {
		return rt.activeTurn.turnID
	}
	return ""
}
