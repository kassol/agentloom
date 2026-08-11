package hub

import (
	"context"

	"github.com/yan5xu/codex-loom/internal/codex"
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

// hubLockedRuntimeHostDriver is the narrow internal acquisition seam used by
// Hub while its registry lock is held. Runtime selection happens once in the
// driver registry rather than throughout product consumers.
type hubLockedRuntimeHostDriver interface {
	RuntimeHostDriver
	acquireWhileHubLocked(context.Context, AgentHostRequest) (AgentHost, error)
}

// legacyRuntimeHost is the temporary v1 compatibility projection for product
// readers and controls assigned to later capability tickets.
type legacyRuntimeHost interface {
	legacyRuntime() AgentRuntime
}

// codexCompatibilityHost carries raw Codex notification routing only until
// the canonical public-stream ticket removes it from ordinary delivery.
type codexCompatibilityHost interface {
	codexCompatibility() (*codex.Client, uint64)
}

type runtimeTurnCorrelationSeeder interface {
	seedTurnBindings(map[string]string)
}

type runtimeTurnCorrelationBinder interface {
	bindTurn(string, string, string)
}

type runtimeHostReadier interface {
	waitRuntimeHostReady(context.Context) error
}

// runtimeCompatibilityControls carries existing sandbox/provider/model/
// Approval/Skill inputs until their optional capability tickets replace the
// compatibility fields. Product lifecycle consumers still call v2 only.
type runtimeCompatibilityControls interface {
	setCompatibilityBinding(RuntimeBindingRequest)
	setCompatibilityTurn(RuntimeTurnRequest)
}

// runtimeInterruptedTurnInspector is an optional v2 recovery capability. It
// inspects private native evidence behind a canonical Loom Turn target.
type runtimeInterruptedTurnInspector interface {
	InspectInterruptedTurn(context.Context, runtimecontract.TurnTarget) (RuntimeInterruptionEvidence, error)
}
