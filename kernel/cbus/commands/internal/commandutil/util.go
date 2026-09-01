// Package commandutil provides narrow shared helpers for Phase 1B handlers.
package commandutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/execution/adminrun"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

func Runtime(serviceSet *services.Services) (*services.RuntimeServices, error) {
	runtimeServices := serviceSet.RuntimeSnapshot()
	if runtimeServices == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "runtime subsystem is unavailable")
	}
	if runtimeServices.Failure != "" {
		return nil, core.NewError(core.CodeRuntimeUnavailable, runtimeServices.Failure)
	}
	return runtimeServices, nil
}

func String(request core.Request, name string) string {
	value, _ := request.Arguments[name].(string)
	return value
}

func Int(request core.Request, name string) int {
	value, _ := request.Arguments[name].(int64)
	return int(value)
}

func Bool(request core.Request, name string) bool {
	value, _ := request.Arguments[name].(bool)
	return value
}

func Has(request core.Request, name string) bool {
	_, ok := request.Arguments[name]
	return ok
}

func CSV(request core.Request, name string) []string {
	raw := String(request, name)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && !seen[part] {
			seen[part] = true
			result = append(result, part)
		}
	}
	return result
}

func JSON(request core.Request, name string) (any, error) {
	raw := String(request, name)
	if raw == "" {
		return nil, nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, core.NewError(core.CodeInvalidArguments, fmt.Sprintf("%s must be valid JSON: %v", name, err))
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, core.NewError(core.CodeInvalidArguments, name+" must contain one JSON value")
	}
	return value, nil
}

func Duration(request core.Request, name string) time.Duration {
	if !Has(request, name) {
		return 0
	}
	return time.Duration(Int(request, name)) * time.Millisecond
}

func Permissions(request core.Request) *supervisor.WorkerPermissions {
	permissions := supervisor.WorkerPermissions{
		Read: CSV(request, "read"), Write: CSV(request, "write"), Net: CSV(request, "network"),
		Import: CSV(request, "imports"), Env: CSV(request, "environment"),
	}
	if Bool(request, "system_info") {
		permissions.Sys = []string{"hostname", "osRelease"}
	}
	if len(permissions.Read)+len(permissions.Write)+len(permissions.Net)+len(permissions.Import)+len(permissions.Env)+len(permissions.Sys) == 0 {
		return nil
	}
	return &permissions
}

func OperationError(err error) error {
	if err == nil {
		return nil
	}
	var commandError *core.Error
	if errors.As(err, &commandError) {
		return err
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return core.NewError(core.CodeNotFound, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return core.NewError(core.CodeTimeout, err.Error())
	default:
		return core.NewError(core.CodeRuntimeOperation, err.Error())
	}
}

// AdministrativeExecution shapes eval/run results for their shared concise
// default view while preserving the complete internal record on request.
func AdministrativeExecution(result adminrun.Result, detail bool) core.Result {
	if detail {
		return core.Result{"execution": result}
	}
	execution := result.Execution
	response := core.Result{"state": execution.State}
	if execution.Detached {
		response["execution_id"] = execution.ExecutionID
		return response
	}
	response["result"] = execution.Result
	response["duration"] = execution.Duration.String()
	if len(execution.Logs) > 0 {
		response["logs"] = execution.Logs
	}
	return response
}

// WebServiceStatus keeps lifecycle commands concise by default while making
// the complete diagnostic status available through an explicit detail view.
func WebServiceStatus(status webservices.Status, detail bool) core.Result {
	if detail {
		return core.Result{"service": status}
	}
	return core.Result{
		"service_id":      status.ServiceID,
		"state":           status.State,
		"enabled":         status.Enabled,
		"desired_version": status.DesiredVersion,
		"loaded_version":  status.LoadedVersion,
		"version_count":   status.VersionCount,
		"sandbox_count":   status.SandboxCount,
		"worker_count":    status.WorkerCount,
	}
}
