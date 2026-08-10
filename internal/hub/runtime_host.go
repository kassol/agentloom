package hub

import (
	"context"

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
