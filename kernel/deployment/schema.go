// Package deployment defines the package-to-schema activation handshake.
package deployment

import (
	"context"
	"errors"
)

type Candidate struct {
	PackageID string
	Root      string
	Commit    string
}

type SchemaHook interface {
	Prepare(context.Context, []Candidate) error
	Complete(context.Context, bool) error
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

func (hook unavailableHook) Prepare(context.Context, []Candidate) error { return hook.err }
func (hook unavailableHook) Complete(context.Context, bool) error       { return hook.err }
