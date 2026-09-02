// Package databasecommands contains shared system-database command wiring.
package databasecommands

import (
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func Service(serviceSet *services.Services) (services.DatabaseService, error) {
	if serviceSet == nil || serviceSet.Database == nil {
		return nil, core.NewError(core.CodeDatabaseUnavailable, "system database is unavailable")
	}
	return serviceSet.Database, nil
}
