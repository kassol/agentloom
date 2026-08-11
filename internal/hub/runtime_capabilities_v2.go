package hub

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type runtimeCapabilityQuery struct {
	agentID  string
	binding  runtimecontract.Binding
	rt       *runtime
	contract runtimecontract.Contract
	scope    runtimecontract.CapabilityScope
}

func (h *Hub) captureRuntimeCapabilityQueryLocked(meta *Agent, rt *runtime) runtimeCapabilityQuery {
	query := runtimeCapabilityQuery{rt: rt, contract: nil, binding: runtimeContractBinding(meta)}
	if meta != nil {
		query.agentID = meta.ID
		query.scope = h.runtimeCapabilityScopeLocked(meta)
	}
	if rt != nil {
		query.contract = rt.runtimeContract
	}
	return query
}

func (h *Hub) queryRuntimeCapabilities(query runtimeCapabilityQuery) (runtimecontract.CapabilitySnapshot, error) {
	var snapshot runtimecontract.CapabilitySnapshot
	if query.contract != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snapshot = query.contract.CapabilitySnapshot(ctx, query.binding)
	} else if query.rt != nil && runtimeBackend(query.rt) != nil {
		snapshot = compatibilityControlPlaneCapabilitySnapshot(query.rt, query.binding.RuntimeKind)
	} else {
		var err error
		snapshot, err = h.coldRuntimeCapabilitySnapshot(query.binding.RuntimeKind)
		if err != nil {
			return runtimecontract.CapabilitySnapshot{}, err
		}
	}
	return scopeRuntimeCapabilitySnapshot(snapshot, query.scope)
}

func (h *Hub) refreshRuntimeCapabilitySnapshot(agentID string, emit bool) (runtimecontract.CapabilitySnapshot, bool) {
	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		return runtimecontract.CapabilitySnapshot{}, false
	}
	query := h.captureRuntimeCapabilityQueryLocked(meta, h.runtimes[agentID])
	h.mu.Unlock()
	snapshot, err := h.queryRuntimeCapabilities(query)
	if err != nil {
		return runtimecontract.CapabilitySnapshot{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.runtimeCapabilityQueryCurrentLocked(query) {
		return runtimecontract.CapabilitySnapshot{}, false
	}
	meta = h.agents[agentID]
	meta.capabilitySnapshot = snapshot
	if emit {
		h.emitStatusLocked(meta, meta.Status)
	}
	return snapshot, true
}

func (h *Hub) runtimeCapabilityQueryCurrentLocked(query runtimeCapabilityQuery) bool {
	meta := h.agents[query.agentID]
	if meta == nil || runtimeContractBinding(meta) != query.binding || h.runtimeCapabilityScopeLocked(meta) != query.scope {
		return false
	}
	return h.runtimes[query.agentID] == query.rt
}

func (h *Hub) runtimeCapabilityScopeLocked(meta *Agent) runtimecontract.CapabilityScope {
	if meta == nil {
		return runtimecontract.CapabilityScope{}
	}
	binding := runtimeContractBinding(meta)
	bindingDigest := sha256Hex([]byte(strconv.Itoa(binding.SchemaVersion) + "\x00" + binding.RuntimeKind + "\x00" + binding.NativeRef))
	configDigest := sha256Hex([]byte(strings.Join([]string{
		meta.ProviderID, meta.Model, meta.Effort, meta.Sandbox, meta.ApprovalPolicy,
		agentSkillConfigHash(h.disabledSkillPathsLocked(meta.ID)),
	}, "\x00")))
	return runtimecontract.CapabilityScope{
		RuntimeKind: binding.RuntimeKind, BindingRevision: "binding:" + bindingDigest[:16], Model: meta.Model,
		ConfigurationRevision: "config:" + configDigest[:16],
	}
}

func scopeRuntimeCapabilitySnapshot(snapshot runtimecontract.CapabilitySnapshot, expected runtimecontract.CapabilityScope) (runtimecontract.CapabilitySnapshot, error) {
	snapshot.Capabilities = append([]runtimecontract.CapabilityDescriptor(nil), snapshot.Capabilities...)
	for index := range snapshot.Capabilities {
		descriptor := &snapshot.Capabilities[index]
		for _, field := range []struct {
			name     string
			actual   *string
			expected string
		}{
			{name: "runtimeKind", actual: &descriptor.Scope.RuntimeKind, expected: expected.RuntimeKind},
			{name: "bindingRevision", actual: &descriptor.Scope.BindingRevision, expected: expected.BindingRevision},
			{name: "model", actual: &descriptor.Scope.Model, expected: expected.Model},
			{name: "configurationRevision", actual: &descriptor.Scope.ConfigurationRevision, expected: expected.ConfigurationRevision},
		} {
			if *field.actual == "" {
				*field.actual = field.expected
				continue
			}
			if *field.actual != field.expected {
				return runtimecontract.CapabilitySnapshot{}, errf(409, "Runtime capability %s scope %s does not match the current Agent; retry", descriptor.ID, field.name)
			}
		}
	}
	// A snapshot revision describes the fully scoped observation, not merely
	// the Driver's static capability vocabulary. This makes model/config/binding
	// changes observable without inventing per-Runtime revision conventions.
	sourceRevision := snapshot.Revision
	snapshot.Revision = ""
	encoded, err := json.Marshal(struct {
		Source       string                                 `json:"source"`
		Capabilities []runtimecontract.CapabilityDescriptor `json:"capabilities"`
	}{Source: sourceRevision, Capabilities: snapshot.Capabilities})
	if err != nil {
		return runtimecontract.CapabilitySnapshot{}, err
	}
	digest := sha256Hex(encoded)
	snapshot.Revision = "snapshot:" + digest[:16]
	return snapshot, nil
}

type runtimeCapabilitySnapshotProvider interface {
	CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot
}

func (h *Hub) coldRuntimeCapabilitySnapshot(runtimeKind string) (runtimecontract.CapabilitySnapshot, error) {
	h.mu.Lock()
	driver, err := h.runtimeHostDriverLocked(runtimeKind)
	h.mu.Unlock()
	if err != nil {
		return runtimecontract.CapabilitySnapshot{}, err
	}
	provider, ok := driver.(runtimeCapabilitySnapshotProvider)
	if !ok {
		return controlPlaneCapabilitySnapshot(runtimeKind, nil), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return provider.CapabilitySnapshot(ctx, runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: runtimeKind}), nil
}

var controlPlaneCapabilityAlternatives = map[string][2]string{
	runtimecontract.CapabilitySandboxConfiguration: {
		"this Runtime does not provide whole-process sandbox isolation",
		"use Approval policy to authorize individual tool actions",
	},
	runtimecontract.CapabilityProviderConfiguration: {
		"this Runtime does not apply Loom Provider configuration",
		"use the Runtime model switch operation or native Runtime settings",
	},
	runtimecontract.CapabilityApprovalPolicy: {
		"this Runtime does not enforce Loom Approval policy",
		"use the Runtime's native authorization controls",
	},
	runtimecontract.CapabilitySkillsPolicy: {
		"this Runtime does not apply Loom Skill policy",
		"manage Skills in the native Runtime settings",
	},
	runtimecontract.CapabilityContextDelivery: {
		"this Runtime does not accept Loom context delivery",
		"include the required context in the Turn input",
	},
	runtimecontract.CapabilityNativeRename: {
		"this Runtime does not expose native conversation naming",
		"use the authoritative Loom Agent name",
	},
	runtimecontract.CapabilityNativeArchive: {
		"this Runtime does not expose native conversation archival",
		"archive the authoritative Loom Agent",
	},
}

func controlPlaneCapabilitySnapshot(runtimeKind string, available map[string]bool) runtimecontract.CapabilitySnapshot {
	ids := []string{
		runtimecontract.CapabilitySandboxConfiguration,
		runtimecontract.CapabilityProviderConfiguration,
		runtimecontract.CapabilityApprovalPolicy,
		runtimecontract.CapabilitySkillsPolicy,
		runtimecontract.CapabilityContextDelivery,
		runtimecontract.CapabilityNativeRename,
		runtimecontract.CapabilityNativeArchive,
	}
	snapshot := runtimecontract.CapabilitySnapshot{Revision: "control-plane-v1"}
	for _, id := range ids {
		descriptor := runtimecontract.CapabilityDescriptor{
			ID: id, Availability: runtimecontract.CapabilityUnavailable,
			Scope: runtimecontract.CapabilityScope{RuntimeKind: runtimeKind}, Revision: "control-plane-v1",
		}
		if available[id] {
			descriptor.Availability = runtimecontract.CapabilityAvailable
		} else if reasonAlternative, ok := controlPlaneCapabilityAlternatives[id]; ok {
			descriptor.Reason, descriptor.Alternative = reasonAlternative[0], reasonAlternative[1]
		}
		snapshot.Capabilities = append(snapshot.Capabilities, descriptor)
	}
	return snapshot
}

func codexControlPlaneCapabilitySnapshot() runtimecontract.CapabilitySnapshot {
	return controlPlaneCapabilitySnapshot("codex", map[string]bool{
		runtimecontract.CapabilitySandboxConfiguration:  true,
		runtimecontract.CapabilityProviderConfiguration: true,
		runtimecontract.CapabilityApprovalPolicy:        true,
		runtimecontract.CapabilitySkillsPolicy:          true,
		runtimecontract.CapabilityContextDelivery:       true,
		runtimecontract.CapabilityNativeRename:          true,
		runtimecontract.CapabilityNativeArchive:         true,
	})
}

func piControlPlaneCapabilitySnapshot() runtimecontract.CapabilitySnapshot {
	return controlPlaneCapabilitySnapshot("pi", map[string]bool{
		runtimecontract.CapabilityApprovalPolicy:  true,
		runtimecontract.CapabilityContextDelivery: true,
	})
}

func capabilityDescriptor(snapshot runtimecontract.CapabilitySnapshot, id string) (runtimecontract.CapabilityDescriptor, bool) {
	for _, descriptor := range snapshot.Capabilities {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return runtimecontract.CapabilityDescriptor{}, false
}

func requireCapability(snapshot runtimecontract.CapabilitySnapshot, id, operation string) error {
	descriptor, ok := capabilityDescriptor(snapshot, id)
	if !ok {
		reasonAlternative := controlPlaneCapabilityAlternatives[id]
		return errf(409, "%s is unavailable: %s; alternative: %s", operation, reasonAlternative[0], reasonAlternative[1])
	}
	if descriptor.Availability == runtimecontract.CapabilityAvailable {
		return nil
	}
	reason, alternative := descriptor.Reason, descriptor.Alternative
	if reason == "" || alternative == "" {
		fallback := controlPlaneCapabilityAlternatives[id]
		if reason == "" {
			reason = fallback[0]
		}
		if alternative == "" {
			alternative = fallback[1]
		}
	}
	return errf(409, "%s is unavailable: %s; alternative: %s", operation, reason, alternative)
}

func compatibilityControlPlaneCapabilitySnapshot(rt *runtime, runtimeKind string) runtimecontract.CapabilitySnapshot {
	backend := runtimeBackend(rt)
	if backend != nil {
		legacy := backend.Capabilities()
		return controlPlaneCapabilitySnapshot(runtimeKind, map[string]bool{
			runtimecontract.CapabilitySandboxConfiguration:  legacy.Sandbox,
			runtimecontract.CapabilityProviderConfiguration: legacy.Provider,
			runtimecontract.CapabilityApprovalPolicy:        legacy.Approval,
			runtimecontract.CapabilitySkillsPolicy:          legacy.Skills,
			runtimecontract.CapabilityContextDelivery:       true,
			runtimecontract.CapabilityNativeRename:          legacy.Naming,
			runtimecontract.CapabilityNativeArchive:         legacy.Archive,
		})
	}
	return controlPlaneCapabilitySnapshot(runtimeKind, nil)
}

func (h *Hub) validateRequestedRuntimeConfiguration(runtimeKind, sandbox string, providerRequested, approvalRequested bool) error {
	snapshot, err := h.coldRuntimeCapabilitySnapshot(runtimeKind)
	if err != nil {
		return err
	}
	// danger-full-access is the compatibility spelling for no Runtime sandbox;
	// it does not claim an isolation capability.
	if sandbox = strings.TrimSpace(sandbox); sandbox != "" && sandbox != "danger-full-access" {
		if err := requireCapability(snapshot, runtimecontract.CapabilitySandboxConfiguration, "sandbox configuration"); err != nil {
			return err
		}
	}
	if providerRequested {
		if err := requireCapability(snapshot, runtimecontract.CapabilityProviderConfiguration, "Provider configuration"); err != nil {
			return err
		}
	}
	if approvalRequested {
		if err := requireCapability(snapshot, runtimecontract.CapabilityApprovalPolicy, "Approval policy configuration"); err != nil {
			return err
		}
	}
	return nil
}
