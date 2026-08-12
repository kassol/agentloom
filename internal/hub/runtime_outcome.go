package hub

import (
	"errors"
	"fmt"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// runtimeIndeterminateError preserves a typed lifecycle result when a caller
// needs Go's error control flow without losing the Runtime failure code.
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

func lifecycleOutcomeError(outcome runtimecontract.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return fmt.Errorf("invalid Runtime lifecycle outcome: %w", err)
	}
	if outcome.State == runtimecontract.LifecycleIndeterminate && outcome.Failure != nil {
		return &runtimeIndeterminateError{failure: outcome.Failure}
	}
	if outcome.Failure == nil {
		return nil
	}
	if outcome.Failure.Cause != nil {
		return outcome.Failure.Cause
	}
	if outcome.Failure.Diagnostic != "" {
		return errors.New(outcome.Failure.Diagnostic)
	}
	return errors.New(outcome.Failure.Message)
}

func runtimeLifecycleOutcomeError(outcome runtimecontract.Outcome, expected runtimecontract.LifecycleState, requireRuntimeTurnRef bool) error {
	if err := lifecycleOutcomeError(outcome); err != nil {
		return err
	}
	if outcome.State != expected {
		return fmt.Errorf("invalid Runtime lifecycle outcome: operation requires %q, got %q", expected, outcome.State)
	}
	if requireRuntimeTurnRef && outcome.RuntimeTurnRef == "" {
		return fmt.Errorf("invalid Runtime lifecycle outcome: accepted Turn requires a Runtime Turn reference")
	}
	return nil
}

func runtimeInterruptReceiptError(outcome runtimecontract.Outcome) error {
	if err := lifecycleOutcomeError(outcome); err != nil {
		return err
	}
	if outcome.State != runtimecontract.LifecycleAccepted && outcome.State != runtimecontract.LifecycleInterrupted {
		return fmt.Errorf("invalid Runtime lifecycle outcome: interrupt requires correlated receipt, got %q", outcome.State)
	}
	return nil
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
