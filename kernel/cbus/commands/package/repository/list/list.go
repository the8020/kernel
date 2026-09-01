// Package list implements package.repository.list.
package list

import (
	"context"

	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		repositories, err := management.ListPackageRepositories(ctx)
		return core.Result{"repositories": repositories}, err
	}
}
