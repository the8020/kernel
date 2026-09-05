// Package packagecommands contains shared, package-domain command behavior.
package packagecommands

import (
	"context"

	"the8020/kernel/cbus/core"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/services"
)

// SynchronizationResult is the complete public result for one synchronized
// package. Detailed Git and service-refresh state remains internal.
type SynchronizationResult struct {
	PackageID string `json:"package_id"`
	Commit    string `json:"commit"`
	Success   bool   `json:"success"`
}

func Management(serviceSet *services.Services) (services.PackageManagementService, error) {
	service := serviceSet.PackageManagementSnapshot()
	if service == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "package management is unavailable")
	}
	return service, nil
}

// Synchronize updates package worktrees and then refreshes only services owned
// by changed packages. Offline deployment dispatch has no runtime service and
// therefore performs only the package transaction.
func Synchronize(ctx context.Context, serviceSet *services.Services, packageIDs []string) ([]SynchronizationResult, error) {
	return SynchronizeWithCredential(ctx, serviceSet, packageIDs, "")
}

// SynchronizeWithCredential performs the same package transaction with one
// invocation-scoped Git token supplied by the administrative client.
func SynchronizeWithCredential(ctx context.Context, serviceSet *services.Services, packageIDs []string, token string) ([]SynchronizationResult, error) {
	management, err := Management(serviceSet)
	if err != nil {
		return nil, err
	}
	var results []workspacepackages.PackageSynchronization
	if token == "" {
		results, err = management.SynchronizePackages(ctx, packageIDs)
	} else if credentialManagement, ok := management.(services.PackageCredentialManagementService); ok {
		results, err = credentialManagement.SynchronizePackagesWithCredential(ctx, packageIDs, token)
	} else {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "transient Git credentials are unavailable")
	}
	if err != nil {
		return nil, err
	}
	summaries := make([]SynchronizationResult, 0, len(results))
	firstFailure := -1
	for index, result := range results {
		summaries = append(summaries, SynchronizationResult{
			PackageID: result.PackageID,
			Commit:    result.Commit,
			Success:   result.Success,
		})
		if !result.Success && firstFailure < 0 {
			firstFailure = index
		}
	}
	if firstFailure >= 0 {
		failed := results[firstFailure]
		message := "one or more packages failed to synchronize"
		if failed.Error != "" {
			message += ": " + failed.PackageID + ": " + failed.Error
		}
		commandError := core.NewError(core.CodeRuntimeOperation, message)
		commandError.Details = map[string]any{"packages": summaries}
		return summaries, commandError
	}
	return summaries, nil
}
