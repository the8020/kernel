package callback

import (
	"context"
	"strings"

	"the8020/kernel/cbus/cli"
	"the8020/kernel/cbus/core"
)

type consoleExecutor struct{ AdminBus }

func (e consoleExecutor) Execute(ctx context.Context, request core.Request) (core.Response, error) {
	return e.AdminBus.Execute(ctx, request), nil
}

type consoleOutput struct {
	strings.Builder
	truncated bool
}

func (b *consoleOutput) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := (64 << 10) - b.Len(); n > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.Builder.Write(data)
	return n, nil
}

func (s *Server) administration(ctx context.Context, operation string, input map[string]any) (any, error) {
	if s.adminBus == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "administration is unavailable")
	}
	catalog := s.adminBus.Catalog()
	if operation == "admin.catalog" {
		if len(input) != 0 {
			return nil, core.NewError(core.CodeInvalidArguments, "catalog takes no arguments")
		}
		return catalog, nil
	}
	line, ok := input["line"].(string)
	if !ok || len(input) != 1 {
		return nil, core.NewError(core.CodeInvalidArguments, "line is required")
	}
	var output consoleOutput
	code := cli.New(catalog.Commands, consoleExecutor{s.adminBus}).RunLine(ctx, line, &output)
	text := strings.ToValidUTF8(output.String(), "�")
	if output.truncated {
		text += "\n[Output truncated at 64 KiB]"
	}
	return map[string]any{"exit_code": code, "output": text, "truncated": output.truncated}, nil
}
