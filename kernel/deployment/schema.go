// Package deployment defines the package-to-schema activation handshake.
package deployment

import (
	"context"
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
