package development

import (
	"context"
	"io"
)

type SandboxStart struct {
	UserID    string
	SandboxID string
	Packages  string
	RootFS    string
	Endpoint  string
	Token     string
	Mounts    []SandboxMount
}

// SandboxMount is one validated profile mount with its canonical host source.
// Ephemeral mounts have no host source.
type SandboxMount struct {
	MountDefinition
	HostSource string
}

// SandboxDriver is deliberately development-specific: these sandboxes run a
// tool environment rather than the Deno workload supervisor.
type SandboxDriver interface {
	List(context.Context) ([]string, error)
	Start(context.Context, SandboxStart) error
	Exec(context.Context, string, string) ([]byte, error)
	ExecStream(context.Context, string, string, io.Reader, io.Writer) error
	ExecCommand(context.Context, string, []string, io.Reader, io.Writer) error
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Stop(context.Context, string) error
	Kill(context.Context, string) error
	Delete(context.Context, string) error
	Running(context.Context, string) (bool, error)
}
