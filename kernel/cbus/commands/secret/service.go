// Package secretcommands contains shared named-secret command wiring.
package secretcommands

import (
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func Service(serviceSet *services.Services) (services.SecretService, error) {
	service := serviceSet.PlatformSnapshot().Secrets
	if service == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "secret storage is unavailable")
	}
	return service, nil
}
