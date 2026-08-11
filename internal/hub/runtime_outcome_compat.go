package hub

import (
	"errors"
	"fmt"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// runtimeIndeterminateError preserves the typed v2 lifecycle result while v1
// Hub consumers coexist with Runtime Contract v2.
type runtimeIndeterminateError struct {
	failure *runtimecontract.Failure
}

func (e *runtimeIndeterminateError) Error() string {
	if e == nil || e.failure == nil {
		return "Runtime outcome is indeterminate"
	}
	return e.failure.Message
}

func (e *runtimeIndeterminateError) Unwrap() error {
	if e == nil || e.failure == nil {
		return nil
	}
	return e.failure.Cause
}

func (e *runtimeIndeterminateError) RuntimeFailure() *runtimecontract.Failure {
	if e == nil {
		return nil
	}
	return e.failure
}

func compatibilityLifecycleOutcomeError(outcome runtimecontract.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return fmt.Errorf("invalid Runtime lifecycle outcome: %w", err)
	}
	if outcome.State == runtimecontract.LifecycleIndeterminate && outcome.Failure != nil {
		return &runtimeIndeterminateError{failure: outcome.Failure}
	}
	return compatibilityOutcomeError(outcome)
}

func isRuntimeIndeterminate(err error) bool {
	var target *runtimeIndeterminateError
	return errors.As(err, &target)
}

func runtimeFailureFromError(err error) *runtimecontract.Failure {
	var typed interface {
		RuntimeFailure() *runtimecontract.Failure
	}
	if errors.As(err, &typed) {
		return typed.RuntimeFailure()
	}
	return nil
}
