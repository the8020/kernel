// Package list implements development.sandbox.list.
package list

import (
	devhandlers "the8020/kernel/cbus/commands/development/shared"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler { return devhandlers.SandboxList(serviceSet) }
