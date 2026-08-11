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
	} else {
		var err error
		snapshot, err = h.coldRuntimeCapabilitySnapshot(query.binding.RuntimeKind)
		if err != nil {
			return runtimecontract.CapabilitySnapshot{}, err
		}
	}
	scoped, err := scopeRuntimeCapabilitySnapshot(snapshot, query.scope)
	if err != nil {
		return runtimecontract.CapabilitySnapshot{}, err
	}
	if query.contract != nil {
		if err := validateRuntimeCapabilityHooks(query.contract, scoped); err != nil {
			return runtimecontract.CapabilitySnapshot{}, err
		}
	}
	return scoped, nil
}

func validateRuntimeCapabilityHooks(contract runtimecontract.Contract, snapshot runtimecontract.CapabilitySnapshot) error {
	for _, descriptor := range snapshot.Capabilities {
		if descriptor.Availability != runtimecontract.CapabilityAvailable {
			continue
		}
		implemented := false
		switch descriptor.ID {
		case runtimecontract.CapabilitySandboxConfiguration:
			_, implemented = contract.(runtimeSandboxConfiguration)
		case runtimecontract.CapabilityApprovalPolicy:
			_, configured := contract.(runtimeApprovalConfiguration)
			_, governed := contract.(runtimecontract.ApprovalCapability)
			implemented = configured && governed
		case runtimecontract.CapabilitySkillsPolicy:
			_, implemented = contract.(runtimeSkillsConfiguration)
		case runtimecontract.CapabilityContextDelivery:
			_, implemented = contract.(runtimecontract.ContextDeliveryPolicy)
		case runtimecontract.CapabilityNativeRename:
			_, implemented = contract.(runtimecontract.BindingNameCapability)
		case runtimecontract.CapabilityNativeArchive:
			_, implemented = contract.(runtimecontract.BindingArchiveCapability)
		case runtimecontract.CapabilityGoal:
			_, implemented = contract.(runtimeGoalCapability)
		case runtimecontract.CapabilityUsageReporting:
			_, implemented = contract.(runtimeUsageCapability)
		case runtimecontract.CapabilityModelConfiguration:
			_, implemented = contract.(runtimecontract.ModelControlCapability)
		case runtimecontract.CapabilityManualCompaction:
			_, implemented = contract.(runtimeCompactionCapability)
		case runtimecontract.CapabilityImageInput:
			_, implemented = contract.(runtimecontract.InputCapability)
		}
		if !implemented {
			return errf(500, "Runtime advertises capability %q without its typed hook", descriptor.ID)
		}
	}
	return nil
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
	if strings.TrimSpace(snapshot.Revision) == "" {
		return runtimecontract.CapabilitySnapshot{}, errf(500, "Runtime capability snapshot revision is required")
	}
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
	if err := snapshot.Validate(); err != nil {
		return runtimecontract.CapabilitySnapshot{}, errf(500, "invalid Runtime capability snapshot: %s", err)
	}
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
		return controlPlaneCapabilitySnapshot(runtimeKind), nil
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
	runtimecontract.CapabilityGoal: {
		"this Runtime does not provide Goal tracking",
		"track the objective in the Agent Thread",
	},
	runtimecontract.CapabilityRemote: {
		"this Runtime does not provide remote conversation adoption",
		"create a Loom-owned Agent binding",
	},
	runtimecontract.CapabilityUsageReporting: {
		"this Runtime does not report token usage",
		"inspect usage in the native Runtime",
	},
	runtimecontract.CapabilityModelConfiguration: {
		"this Runtime does not expose model configuration",
		"configure the model in the native Runtime",
	},
	runtimecontract.CapabilityManualCompaction: {
		"this Runtime does not expose manual compaction",
		"allow the Runtime to compact automatically",
	},
	runtimecontract.CapabilityImageInput: {
		"the active Runtime model does not accept image input",
		"select an image-capable model or send text and file attachments",
	},
}

func runtimeCapabilityDescriptor(runtimeKind, id string, available bool) runtimecontract.CapabilityDescriptor {
	descriptor := runtimecontract.CapabilityDescriptor{
		ID: id, Availability: runtimecontract.CapabilityUnavailable,
		Scope: runtimecontract.CapabilityScope{RuntimeKind: runtimeKind}, Revision: "runtime-contract-v2",
	}
	if available {
		descriptor.Availability = runtimecontract.CapabilityAvailable
		return descriptor
	}
	if reasonAlternative, ok := controlPlaneCapabilityAlternatives[id]; ok {
		descriptor.Reason, descriptor.Alternative = reasonAlternative[0], reasonAlternative[1]
	}
	return descriptor
}

func controlPlaneCapabilitySnapshot(runtimeKind string) runtimecontract.CapabilitySnapshot {
	return runtimecontract.CapabilitySnapshot{Revision: "runtime-contract-v2", Capabilities: []runtimecontract.CapabilityDescriptor{
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilitySandboxConfiguration, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityApprovalPolicy, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilitySkillsPolicy, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityContextDelivery, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityNativeRename, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityNativeArchive, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityGoal, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityRemote, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityUsageReporting, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityModelConfiguration, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityManualCompaction, false),
		runtimeCapabilityDescriptor(runtimeKind, runtimecontract.CapabilityImageInput, false),
	}}
}

func codexControlPlaneCapabilitySnapshot(imageInput ...bool) runtimecontract.CapabilitySnapshot {
	imageAvailable := len(imageInput) > 0 && imageInput[0]
	return runtimecontract.CapabilitySnapshot{Revision: "runtime-contract-v2", Capabilities: []runtimecontract.CapabilityDescriptor{
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilitySandboxConfiguration, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityApprovalPolicy, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilitySkillsPolicy, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityContextDelivery, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityNativeRename, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityNativeArchive, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityGoal, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityRemote, false),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityUsageReporting, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityModelConfiguration, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityManualCompaction, true),
		runtimeCapabilityDescriptor("codex", runtimecontract.CapabilityImageInput, imageAvailable),
	}}
}

func piControlPlaneCapabilitySnapshot(imageInput ...bool) runtimecontract.CapabilitySnapshot {
	imageAvailable := len(imageInput) > 0 && imageInput[0]
	return runtimecontract.CapabilitySnapshot{Revision: "runtime-contract-v2", Capabilities: []runtimecontract.CapabilityDescriptor{
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilitySandboxConfiguration, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityApprovalPolicy, true),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilitySkillsPolicy, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityContextDelivery, true),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityNativeRename, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityNativeArchive, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityGoal, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityRemote, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityUsageReporting, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityModelConfiguration, true),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityManualCompaction, false),
		runtimeCapabilityDescriptor("pi", runtimecontract.CapabilityImageInput, imageAvailable),
	}}
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
		if err := requireCapability(snapshot, runtimecontract.CapabilityModelConfiguration, "Runtime model configuration"); err != nil {
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
