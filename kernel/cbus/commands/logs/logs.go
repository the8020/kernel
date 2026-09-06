// Package logs adapts bounded node log queries to the command bus.
package logs

import (
	"context"
	"time"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/logging/records"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		q := records.Query{Filter: records.Filter{
			Level: commandutil.String(request, "level"), Source: commandutil.String(request, "source"),
			NodeID: commandutil.String(request, "node_id"), SandboxID: commandutil.String(request, "sandbox_id"),
			WorkerID: commandutil.String(request, "worker_id"), ContextID: commandutil.String(request, "context_id"),
			ParentContextID: commandutil.String(request, "parent_context_id"), JobID: commandutil.String(request, "job_id"),
			ServiceID: commandutil.String(request, "service_id"), PersistentID: commandutil.String(request, "persistent_id"),
			Object: commandutil.String(request, "object"), Username: commandutil.String(request, "username"),
		}, Limit: commandutil.Int(request, "limit"), Position: commandutil.String(request, "position"), Cursor: commandutil.String(request, "cursor"), Tail: commandutil.Bool(request, "tail")}
		for key, target := range map[string]*time.Time{"from": &q.From, "until": &q.Until} {
			if text := commandutil.String(request, key); text != "" {
				parsed, err := time.Parse(time.RFC3339Nano, text)
				if err != nil {
					return nil, core.NewError(core.CodeInvalidArguments, key+" must be an RFC3339 timestamp")
				}
				*target = parsed
			}
		}
		page, err := Query(ctx, serviceSet, q)
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"page": page}, nil
	}
}

// Query is shared by command and SDK adapters. Local reads remain available
// before runtime/topology startup; a remote target uses the existing node owner.
func Query(ctx context.Context, serviceSet *services.Services, query records.Query) (records.Page, error) {
	if query.NodeID == "" && serviceSet.Logging != nil {
		query.NodeID = serviceSet.Logging.NodeID()
	}
	query, err := query.Normalize()
	if err != nil {
		return records.Page{}, core.NewError(core.CodeInvalidArguments, err.Error())
	}
	if serviceSet.Logging != nil && query.NodeID == serviceSet.Logging.NodeID() {
		return serviceSet.Logging.Query(ctx, query)
	}
	if nodes := serviceSet.PlatformSnapshot().Nodes; nodes != nil {
		return nodes.QueryLogs(ctx, query)
	}
	return records.Page{State: "unavailable", Reason: "The node that owns these logs is unavailable.", Records: []records.LocatedRecord{}}, nil
}
