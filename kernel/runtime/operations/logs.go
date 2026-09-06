package operations

import (
	"bytes"
	"context"
	"encoding/json"

	"the8020/kernel/cbus/commands/logs"
	"the8020/kernel/cbus/core"
	"the8020/kernel/logging/records"
)

func (d *Dispatcher) queryLogs(ctx context.Context, input map[string]any) (records.Page, error) {
	data, err := json.Marshal(input)
	if err != nil || len(data) > 16<<10 {
		return records.Page{}, core.NewError(core.CodeInvalidArguments, "log query exceeds its request limit")
	}
	var query records.Query
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&query); err != nil {
		return records.Page{}, core.NewError(core.CodeInvalidArguments, "invalid log query")
	}
	return logs.Query(ctx, d.services, query)
}
