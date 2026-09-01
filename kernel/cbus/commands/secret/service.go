// Package secretcommands contains shared named-secret command wiring.
package secretcommands

import (
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func Service(serviceSet *services.Services) (services.SecretService, error) {
	if serviceSet == nil || serviceSet.Secrets == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "secret storage is unavailable")
	}
	return serviceSet.Secrets, nil
}
