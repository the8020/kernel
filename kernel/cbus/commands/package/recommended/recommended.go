package recommended

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
	"the8020/kernel/packages"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		items, err := management.RecommendedPackages()
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		runtime, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtime.Programs == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "recommended source selection requires the program runtime")
		}
		selected, err := runtime.Programs.RunWithOptions(ctx, "the8020/deployments/recommended-sources", "", []any{items}, nil,
			programs.Options{User: execution.SystemUser(), Timeout: 5 * time.Minute})
		if err != nil {
			return nil, commandutil.OperationError(fmt.Errorf("select recommended sources: %w; install the deployments source-selection program and repair its development connection before retrying; native kernel.packages.set/synchronize remain available for repair", err))
		}
		encoded, err := json.Marshal(selected.Value)
		if err != nil || len(encoded) > 256*1024 {
			return nil, core.NewError(core.CodeRuntimeOperation, "invalid recommended sources response")
		}
		var sources []packages.RecommendedSource
		if err := json.Unmarshal(encoded, &sources); err != nil {
			return nil, commandutil.OperationError(err)
		}
		var results []packages.RecommendedPackageResult
		if commandutil.Bool(request, "prepared_update") {
			results, err = management.InstallPreparedRecommendationsFrom(ctx, sources, serviceSet.Instance.BuildID)
		} else {
			results, err = management.InstallRecommendedPackagesFrom(ctx, sources)
		}
		failure := commandutil.OperationError(err)
		var commandError *core.Error
		if errors.As(failure, &commandError) {
			commandError.Details = map[string]any{"packages": results}
		}
		return core.Result{"packages": results}, failure
	}
}
