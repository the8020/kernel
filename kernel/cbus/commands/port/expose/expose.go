// Package expose implements port.expose.
package expose

import (
	"context"
	"time"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/ports"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Ports == nil || runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "port or sandbox manager is unavailable")
		}
		inspection, err := runtimeServices.Sandboxes.Inspect(ctx, commandutil.String(request, "sandbox_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		internalPort := commandutil.Int(request, "internal_port")
		declared := false
		for _, port := range inspection.Spec.InternalPorts {
			if port == internalPort {
				declared = true
				break
			}
		}
		if !declared {
			return nil, core.NewError(core.CodeInvalidArguments, "internal port is not declared by the sandbox")
		}
		protocol := commandutil.String(request, "protocol")
		if protocol == "" {
			protocol = "tcp"
		}
		purpose := commandutil.String(request, "purpose")
		if purpose == "" {
			purpose = "administrative"
		}
		var expiration time.Time
		if seconds := commandutil.Int(request, "expiration"); seconds > 0 {
			expiration = time.Now().UTC().Add(time.Duration(seconds) * time.Second)
		}
		lease, err := runtimeServices.Ports.Expose(ctx, ports.Request{
			SandboxID: inspection.Spec.SandboxID, SandboxIP: inspection.Spec.Network.SandboxIP,
			InternalPort: internalPort, TargetPort: targetPort(inspection.Spec, internalPort), BindAddress: commandutil.String(request, "bind_address"),
			HostPort: commandutil.Int(request, "host_port"), Protocol: protocol, Purpose: purpose, ExpiresAt: expiration,
		})
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"port": lease}, nil
	}
}

func targetPort(spec model.SandboxSpec, internalPort int) int {
	if internalPort == model.DefaultSupervisorPort {
		return spec.Network.SupervisorEndpointPort()
	}
	if internalPort == model.DefaultInspectorPort {
		return spec.Network.InspectorEndpointPort()
	}
	return internalPort
}
