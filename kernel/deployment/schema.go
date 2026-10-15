// Package deployment defines the package-to-schema activation handshake.
package deployment

import (
	"context"
	"encoding/json"
	"errors"
)

type Candidate struct {
	PackageID string
	Root      string
	// An empty commit removes the package. Root remains its installed path.
	Commit string
}

type SchemaHook interface {
	// Callers persist a fresh activation ID before preparation and retain it
	// through completion. An abandoned ID must never be reused.
	Prepare(context.Context, string, []Candidate) error
	// False aborts the exact preparation, including one that never started.
	Complete(context.Context, string, bool) error
}

// Schema policy stays opaque here; source owners bind consent to one candidate.
type SchemaOptions struct {
	ForceColumns []string `json:"force_columns,omitempty"`
	ApprovedPlan string   `json:"approved_plan,omitempty"`
	Preview      bool     `json:"schema_preview,omitempty"`
}
type schemaOptionsKey struct{}
type candidatesKey struct{}

func WithCandidates(ctx context.Context, candidates []Candidate) context.Context {
	return context.WithValue(ctx, candidatesKey{}, candidates)
}
func Candidates(ctx context.Context) []Candidate {
	value, _ := ctx.Value(candidatesKey{}).([]Candidate)
	return value
}

func WithSchemaOptions(ctx context.Context, options SchemaOptions) context.Context {
	return context.WithValue(ctx, schemaOptionsKey{}, options)
}
func Options(ctx context.Context) SchemaOptions {
	value, _ := ctx.Value(schemaOptionsKey{}).(SchemaOptions)
	return value
}

type PlanningHook interface {
	Plan(context.Context, []Candidate) (json.RawMessage, error)
}
type ForwardHook interface {
	ForwardRequired(context.Context, string) (bool, error)
}
type RecoveryHook interface {
	RecoveryCandidates(context.Context, string) ([]Candidate, error)
}
type OutcomeHook interface {
	SchemaOutcome(context.Context, string) (json.RawMessage, error)
}

func NeedsForward(ctx context.Context, hook SchemaHook, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if forward, ok := hook.(ForwardHook); ok {
		return forward.ForwardRequired(ctx, id)
	}
	return false, nil
}

type unavailableHook struct{ err error }

// Unavailable returns a temporary hook that fails closed until the runtime
// installs the real schema evaluator.
func Unavailable(message string) SchemaHook {
	if message == "" {
		message = "database schema evaluator is unavailable"
	}
	return unavailableHook{err: errors.New(message)}
}

func (hook unavailableHook) Prepare(context.Context, string, []Candidate) error { return hook.err }
func (hook unavailableHook) Complete(context.Context, string, bool) error       { return hook.err }
