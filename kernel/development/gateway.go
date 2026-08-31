package development

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"the8020/kernel/cbus/core"
)

// ActivationGateway is the only path from the workspace-scoped HTTP ingress
// to activation. Production supplies a CommandBusGateway backed by the same
// registry as the administrative socket.
type ActivationGateway interface {
	Preview(context.Context, string, ActivationOptions) (ActivationPreview, error)
	Activate(context.Context, string, ActivationOptions) (ActivationResult, error)
}

// CommandExecutor is the transport-independent command-bus dispatch surface.
type CommandExecutor interface {
	Execute(context.Context, core.Request) core.Response
}

// CommandBusGateway translates the narrow workspace request into the existing
// declarative activation commands. It does not call the development manager.
type CommandBusGateway struct{ executor CommandExecutor }

func NewCommandBusGateway(executor CommandExecutor) *CommandBusGateway {
	return &CommandBusGateway{executor: executor}
}

func (g *CommandBusGateway) Preview(ctx context.Context, workspaceID string, options ActivationOptions) (ActivationPreview, error) {
	var result ActivationPreview
	err := g.execute(ctx, "development.activate.preview", "preview", workspaceID, options, &result)
	return result, err
}

func (g *CommandBusGateway) Activate(ctx context.Context, workspaceID string, options ActivationOptions) (ActivationResult, error) {
	var result ActivationResult
	err := g.execute(ctx, "development.activate.run", "activation", workspaceID, options, &result)
	return result, err
}

func (g *CommandBusGateway) execute(ctx context.Context, commandID, resultField, workspaceID string, options ActivationOptions, output any) error {
	if g == nil || g.executor == nil {
		return errors.New("development activation command bus is unavailable")
	}
	arguments := map[string]any{"workspace_id": workspaceID}
	if options.Description != "" || commandID == "development.activate.run" {
		arguments["message"] = options.Description
	}
	if len(options.SelectedPackages) > 0 {
		arguments["packages"] = strings.Join(options.SelectedPackages, ",")
	}
	for name, value := range map[string]string{"author_name": options.AuthorName, "author_email": options.AuthorEmail} {
		if value != "" {
			arguments[name] = value
		}
	}
	for name, value := range map[string]map[string]string{"package_messages": options.PackageMessages, "metadata": options.Metadata} {
		if len(value) == 0 {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode activation %s: %w", name, err)
		}
		arguments[name] = string(encoded)
	}
	response := g.executor.Execute(ctx, core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: commandID, Arguments: arguments})
	if !response.Success {
		if response.Error != nil {
			return response.Error
		}
		return errors.New("development activation command failed")
	}
	value, ok := response.Result[resultField]
	if !ok {
		return fmt.Errorf("development activation command omitted %s result", resultField)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode development activation result: %w", err)
	}
	if err := json.Unmarshal(encoded, output); err != nil {
		return fmt.Errorf("decode development activation result: %w", err)
	}
	return nil
}
