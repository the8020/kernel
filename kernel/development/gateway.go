package development

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"the8020/kernel/cbus/core"
)

// ActivationGateway is the only path from the sandbox HTTP ingress
// to activation. Production supplies a CommandBusGateway backed by the same
// registry as the administrative socket.
type ActivationGateway interface {
	Preview(context.Context, string, ActivationOptions) (ActivationPreview, error)
	Activate(context.Context, string, ActivationOptions) (ActivationResult, error)
}

// CommandExecutor is the transport-independent command-bus dispatch surface.
type CommandExecutor interface {
	Catalog() core.Catalog
	Execute(context.Context, core.Request) core.Response
}

// CommandBusGateway translates the narrow sandbox request into the existing
// declarative activation commands. It does not call the development manager.
type CommandBusGateway struct{ executor CommandExecutor }

func NewCommandBusGateway(executor CommandExecutor) *CommandBusGateway {
	return &CommandBusGateway{executor: executor}
}

func (g *CommandBusGateway) Preview(ctx context.Context, userID string, options ActivationOptions) (ActivationPreview, error) {
	var result ActivationPreview
	err := g.execute(ctx, "dev-core.activate.preview", "preview", userID, options, &result)
	return result, err
}

func (g *CommandBusGateway) Activate(ctx context.Context, userID string, options ActivationOptions) (ActivationResult, error) {
	var result ActivationResult
	err := g.execute(ctx, "dev-core.activate.run", "activation", userID, options, &result)
	return result, err
}

func (g *CommandBusGateway) execute(ctx context.Context, commandName, resultField, userID string, options ActivationOptions, output any) error {
	if g == nil || g.executor == nil {
		return errors.New("development activation command bus is unavailable")
	}
	catalog := g.executor.Catalog()
	commandID := ""
	for _, command := range catalog.Commands {
		if command.Name == commandName && command.Kind == core.CommandKindPackage {
			commandID = command.ID
			break
		}
	}
	if commandID == "" {
		return fmt.Errorf("development activation command %s is unavailable", commandName)
	}
	arguments := []string{userID}
	if options.Description != "" || resultField == "activation" {
		arguments = append(arguments, "--message", options.Description)
	}
	if len(options.SelectedPackages) > 0 {
		arguments = append(arguments, "--packages", strings.Join(options.SelectedPackages, ","))
	}
	if resultField == "preview" && options.PreviewFile != "" {
		arguments = append(arguments, "--file", options.PreviewFile)
	}
	for name, value := range map[string]string{"author-name": options.AuthorName, "author-email": options.AuthorEmail} {
		if value != "" {
			arguments = append(arguments, "--"+name, value)
		}
	}
	for name, value := range map[string]map[string]string{"package-messages": options.PackageMessages, "metadata": options.Metadata} {
		if len(value) == 0 {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode activation %s: %w", name, err)
		}
		arguments = append(arguments, "--"+name, string(encoded))
	}
	response := g.executor.Execute(ctx, core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: commandID, CatalogRevision: catalog.Revision, Argv: arguments})
	if !response.Success {
		if response.Error != nil {
			return response.Error
		}
		return errors.New("development activation command failed")
	}
	result, ok := response.Result.(core.Result)
	if !ok {
		if values, mapOK := response.Result.(map[string]any); mapOK {
			result, ok = core.Result(values), true
		}
	}
	if !ok {
		return errors.New("development activation command returned an invalid result")
	}
	value, ok := result[resultField]
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
