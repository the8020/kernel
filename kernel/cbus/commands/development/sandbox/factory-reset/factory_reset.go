// Package factory_reset implements development.sandbox.factory_reset.
package factory_reset

import (
	devhandlers "the8020/kernel/cbus/commands/development/shared"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return devhandlers.SandboxFactoryReset(serviceSet)
}
