// Package list implements package.list.
package list

import (
	"context"
	"strings"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type summary struct {
	PackageID       string `json:"package_id"`
	Description     string `json:"description,omitempty"`
	Valid           bool   `json:"valid"`
	ServiceCount    int    `json:"service_count"`
	ValidationError string `json:"validation_error,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		service := serviceSet.PlatformSnapshot().Packages
		if service == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "package store is unavailable")
		}
		packages, err := service.ListPackages()
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		items := make([]summary, 0, len(packages))
		for _, item := range packages {
			items = append(items, summary{
				PackageID:       item.ID,
				Description:     item.Description,
				Valid:           item.Valid,
				ServiceCount:    item.ServiceCount,
				ValidationError: strings.Join(item.ValidationErrors, "; "),
			})
		}
		return core.Result{"packages": items}, nil
	}
}
