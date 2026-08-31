// Package stop implements development.sandbox.stop.
package stop

import (
	devhandlers "the8020/kernel/cbus/commands/development/shared"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler { return devhandlers.SandboxStop(serviceSet) }
