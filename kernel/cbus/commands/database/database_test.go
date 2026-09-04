package databasecommands_test

import (
	"context"
	"errors"
	"testing"

	databasecheck "the8020/kernel/cbus/commands/database/check"
	databasesql "the8020/kernel/cbus/commands/database/sql"
	databasecompare "the8020/kernel/cbus/commands/database/table/compare"
	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	"the8020/kernel/services"
)

type fakeDatabase struct {
	status     database.Status
	checkErr   error
	statement  string
	parameters []any
	executed   bool
	compared   string
}

func (f *fakeDatabase) Status() database.Status { return f.status }
func (f *fakeDatabase) Check(context.Context) (database.Status, error) {
	return f.status, f.checkErr
}
func (f *fakeDatabase) Query(_ context.Context, statement string, parameters []any) (database.QueryResult, error) {
	f.statement, f.parameters = statement, parameters
	return database.QueryResult{Columns: []string{"value"}, Rows: [][]any{{int64(7)}}}, nil
}
func (f *fakeDatabase) Execute(_ context.Context, statement string, parameters []any) (database.ExecuteResult, error) {
	f.statement, f.parameters, f.executed = statement, parameters, true
	return database.ExecuteResult{RowsAffected: 2}, nil
}
func (f *fakeDatabase) ListTables(context.Context) ([]database.TableSummary, error) {
	return nil, nil
}
func (f *fakeDatabase) ListDefinitions(context.Context) ([]database.DefinitionSummary, error) {
	return nil, nil
}
func (f *fakeDatabase) InspectTable(context.Context, string) (database.TableDetail, error) {
	return database.TableDetail{}, nil
}
func (f *fakeDatabase) CompareTable(_ context.Context, tableID string) (database.TableDetail, error) {
	f.compared = tableID
	return database.TableDetail{TableSummary: database.TableSummary{TableID: tableID}}, nil
}

func TestTableCompareDelegatesToExplicitSourceComparison(t *testing.T) {
	fake := &fakeDatabase{}
	handler := databasecompare.New(&services.Services{Database: fake})
	result, err := handler(context.Background(), core.Request{Arguments: map[string]any{
		"table_id": "acme__orders__orders",
	}})
	detail, ok := result["table"].(database.TableDetail)
	if err != nil || !ok || detail.TableID != "acme__orders__orders" || fake.compared != detail.TableID {
		t.Fatalf("result=%#v compared=%q error=%v", result, fake.compared, err)
	}
}
func (f *fakeDatabase) SynchronizeDefinition(context.Context, string, string) (database.SynchronizationResult, error) {
	return database.SynchronizationResult{}, nil
}
func (f *fakeDatabase) SynchronizeDefinitions(context.Context, []string, bool) ([]database.SynchronizationResult, error) {
	return nil, nil
}
func (f *fakeDatabase) Trim(context.Context, string, []string, bool) error { return nil }

func TestCheckReportsConnectivityAndStructuredFailure(t *testing.T) {
	fake := &fakeDatabase{status: database.Status{Backend: database.BackendPostgreSQL, Location: "postgresql://localhost/system", State: database.StateReady}}
	handler := databasecheck.New(&services.Services{Database: fake})
	result, err := handler(context.Background(), core.Request{})
	if err != nil || result["status"] != database.StateReady || result["backend"] != database.BackendPostgreSQL {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	fake.status.State, fake.checkErr = database.StateUnavailable, errors.New("connection refused")
	_, err = handler(context.Background(), core.Request{})
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeDatabaseUnavailable || commandError.Details["location"] != fake.status.Location {
		t.Fatalf("error=%#v", err)
	}
}

func TestSQLRoutesQueryExecuteAndScalarParameters(t *testing.T) {
	fake := &fakeDatabase{}
	handler := databasesql.New(&services.Services{Database: fake})
	result, err := handler(context.Background(), core.Request{Arguments: map[string]any{
		"statement": "SELECT $1", "parameters": `[7]`,
	}})
	if err != nil || result["columns"].([]string)[0] != "value" || fake.statement != "SELECT $1" || fake.parameters[0] != int64(7) || fake.executed {
		t.Fatalf("query=%#v fake=%#v error=%v", result, fake, err)
	}
	result, err = handler(context.Background(), core.Request{Arguments: map[string]any{
		"statement": "DELETE FROM example", "execute": true,
	}})
	if err != nil || result["rows_affected"] != int64(2) || !fake.executed {
		t.Fatalf("execute=%#v fake=%#v error=%v", result, fake, err)
	}
	_, err = handler(context.Background(), core.Request{Arguments: map[string]any{
		"statement": "SELECT $1", "parameters": `[{}]`,
	}})
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeInvalidArguments {
		t.Fatalf("parameter error=%#v", err)
	}
}
