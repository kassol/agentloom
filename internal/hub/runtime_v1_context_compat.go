package hub

import (
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
)

// injectLegacyDeveloperContext retains the v1 test-double and compatibility
// projection only. Production Runtime Host contracts receive developer
// context as a typed StartTurn input block.
func (h *Hub) injectLegacyDeveloperContext(agentID string, rt *runtime, content string) error {
	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		return errf(404, "agent vanished")
	}
	threadID := meta.RuntimeBinding.NativeRef
	h.mu.Unlock()
	backend := runtimeBackend(rt)
	if backend == nil {
		return errf(500, "Agent Runtime is unavailable")
	}
	err := backend.InjectDeveloperContext(threadID, content, h.effectiveDeveloperContextTimeout())
	if err != nil {
		if codex.IsRequestTimeout(err) {
			h.markThreadControlIndeterminate(rt, threadID, "thread/inject_items")
		}
		return errf(500, "inject Developer context: %s", err)
	}
	return nil
}

func (h *Hub) effectiveDeveloperContextTimeout() time.Duration {
	if h.developerContextTimeout > 0 {
		return h.developerContextTimeout
	}
	return 30 * time.Second
}
