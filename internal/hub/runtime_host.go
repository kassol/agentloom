package hub

import (
	"context"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// RuntimeHostDriver owns Runtime process/client supervision separately from
// the per-Agent Runtime Contract.
type RuntimeHostDriver interface {
	Preflight(context.Context) error
	Acquire(context.Context, AgentHostRequest) (AgentHost, error)
	Shutdown(context.Context) error
}

type AgentHostRequest struct {
	AgentID string
}

// AgentHost is one Agent's handle into a Runtime Host. Closing the handle must
// not close a Host shared by other Agents.
type AgentHost interface {
	Alive() bool
	Contract() runtimecontract.Contract
	SetFailureHandler(func(error))
	Close()
}

// runtimeHostGenerationSource identifies handles that share one failure
// domain without exposing the Host's native protocol client to the Hub.
type runtimeHostGenerationSource interface {
	RuntimeHostGeneration() uint64
}

type runtimeTurnCorrelationSeeder interface {
	seedTurnBindings(map[string]string)
}

// runtimeHistoryContractProvider creates a passive Contract reader without
// acquiring or starting a Runtime Host. History stays behind the registered
// Driver boundary even when an Agent is cold after restart.
type runtimeHistoryContractProvider interface {
	HistoryContract(AgentHostRequest) runtimecontract.Contract
}

// runtimeResourceContractProvider lets a shared Host inspect one Agent's
// native resources without loading that Agent's binding. Per-Agent Runtimes
// simply use their acquired Contract.
type runtimeResourceContractProvider interface {
	ResourceContract(context.Context, AgentHostRequest) (runtimecontract.Contract, string, error)
	ResourceContractCurrent(string) bool
}

type runtimeTurnCorrelationBinder interface {
	bindTurn(string, string, string)
}

type runtimeHostReadier interface {
	waitRuntimeHostReady(context.Context) error
}

type runtimeSandboxConfiguration interface{ SetRuntimeSandbox(string) }
type runtimeProviderConfiguration interface{ SetRuntimeProvider(string, string) }
type runtimeModelConfiguration interface{ SetRuntimeModel(string) }
type runtimeSkillsConfiguration interface{ SetRuntimeDisabledSkills([]string) }
type runtimeApprovalConfiguration interface{ SetRuntimeApprovalPolicy(string) }
type runtimeEffortConfiguration interface{ SetRuntimeEffort(string) }
type runtimeContextTimeoutConfiguration interface{ SetRuntimeDeveloperContextTimeout(time.Duration) }

type runtimeGoalCapability interface {
	RuntimeGoal(context.Context, runtimecontract.Binding) (*ThreadGoal, error)
	UpdateRuntimeGoal(context.Context, runtimecontract.Binding, GoalUpdateParams) (*ThreadGoal, error)
	ClearRuntimeGoal(context.Context, runtimecontract.Binding) (bool, error)
}

type runtimeCompactionCapability interface {
	CompactRuntimeBinding(context.Context, runtimecontract.Binding) error
}

type RuntimeContextEvidenceQuery = runtimecontract.ContextEvidenceQuery
type RuntimeContextEvidence = runtimecontract.ContextEvidence

func configureRuntimeBinding(contract runtimecontract.Contract, sandbox, providerID, model, effort string, disabledSkillPaths []string) {
	if capability, ok := contract.(runtimeSandboxConfiguration); ok {
		capability.SetRuntimeSandbox(sandbox)
	}
	if capability, ok := contract.(runtimeProviderConfiguration); ok {
		capability.SetRuntimeProvider(providerID, model)
	}
	if capability, ok := contract.(runtimeEffortConfiguration); ok {
		capability.SetRuntimeEffort(effort)
	}
	if capability, ok := contract.(runtimeSkillsConfiguration); ok {
		capability.SetRuntimeDisabledSkills(disabledSkillPaths)
	}
}

func configureRuntimeTurn(contract runtimecontract.Contract, approvalPolicy, sandbox, model, effort string, developerContextTimeout time.Duration) {
	if capability, ok := contract.(runtimeSandboxConfiguration); ok {
		capability.SetRuntimeSandbox(sandbox)
	}
	if capability, ok := contract.(runtimeModelConfiguration); ok {
		capability.SetRuntimeModel(model)
	}
	if capability, ok := contract.(runtimeApprovalConfiguration); ok {
		capability.SetRuntimeApprovalPolicy(approvalPolicy)
	}
	if capability, ok := contract.(runtimeEffortConfiguration); ok {
		capability.SetRuntimeEffort(effort)
	}
	if capability, ok := contract.(runtimeContextTimeoutConfiguration); ok {
		capability.SetRuntimeDeveloperContextTimeout(developerContextTimeout)
	}
}

// runtimeInterruptedTurnInspector is an optional v2 recovery capability. It
// inspects private native evidence behind a canonical Loom Turn target.
type runtimeInterruptedTurnInspector interface {
	InspectInterruptedTurn(context.Context, runtimecontract.TurnTarget) (RuntimeInterruptionEvidence, error)
}
