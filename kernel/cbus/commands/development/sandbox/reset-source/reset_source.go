// Package reset_source implements development.sandbox.reset_source.
package reset_source

import (
	devhandlers "the8020/kernel/cbus/commands/development/shared"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return devhandlers.SandboxResetSource(serviceSet)
}
