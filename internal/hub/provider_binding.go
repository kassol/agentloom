package hub

import (
	"fmt"
	"time"
)

// ProviderSwitchBinding and ProviderBindingChange remain part of the durable
// Agent schema so current post-fork stores can reopen. New model/provider
// changes use the typed Runtime ModelControl capability.
type ProviderSwitchBinding struct {
	ProviderID string `json:"providerId"`
	Model      string `json:"model,omitempty"`
	StartedAt  string `json:"startedAt"`
}

type ProviderBindingChange struct {
	PreviousProviderID string `json:"previousProviderId"`
	PreviousModel      string `json:"previousModel,omitempty"`
	ProviderID         string `json:"providerId"`
	Model              string `json:"model,omitempty"`
	SwitchedAt         string `json:"switchedAt"`
}

func publicProviderID(providerID string) string {
	return normalizePublicProviderID(providerID)
}

func effectiveProviderBinding(agent *Agent) (string, string) {
	if agent == nil {
		return "", ""
	}
	return agent.ProviderID, agent.Model
}

// detachCodexHostLocked removes projections for one shared Host generation.
// The old OnClose callback becomes a generation no-op.
func (h *Hub) detachCodexHostLocked() *codexHostRuntime {
	host := h.codexHost
	if host == nil {
		return nil
	}
	h.codexHost = nil
	for agentID, runtime := range h.runtimes {
		if runtime != nil && runtime.hostGeneration == host.generation {
			delete(h.runtimes, agentID)
		}
	}
	h.remoteRuntime = nil
	h.remoteEnabledGeneration = 0
	h.remotePairing = nil
	if h.remoteConfig.Enabled {
		h.remoteStatus.State = "starting"
		h.remoteStatus.LastError = ""
		h.remoteStatus.UpdatedAt = now()
		h.emitRemoteLocked()
	}
	return host
}

func closeCodexHost(host *codexHostRuntime) error {
	if host == nil {
		return nil
	}
	host.close()
	if !host.client.WaitClosed(4 * time.Second) {
		return fmt.Errorf("CodexHost did not exit within 4s")
	}
	return nil
}
