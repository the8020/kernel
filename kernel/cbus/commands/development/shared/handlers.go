package shared

import (
	"context"
	"encoding/json"
	"errors"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/development"
	"the8020/kernel/services"
)

func service(serviceSet *services.Services) (services.DevelopmentService, error) {
	service := serviceSet.PlatformSnapshot().Development
	if service == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "development sandbox manager is unavailable")
	}
	return service, nil
}
func operation(err error) error {
	if err == nil {
		return nil
	}
	var commandError *core.Error
	if errors.As(err, &commandError) {
		return err
	}
	return commandutil.OperationError(err)
}

func ImageStatus(s *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		result, err := service.ImageStatus()
		return core.Result{"image": result}, operation(err)
	}
}
func SandboxCreate(s *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		result, err := service.Create(ctx, commandutil.String(request, "user_id"))
		return core.Result{"sandbox": result}, operation(err)
	}
}
func SandboxList(s *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		result, err := service.List()
		return core.Result{"sandboxes": result}, operation(err)
	}
}
func SandboxInspect(s *services.Services) core.Handler {
	return sandboxOne(s, func(ctx context.Context, service services.DevelopmentService, userID string, _ core.Request) (development.Sandbox, error) {
		return service.Inspect(userID)
	})
}
func SandboxStart(s *services.Services) core.Handler {
	return sandboxOne(s, func(ctx context.Context, service services.DevelopmentService, userID string, _ core.Request) (development.Sandbox, error) {
		return service.Start(ctx, userID)
	})
}
func SandboxStop(s *services.Services) core.Handler {
	return sandboxOne(s, func(ctx context.Context, service services.DevelopmentService, userID string, _ core.Request) (development.Sandbox, error) {
		return service.Stop(ctx, userID)
	})
}
func SandboxRestart(s *services.Services) core.Handler {
	return sandboxOne(s, func(ctx context.Context, service services.DevelopmentService, userID string, _ core.Request) (development.Sandbox, error) {
		return service.Restart(ctx, userID)
	})
}
func SandboxKill(s *services.Services) core.Handler {
	return sandboxOne(s, func(ctx context.Context, service services.DevelopmentService, userID string, _ core.Request) (development.Sandbox, error) {
		return service.Kill(ctx, userID)
	})
}
func SandboxResetSource(s *services.Services) core.Handler {
	return sandboxOne(s, func(ctx context.Context, service services.DevelopmentService, userID string, request core.Request) (development.Sandbox, error) {
		return service.ResetSource(ctx, userID, commandutil.Bool(request, "confirm"))
	})
}
func SandboxFactoryReset(s *services.Services) core.Handler {
	return sandboxOne(s, func(ctx context.Context, service services.DevelopmentService, userID string, request core.Request) (development.Sandbox, error) {
		return service.FactoryReset(ctx, userID, commandutil.Bool(request, "confirm"))
	})
}
func sandboxOne(s *services.Services, call func(context.Context, services.DevelopmentService, string, core.Request) (development.Sandbox, error)) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		result, err := call(ctx, service, commandutil.String(request, "user_id"), request)
		return core.Result{"sandbox": result}, operation(err)
	}
}
func SandboxDelete(s *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		err = service.Delete(ctx, commandutil.String(request, "user_id"))
		return core.Result{"deleted": err == nil}, operation(err)
	}
}
func SandboxShell(s *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		command := commandutil.String(request, "command")
		if command == "" {
			command = "pwd"
		}
		result, err := service.Shell(ctx, commandutil.String(request, "user_id"), command)
		return core.Result{"shell": result}, operation(err)
	}
}
func ActivatePreview(s *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		options, err := activationOptions(request)
		if err != nil {
			return nil, err
		}
		result, err := service.Preview(ctx, commandutil.String(request, "user_id"), options)
		return core.Result{"preview": result}, operation(err)
	}
}
func ActivateRun(s *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := service(s)
		if err != nil {
			return nil, err
		}
		options, err := activationOptions(request)
		if err != nil {
			return nil, err
		}
		result, activationErr := service.Activate(ctx, commandutil.String(request, "user_id"), options)
		if result.Status != "" {
			return core.Result{"activation": result}, nil
		}
		return nil, operation(activationErr)
	}
}
func activationOptions(request core.Request) (development.ActivationOptions, error) {
	messages := map[string]string{}
	if raw := commandutil.String(request, "package_messages"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &messages); err != nil {
			return development.ActivationOptions{}, core.NewError(core.CodeInvalidArguments, "package-messages must be a JSON object of package IDs to messages")
		}
	}
	metadata := map[string]string{}
	if raw := commandutil.String(request, "metadata"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			return development.ActivationOptions{}, core.NewError(core.CodeInvalidArguments, "metadata must be a JSON object of string values")
		}
	}
	return development.ActivationOptions{Description: commandutil.String(request, "message"), SelectedPackages: commandutil.CSV(request, "packages"), PackageMessages: messages, AuthorName: commandutil.String(request, "author_name"), AuthorEmail: commandutil.String(request, "author_email"), Metadata: metadata}, nil
}
