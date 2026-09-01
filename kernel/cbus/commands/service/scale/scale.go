// Package scale implements service.scale.
package scale

import (
	"context"
	"errors"
	"strconv"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Services == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "service manager is unavailable")
		}
		targetUtilization, parseErr := floatOption(request, "target_utilization")
		if parseErr != nil {
			return nil, core.NewError(core.CodeInvalidArguments, parseErr.Error())
		}
		options := webservices.ScaleOptions{
			MinimumWorkers:       intOption(request, "minimum_workers"),
			MaximumWorkers:       intOption(request, "maximum_workers"),
			ConcurrencyPerWorker: intOption(request, "concurrency_per_worker"),
			TargetUtilization:    targetUtilization,
			WorkerKeepAlive:      stringOption(request, "worker_keep_alive"),
			WorkersPerSandbox:    intOption(request, "workers_per_sandbox"),
			SandboxGroup:         stringOption(request, "sandbox_group"),
			MinimumSandboxes:     intOption(request, "minimum_sandboxes"),
			ServiceType:          stringOption(request, "service_type"),
			SessionKeepAlive:     stringOption(request, "session_keep_alive"),
		}
		if options == (webservices.ScaleOptions{}) {
			return nil, core.NewError(core.CodeInvalidArguments, "service scale requires at least one scaling, placement, or lifecycle option")
		}
		item, err := runtimeServices.Services.Scale(ctx, commandutil.String(request, "service_id"), options)
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return commandutil.WebServiceStatus(item, commandutil.Bool(request, "detail")), nil
	}
}

func floatOption(request core.Request, name string) (*float64, error) {
	if !commandutil.Has(request, name) {
		return nil, nil
	}
	value, err := strconv.ParseFloat(commandutil.String(request, name), 64)
	if err != nil {
		return nil, errors.New(name + " must be a decimal number")
	}
	return &value, nil
}

func intOption(request core.Request, name string) *int {
	if !commandutil.Has(request, name) {
		return nil
	}
	value := commandutil.Int(request, name)
	return &value
}

func stringOption(request core.Request, name string) *string {
	if !commandutil.Has(request, name) {
		return nil
	}
	value := commandutil.String(request, name)
	return &value
}
