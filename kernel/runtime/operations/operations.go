// Package operations adapts trusted runtime calls to kernel primitives.
package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	databasecheck "the8020/kernel/cbus/commands/database/check"
	databasesql "the8020/kernel/cbus/commands/database/sql"
	databasecompare "the8020/kernel/cbus/commands/database/table/compare"
	databasedefinitions "the8020/kernel/cbus/commands/database/table/definitions"
	databaseinspect "the8020/kernel/cbus/commands/database/table/inspect"
	databaselist "the8020/kernel/cbus/commands/database/table/list"
	databasesync "the8020/kernel/cbus/commands/database/table/sync"
	databasesyncall "the8020/kernel/cbus/commands/database/table/syncall"
	databasetrim "the8020/kernel/cbus/commands/database/table/trim"
	activatepreview "the8020/kernel/cbus/commands/development/activate/preview"
	activaterun "the8020/kernel/cbus/commands/development/activate/run"
	developmentimage "the8020/kernel/cbus/commands/development/image/status"
	developmentcreate "the8020/kernel/cbus/commands/development/sandbox/create"
	developmentdelete "the8020/kernel/cbus/commands/development/sandbox/delete"
	developmentfactoryreset "the8020/kernel/cbus/commands/development/sandbox/factory-reset"
	developmentinspect "the8020/kernel/cbus/commands/development/sandbox/inspect"
	developmentkill "the8020/kernel/cbus/commands/development/sandbox/kill"
	developmentlist "the8020/kernel/cbus/commands/development/sandbox/list"
	developmentresetsource "the8020/kernel/cbus/commands/development/sandbox/reset-source"
	developmentrestart "the8020/kernel/cbus/commands/development/sandbox/restart"
	developmentshell "the8020/kernel/cbus/commands/development/sandbox/shell"
	developmentstart "the8020/kernel/cbus/commands/development/sandbox/start"
	developmentstop "the8020/kernel/cbus/commands/development/sandbox/stop"
	nodelist "the8020/kernel/cbus/commands/node/list"
	noderemove "the8020/kernel/cbus/commands/node/remove"
	nodeset "the8020/kernel/cbus/commands/node/set"
	packageinspectindex "the8020/kernel/cbus/commands/package/index/inspect"
	packagelistindex "the8020/kernel/cbus/commands/package/index/list"
	packagesetindex "the8020/kernel/cbus/commands/package/index/set"
	packageinspect "the8020/kernel/cbus/commands/package/inspect"
	packagelist "the8020/kernel/cbus/commands/package/list"
	packagelocalcreate "the8020/kernel/cbus/commands/package/local/create"
	packagerepositorycheckout "the8020/kernel/cbus/commands/package/repository/checkout"
	packagerepositoryinit "the8020/kernel/cbus/commands/package/repository/init"
	packagerepositoryinspect "the8020/kernel/cbus/commands/package/repository/inspect"
	packagerepositorylist "the8020/kernel/cbus/commands/package/repository/list"
	packagerepositorypull "the8020/kernel/cbus/commands/package/repository/pull"
	packagerepositorypush "the8020/kernel/cbus/commands/package/repository/push"
	packagerepositoryremote "the8020/kernel/cbus/commands/package/repository/remote"
	packagerepositorystatus "the8020/kernel/cbus/commands/package/repository/status"
	packagesourceinspect "the8020/kernel/cbus/commands/package/source/inspect"
	packagesynchronize "the8020/kernel/cbus/commands/package/synchronize"
	packageversionlist "the8020/kernel/cbus/commands/package/version/list"
	secretget "the8020/kernel/cbus/commands/secret/get"
	secretlist "the8020/kernel/cbus/commands/secret/list"
	secretset "the8020/kernel/cbus/commands/secret/set"
	serviceinspect "the8020/kernel/cbus/commands/service/inspect"
	servicelist "the8020/kernel/cbus/commands/service/list"
	serviceopenapi "the8020/kernel/cbus/commands/service/openapi"
	servicerefresh "the8020/kernel/cbus/commands/service/refresh"
	servicerequest "the8020/kernel/cbus/commands/service/request"
	servicevalidate "the8020/kernel/cbus/commands/service/validate"
	commandcore "the8020/kernel/cbus/core"
	"the8020/kernel/services"
	"the8020/kernel/settings"
)

type Dispatcher struct {
	services *services.Services
	handlers map[string]commandcore.Handler
}

func New(serviceSet *services.Services) (*Dispatcher, error) {
	if serviceSet == nil || serviceSet.Signing == nil {
		return nil, errors.New("runtime operation services and signer are required")
	}
	return &Dispatcher{services: serviceSet, handlers: map[string]commandcore.Handler{
		"database.check": databasecheck.New(serviceSet), "database.sql": databasesql.New(serviceSet),
		"database.table.compare": databasecompare.New(serviceSet), "database.table.definitions": databasedefinitions.New(serviceSet),
		"database.table.inspect": databaseinspect.New(serviceSet), "database.table.list": databaselist.New(serviceSet),
		"database.table.sync": databasesync.New(serviceSet), "database.table.sync_all": databasesyncall.New(serviceSet), "database.table.trim": databasetrim.New(serviceSet),
		"development.activate.preview": activatepreview.New(serviceSet), "development.activate.run": activaterun.New(serviceSet),
		"development.image.status": developmentimage.New(serviceSet), "development.sandbox.create": developmentcreate.New(serviceSet),
		"development.sandbox.delete": developmentdelete.New(serviceSet), "development.sandbox.factory_reset": developmentfactoryreset.New(serviceSet),
		"development.sandbox.inspect": developmentinspect.New(serviceSet), "development.sandbox.kill": developmentkill.New(serviceSet),
		"development.sandbox.list": developmentlist.New(serviceSet), "development.sandbox.reset_source": developmentresetsource.New(serviceSet),
		"development.sandbox.restart": developmentrestart.New(serviceSet), "development.sandbox.shell": developmentshell.New(serviceSet),
		"development.sandbox.start": developmentstart.New(serviceSet), "development.sandbox.stop": developmentstop.New(serviceSet),
		"node.list": nodelist.New(serviceSet), "node.remove": noderemove.New(serviceSet), "node.set": nodeset.New(serviceSet),
		"package.index.inspect": packageinspectindex.New(serviceSet), "package.index.list": packagelistindex.New(serviceSet), "package.index.set": packagesetindex.New(serviceSet),
		"package.inspect": packageinspect.New(serviceSet), "package.list": packagelist.New(serviceSet), "package.local.create": packagelocalcreate.New(serviceSet),
		"package.repository.checkout": packagerepositorycheckout.New(serviceSet), "package.repository.init": packagerepositoryinit.New(serviceSet),
		"package.repository.inspect": packagerepositoryinspect.New(serviceSet), "package.repository.list": packagerepositorylist.New(serviceSet),
		"package.repository.pull": packagerepositorypull.New(serviceSet), "package.repository.push": packagerepositorypush.New(serviceSet),
		"package.repository.remote": packagerepositoryremote.New(serviceSet), "package.repository.status": packagerepositorystatus.New(serviceSet),
		"package.source.inspect": packagesourceinspect.New(serviceSet), "package.synchronize": packagesynchronize.New(serviceSet), "package.version.list": packageversionlist.New(serviceSet),
		"secret.get": secretget.New(serviceSet), "secret.list": secretlist.New(serviceSet), "secret.set": secretset.New(serviceSet),
		"service.inspect": serviceinspect.New(serviceSet), "service.list": servicelist.New(serviceSet), "service.openapi": serviceopenapi.New(serviceSet), "service.refresh": servicerefresh.New(serviceSet),
		"service.request":  servicerequest.New(serviceSet),
		"service.validate": servicevalidate.New(serviceSet),
	}}, nil
}

func (d *Dispatcher) Execute(ctx context.Context, operation string, input map[string]any) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = commandcore.NewError(commandcore.CodeInvalidArguments, fmt.Sprintf("invalid %s operation input", operation))
		}
	}()
	if strings.HasPrefix(operation, "crypto.") {
		return d.crypto(operation, input)
	}
	if strings.HasPrefix(operation, "settings.global.") {
		return d.settings(ctx, settings.StorageGlobal, strings.TrimPrefix(operation, "settings.global."), input)
	}
	if strings.HasPrefix(operation, "settings.node.") {
		return d.settings(ctx, settings.StorageNode, strings.TrimPrefix(operation, "settings.node."), input)
	}
	if operation == "event.emit" || operation == "program.run" {
		return d.execution(ctx, operation, input)
	}
	if operation == "program.list" {
		runtime := d.services.RuntimeSnapshot()
		if runtime == nil || runtime.ListPrograms == nil {
			return nil, errors.New("program catalog is unavailable")
		}
		return runtime.ListPrograms(ctx)
	}
	handler := d.handlers[operation]
	if handler == nil {
		return nil, fmt.Errorf("unknown runtime operation %q", operation)
	}
	arguments := make(map[string]any, len(input))
	for key, value := range input {
		arguments[key] = value
	}
	return handler(ctx, commandcore.Request{ProtocolVersion: commandcore.ProtocolVersion, CommandID: operation, Arguments: arguments})
}

func (d *Dispatcher) settings(ctx context.Context, storage settings.Storage, action string, input map[string]any) (any, error) {
	if action == "list" {
		values := d.services.Settings.List()
		filtered := make([]settings.Info, 0, len(values))
		for _, value := range values {
			if value.Storage == storage {
				filtered = append(filtered, value)
			}
		}
		return map[string]any{"settings": filtered}, nil
	}
	key, ok := input["key"].(string)
	if !ok || key == "" {
		return nil, errors.New("setting key is required")
	}
	current, err := d.services.Settings.Get(key)
	if err != nil {
		return nil, err
	}
	if current.Storage != storage {
		return nil, fmt.Errorf("setting %s is stored in %s scope", key, current.Storage)
	}
	switch action {
	case "get":
		return map[string]any{"setting": current}, nil
	case "set":
		value, ok := input["value"].(string)
		if !ok {
			return nil, errors.New("setting value must be a string")
		}
		updated, err := d.services.Settings.Set(ctx, key, value)
		return map[string]any{"setting": updated}, err
	case "unset":
		updated, err := d.services.Settings.Unset(ctx, key)
		return map[string]any{"setting": updated}, err
	default:
		return nil, fmt.Errorf("unknown settings operation %q", action)
	}
}
