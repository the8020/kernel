package database_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	. "the8020/kernel/database"
	"the8020/kernel/database/schematest"
	"time"
)

func TestCommittedSchemaRetryRetainsOriginalConsent(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		t.Run(backend, func(t *testing.T) {
			mutations := 0
			m := evolutionDatabase(t, backend, func(request StatementRequest) {
				statement := strings.ToUpper(strings.TrimSpace(request.Statement))
				if strings.Contains(statement, "ACME__ORDERS__ORDERS") && !strings.HasPrefix(statement, "SELECT") && !strings.HasPrefix(statement, "PRAGMA") && !strings.HasPrefix(statement, "LOCK ") {
					mutations++
				}
			})
			defer m.Close()
			ctx := t.Context()
			if _, err := m.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			old := evaluatedTable(t, testDescriptor())
			if _, err := m.Synchronize(ctx, []EvaluatedTable{old}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := m.CompleteInitialization(ctx, map[string]string{"acme/orders": old.SourceCommit}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders(id,status,total) VALUES(7,'draft',123),(9,'confirmed',456)`); err != nil {
				t.Fatal(err)
			}
			d := testDescriptor()
			d.Columns[1].EnumValues = []string{"confirmed"}
			d.Columns[1].Default.Value = "confirmed"
			next := evaluatedTable(t, d)
			next.SourceCommit = strings.Repeat("b", 40)
			encoded, err := m.PlanSchema(ctx, []EvaluatedTable{next})
			if err != nil {
				t.Fatal(err)
			}
			var plan struct{ Identity string }
			if err := json.Unmarshal(encoded, &plan); err != nil || plan.Identity == "" {
				t.Fatalf("preview identity: %s %v", encoded, err)
			}
			const id = "act-retry00001"
			if _, err := m.BeginDeployment(ctx, id, []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: next.SourceCommit}}); err != nil {
				t.Fatal(err)
			}
			options := SynchronizationOptions{ActivationID: id, ForceColumns: []string{d.TableID + ".status"}, ApprovedPlan: plan.Identity}
			stale := options
			stale.ApprovedPlan = "stale"
			if _, err := m.Synchronize(ctx, []EvaluatedTable{next}, stale); err == nil {
				t.Fatal("uncommitted change accepted stale consent")
			}
			results, err := m.Synchronize(ctx, []EvaluatedTable{next}, options)
			if err != nil || len(results) != 1 || results[0].Route != "repair" {
				t.Fatalf("initial repair: %+v %v", results, err)
			}
			pending, exists, err := m.PendingDeploymentFor(ctx, id)
			if err != nil || !exists || len(pending.Rewrites) != 1 {
				t.Fatalf("committed receipt: %+v %v", pending, err)
			}
			// A lost response must return the committed outcome with either original consent or recovery options.
			before := mutations
			for _, retry := range []SynchronizationOptions{options, {ActivationID: id, ApprovedPlan: plan.Identity}, {ActivationID: id}} {
				replayed, err := m.Synchronize(ctx, []EvaluatedTable{next}, retry)
				if err != nil || len(replayed) != 1 || replayed[0] != results[0] {
					t.Errorf("committed retry: %+v %v; want %+v", replayed, err, results)
				}
			}
			if mutations != before {
				t.Fatal("committed retry executed application mutation or DDL")
			}
			wrongCommit := next
			wrongCommit.SourceCommit = strings.Repeat("c", 40)
			if _, err := m.Synchronize(ctx, []EvaluatedTable{wrongCommit}, options); err == nil {
				t.Fatal("committed receipt authorized another candidate")
			}
			wrongOwner := next
			wrongOwner.SourceModule += ".other.ts"
			if _, err := m.Synchronize(ctx, []EvaluatedTable{wrongOwner}, options); err == nil {
				t.Fatal("committed receipt bypassed source ownership")
			}
			changed := testDescriptor()
			changed.Columns[1].EnumValues = []string{"confirmed", "other"}
			changed.Columns[1].Default.Value = "confirmed"
			wrongTarget := evaluatedTable(t, changed)
			wrongTarget.SourceCommit = next.SourceCommit
			if _, err := m.Synchronize(ctx, []EvaluatedTable{wrongTarget}, SynchronizationOptions{ActivationID: id}); err == nil {
				t.Fatal("committed receipt authorized another descriptor")
			}
			if _, err := m.ExecContext(ctx, `DROP INDEX orders_status`); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{next}, options); err == nil {
				t.Fatal("committed receipt hid physical drift")
			}
			var kept int
			if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE (id=7 AND status='confirmed' AND total=123) OR (id=9 AND status='confirmed' AND total=456)`).Scan(&kept); err != nil || kept != 2 {
				t.Fatalf("committed values changed: %d %v", kept, err)
			}
		})
	}
}

func TestSQLiteFloatTextConversionPreservesPrecisionAndNull(t *testing.T) {
	m := evolutionDatabase(t, BackendSQLite)
	defer m.Close()
	ctx := t.Context()
	if _, err := m.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	old := testDescriptor()
	old.Columns[1].LogicalType, old.Columns[1].EnumValues, old.Columns[1].Nullable = "float", nil, true
	old.Columns[1].Default.Value = 1
	if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, old)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	const original = 1.2345678901234567
	if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total) VALUES (7,$1,123),(9,NULL,456)`, original); err != nil {
		t.Fatal(err)
	}
	next := testDescriptor()
	next.Columns[1].LogicalType, next.Columns[1].EnumValues, next.Columns[1].Nullable = "text", nil, true
	next.Columns[1].Default.Value = "1"
	if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, next)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := m.QueryRowContext(ctx, `SELECT status FROM acme__orders__orders WHERE id=7`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	decoded, err := strconv.ParseFloat(value, 64)
	if err != nil || decoded != original {
		t.Fatalf("float precision lost: %s %v", value, err)
	}
	var count int
	if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE id=9 AND status IS NULL AND total=456`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("NULL changed: %d %v", count, err)
	}
}

func evolutionDatabase(t *testing.T, backend string, observe ...func(StatementRequest)) *Manager {
	t.Helper()
	config := sqliteConfig(filepath.Join(t.TempDir(), "evolution.db"))
	if backend == BackendPostgreSQL {
		config.Backend, config.Location = backend, os.Getenv("THE8020_TEST_POSTGRES_LOCATION")
		if config.Location == "" {
			t.Skip("requires a disposable PostgreSQL database")
		}
		config.Username = os.Getenv("THE8020_TEST_POSTGRES_USERNAME")
		schema := fmt.Sprintf("evolution_%d", time.Now().UnixNano())
		base := New(config)
		if _, err := base.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
			t.Fatal(err)
		}
		if err := base.Close(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup := New(config)
			defer cleanup.Close()
			if _, err := cleanup.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
				t.Error(err)
			}
		})
		location, err := url.Parse(config.Location)
		if err != nil {
			t.Fatal(err)
		}
		query := location.Query()
		query.Set("options", "-csearch_path="+schema)
		location.RawQuery = query.Encode()
		config.Location = location.String()
	}
	m := New(config)
	schematest.Attach(t, m, observe...)
	return m
}

func TestIndexChangesGateCleanupAndRollback(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		for _, scenario := range []string{"remove-nonunique", "add-nonunique", "replace-nonunique", "relax-unique", "add-unique", "tighten-unique", "unique-ddl-rollback"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				deletes := 0
				createdUnique := false
				m := evolutionDatabase(t, backend, func(request StatementRequest) {
					statement := strings.ToUpper(strings.TrimSpace(request.Statement))
					if strings.HasPrefix(statement, "DELETE FROM \"ACME__ORDERS__ORDERS\"") {
						deletes++
					}
					if strings.HasPrefix(statement, "CREATE UNIQUE INDEX IF NOT EXISTS \"ORDERS_TOTAL_UNIQUE\"") {
						createdUnique = true
					}
				})
				defer m.Close()
				ctx := t.Context()
				if _, err := m.InitializeCatalog(ctx); err != nil {
					t.Fatal(err)
				}
				old := testDescriptor()
				old.Columns = append(old.Columns, ColumnDescriptor{Name: "marker", LogicalType: "text", Default: &DefaultDescriptor{Kind: "literal", Value: "delete"}})
				if scenario == "relax-unique" || scenario == "tighten-unique" {
					old.Indexes = []IndexDescriptor{{Name: "orders_total", Columns: []string{"total"}, Unique: scenario == "relax-unique"}}
				}
				module, err := filepath.Abs("../../../db/testdata/migration_table.ts")
				if err != nil {
					t.Fatal(err)
				}
				initial := evaluatedTable(t, old)
				initial.SourceModule = module
				if _, err := m.Synchronize(ctx, []EvaluatedTable{initial}, SynchronizationOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := m.CompleteInitialization(ctx, map[string]string{"acme/orders": initial.SourceCommit}); err != nil {
					t.Fatal(err)
				}
				secondTotal := 123
				if scenario == "relax-unique" {
					secondTotal = 456
				}
				if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total,metadata) VALUES (7,'draft',123,'{"kept":7}'),(9,'confirmed',$1,'{"kept":9}')`, secondTotal); err != nil {
					t.Fatal(err)
				}
				nextDescriptor := old
				switch scenario {
				case "remove-nonunique":
					nextDescriptor.Indexes = nil
				case "add-nonunique":
					nextDescriptor.Indexes = append(append([]IndexDescriptor(nil), old.Indexes...), IndexDescriptor{Name: "orders_total", Columns: []string{"total"}})
				case "replace-nonunique":
					nextDescriptor.Indexes = []IndexDescriptor{{Name: "orders_status", Columns: []string{"total"}}}
				case "relax-unique", "tighten-unique":
					nextDescriptor.Indexes = []IndexDescriptor{{Name: "orders_total", Columns: []string{"total"}, Unique: scenario == "tighten-unique"}}
				default:
					nextDescriptor.Indexes = append(append([]IndexDescriptor(nil), old.Indexes...), IndexDescriptor{Name: "orders_total_unique", Columns: []string{"total"}, Unique: true})
				}
				rollback := scenario == "unique-ddl-rollback"
				if rollback {
					if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total) VALUES (11,'confirmed',456)`); err != nil {
						t.Fatal(err)
					}
					nextDescriptor.Indexes = append(nextDescriptor.Indexes, IndexDescriptor{Name: "zz_marker_unique", Columns: []string{"marker"}, Unique: true})
				}
				next := evaluatedTable(t, nextDescriptor)
				next.SourceModule, next.SourceCommit, next.HasMigrate = module, strings.Repeat("b", 40), true
				const id = "act-indexgate1"
				if _, err := m.BeginDeployment(ctx, id, []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: next.SourceCommit}}); err != nil {
					t.Fatal(err)
				}
				_, changeErr := m.Synchronize(ctx, []EvaluatedTable{next}, SynchronizationOptions{ActivationID: id})
				if rollback {
					if changeErr == nil || !strings.Contains(strings.ToLower(changeErr.Error()), "unique") {
						t.Fatalf("expected later unique DDL failure: %v", changeErr)
					}
					if !createdUnique {
						t.Fatal("did not reach successful unique DDL before later failure")
					}
				} else if changeErr != nil {
					t.Fatal(changeErr)
				}
				cleanup := scenario == "add-unique" || scenario == "tighten-unique" || rollback
				if cleanup && deletes != 1 || !cleanup && deletes != 0 {
					t.Fatalf("cleanup deletes=%d, expected cleanup=%v", deletes, cleanup)
				}
				var rows, preserved int
				wantRows := 2
				if cleanup {
					wantRows = 1
				}
				if rollback {
					wantRows = 3
				}
				if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders`).Scan(&rows); err != nil || rows != wantRows {
					t.Fatalf("rows=%d, want %d: %v", rows, wantRows, err)
				}
				if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE id=9 AND status='confirmed' AND total=$1 AND metadata IS NOT NULL AND marker='delete'`, secondTotal).Scan(&preserved); err != nil || preserved != 1 {
					t.Fatalf("unrelated values changed: %d %v", preserved, err)
				}
				detail, err := m.InspectTable(ctx, old.TableID)
				if err != nil || len(detail.Differences) != 0 {
					t.Fatalf("physical/catalog mismatch: %#v %v", detail, err)
				}
				wantCommit := next.SourceCommit
				if rollback {
					wantCommit = initial.SourceCommit
				}
				if detail.SourceCommit != wantCommit {
					t.Fatalf("catalog commit=%s, want %s", detail.SourceCommit, wantCommit)
				}
				if !rollback {
					if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (status,total) VALUES ('draft',$1)`, secondTotal); (err != nil) != cleanup {
						t.Fatalf("target uniqueness enforcement: %v", err)
					}
				}
			})
		}
	}
}

func TestEnumEvolutionPreservesStorage(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		t.Run(backend, func(t *testing.T) {
			m := evolutionDatabase(t, backend)
			defer m.Close()
			ctx := context.Background()
			if _, err := m.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			original := testDescriptor()
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, original)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total,metadata) VALUES (7,'draft',123,'{"kept":true}')`); err != nil {
				t.Fatal(err)
			}
			retired := testDescriptor()
			retired.Columns = retired.Columns[:3]
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, retired)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			widened := retired
			widened.Columns = append([]ColumnDescriptor(nil), retired.Columns...)
			widened.Columns[1].EnumValues = []string{"confirmed", "draft", "cancelled"}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, widened)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			var total, id int64
			var metadata any
			if err := m.QueryRowContext(ctx, `SELECT id,total,metadata FROM acme__orders__orders WHERE id=7`).Scan(&id, &total, &metadata); err != nil || id != 7 || total != 123 || metadata == nil {
				t.Fatalf("preserved row: %d %d %v, %v", id, total, metadata, err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (status,total) VALUES ('cancelled',456)`); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (status,total) VALUES ('invalid',0)`); err == nil {
				t.Fatal("widening removed enum enforcement")
			}
			detail, err := m.InspectTable(ctx, original.TableID)
			if err != nil || len(detail.Differences) != 0 || detail.RetiredColumns != 1 {
				t.Fatalf("widened catalog: %#v, %v", detail, err)
			}
			text := widened
			text.Columns = append([]ColumnDescriptor(nil), widened.Columns...)
			text.Columns[1].LogicalType, text.Columns[1].EnumValues = "text", nil
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, text)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (status,total) VALUES ('open text',789)`); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders`).Scan(&count); err != nil || count != 3 {
				t.Fatalf("count=%d: %v", count, err)
			}
			detail, err = m.InspectTable(ctx, original.TableID)
			if err != nil || len(detail.Differences) != 0 || len(detail.PhysicalIndexes) != 1 {
				t.Fatalf("text catalog: %#v, %v", detail, err)
			}
		})
	}
}

func TestCleanupAndMinimalColumnRepair(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		t.Run(backend, func(t *testing.T) {
			m := evolutionDatabase(t, backend)
			defer m.Close()
			ctx := t.Context()
			if _, err := m.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			module, err := filepath.Abs("../../../db/testdata/migration_table.ts")
			if err != nil {
				t.Fatal(err)
			}
			initial := evaluatedTable(t, testDescriptor())
			initial.SourceModule = module
			if _, err := m.Synchronize(ctx, []EvaluatedTable{initial}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := m.CompleteInitialization(ctx, map[string]string{"acme/orders": initial.SourceCommit}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total,metadata) VALUES (7,'draft',123,'{"kept":true}'),(9,'confirmed',456,'{"other":true}')`); err != nil {
				t.Fatal(err)
			}
			target := testDescriptor()
			target.Columns[1].EnumValues = []string{"confirmed"}
			target.Columns[1].Default.Value = "confirmed"
			target.Columns = append(target.Columns, ColumnDescriptor{Name: "marker", LogicalType: "text", Default: &DefaultDescriptor{Kind: "literal", Value: "delete"}})
			next := evaluatedTable(t, target)
			next.SourceModule = module
			next.SourceCommit = strings.Repeat("b", 40)
			next.HasMigrate = true
			const id = "act-cleanup001"
			if _, err := m.BeginDeployment(ctx, id, []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: next.SourceCommit}}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{next}, SynchronizationOptions{ActivationID: id}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE id=7`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("authored delete: %d %v", count, err)
			}
			if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE id=9 AND total=456`).Scan(&count); err != nil || count != 1 {
				t.Fatal("unrelated row changed", err)
			}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{next}, SynchronizationOptions{ActivationID: id}); err != nil {
				t.Fatal(err)
			}
			if err := m.CompleteDeployment(ctx, id, true); err != nil {
				t.Fatal(err)
			}
			repair := testDescriptor()
			repair.Columns[1].EnumValues = []string{"open", "confirmed"}
			repair.Columns[1].Default.Value = "open"
			repair.Columns = append(repair.Columns, ColumnDescriptor{Name: "marker", LogicalType: "text", Default: &DefaultDescriptor{Kind: "literal", Value: "throw"}})
			// Introduce a second legacy value via a safe widening first.
			wide := target
			wide.Columns = append([]ColumnDescriptor(nil), target.Columns...)
			wide.Columns[1].EnumValues = []string{"confirmed", "draft"}
			w := evaluatedTable(t, wide)
			w.SourceModule = module
			if _, err := m.Synchronize(ctx, []EvaluatedTable{w}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total,metadata) VALUES (11,'draft',789,'{"repair":true}')`); err != nil {
				t.Fatal(err)
			}
			n := evaluatedTable(t, repair)
			n.SourceModule = module
			n.SourceCommit = strings.Repeat("c", 40)
			n.HasMigrate = true
			const repairID = "act-repair0001"
			if _, err := m.BeginDeployment(ctx, repairID, []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: n.SourceCommit}}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{n}, SynchronizationOptions{ActivationID: repairID}); err == nil {
				t.Fatal("throwing cleanup succeeded")
			}
			if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE id=11 AND status='draft'`).Scan(&count); err != nil || count != 1 {
				t.Fatal("cleanup failed to roll back", err)
			}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{n}, SynchronizationOptions{ActivationID: repairID, ForceColumns: []string{repair.TableID}}); err == nil {
				t.Fatal("broad table consent accepted")
			}
			results, err := m.Synchronize(ctx, []EvaluatedTable{n}, SynchronizationOptions{ActivationID: repairID, ForceColumns: []string{repair.TableID + ".status"}})
			if err != nil || results[0].Route != "repair" {
				t.Fatalf("minimal repair: %+v %v", results, err)
			}
			if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE (id=9 AND status='confirmed' AND total=456) OR (id=11 AND status='open' AND total=789)`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("valid/unrelated data lost: %d %v", count, err)
			}
		})
	}
}

func TestCleanupDeletionRollsBackAfterDDLFailure(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		t.Run(backend, func(t *testing.T) {
			m := evolutionDatabase(t, backend)
			defer m.Close()
			ctx := t.Context()
			if _, err := m.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			module, _ := filepath.Abs("../../../db/testdata/migration_table.ts")
			initial := evaluatedTable(t, testDescriptor())
			initial.SourceModule = module
			if _, err := m.Synchronize(ctx, []EvaluatedTable{initial}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := m.CompleteInitialization(ctx, map[string]string{"acme/orders": initial.SourceCommit}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total) VALUES (7,'draft',123),(9,'confirmed',456)`); err != nil {
				t.Fatal(err)
			}
			d := testDescriptor()
			d.Columns[1].EnumValues = []string{"confirmed"}
			d.Columns[1].Default.Value = "confirmed"
			d.Columns = append(d.Columns, ColumnDescriptor{Name: "marker", LogicalType: "text", Default: &DefaultDescriptor{Kind: "literal", Value: "delete-fail"}}, ColumnDescriptor{Name: "required", LogicalType: "text"})
			next := evaluatedTable(t, d)
			next.SourceModule = module
			next.SourceCommit = strings.Repeat("b", 40)
			next.HasMigrate = true
			const id = "act-ddlfail001"
			if _, err := m.BeginDeployment(ctx, id, []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: next.SourceCommit}}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{next}, SynchronizationOptions{ActivationID: id}); err == nil {
				t.Fatal("invalid DDL succeeded")
			}
			var count int
			if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("authored deletion survived rollback: %d %v", count, err)
			}
		})
	}
}

func TestNativeConversionsAndRefusal(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		t.Run(backend, func(t *testing.T) {
			m := evolutionDatabase(t, backend)
			defer m.Close()
			ctx := t.Context()
			if _, err := m.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			old := testDescriptor()
			old.Columns[1].LogicalType = "integer"
			old.Columns[1].EnumValues = nil
			old.Columns[1].Default.Value = 1
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, old)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total) VALUES (7,23,123)`); err != nil {
				t.Fatal(err)
			}
			text := old
			text.Columns = append([]ColumnDescriptor(nil), old.Columns...)
			text.Columns[1].LogicalType = "text"
			text.Columns[1].Default = &DefaultDescriptor{Kind: "literal", Value: "1"}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, text)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			var value string
			if err := m.QueryRowContext(ctx, `SELECT status FROM acme__orders__orders WHERE id=7`).Scan(&value); err != nil || value != "23" {
				t.Fatalf("native text conversion %s %v", value, err)
			}
			if _, err := m.ExecContext(ctx, `UPDATE acme__orders__orders SET status='bad' WHERE id=7`); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, old)}, SynchronizationOptions{}); err == nil {
				t.Fatal("bad numeric text silently cast")
			}
			if err := m.QueryRowContext(ctx, `SELECT status FROM acme__orders__orders WHERE id=7`).Scan(&value); err != nil || value != "bad" {
				t.Fatalf("failed conversion changed data %s %v", value, err)
			}
		})
	}
}

func TestCleanupCancellationNeverRepairs(t *testing.T) {
	m := New(sqliteConfig(filepath.Join(t.TempDir(), "cancel.db")))
	defer m.Close()
	entered := make(chan struct{})
	var signal sync.Once
	schematest.Attach(t, m, func(r StatementRequest) {
		if strings.Contains(r.Statement, "migration_pause") {
			signal.Do(func() { close(entered) })
		}
	})
	ctx := t.Context()
	if _, err := m.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	module, _ := filepath.Abs("../../../db/testdata/migration_table.ts")
	initial := evaluatedTable(t, testDescriptor())
	initial.SourceModule = module
	if _, err := m.Synchronize(ctx, []EvaluatedTable{initial}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteInitialization(ctx, map[string]string{"acme/orders": initial.SourceCommit}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total) VALUES (7,'draft',123)`); err != nil {
		t.Fatal(err)
	}
	d := testDescriptor()
	d.Columns[1].EnumValues = []string{"confirmed"}
	d.Columns[1].Default.Value = "confirmed"
	d.Columns = append(d.Columns, ColumnDescriptor{Name: "marker", LogicalType: "text", Default: &DefaultDescriptor{Kind: "literal", Value: "wait"}})
	next := evaluatedTable(t, d)
	next.SourceModule = module
	next.SourceCommit = strings.Repeat("b", 40)
	next.HasMigrate = true
	const id = "act-cancel0001"
	if _, err := m.BeginDeployment(ctx, id, []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: next.SourceCommit}}); err != nil {
		t.Fatal(err)
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.Synchronize(work, []EvaluatedTable{next}, SynchronizationOptions{ActivationID: id, ForceColumns: []string{d.TableID + ".status"}})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("cleanup did not enter: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not enter")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not roll back")
	}
	var count int
	if err := m.QueryRowContext(ctx, `SELECT COUNT(*) FROM acme__orders__orders WHERE id=7 AND status='draft'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("cancel altered row %d %v", count, err)
	}
}

func TestColumnRepairLegalityAndNoEscalation(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		for _, scenario := range []string{"null", "required", "unique", "unsupported", "key"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				m := evolutionDatabase(t, backend)
				defer m.Close()
				ctx := t.Context()
				if _, err := m.InitializeCatalog(ctx); err != nil {
					t.Fatal(err)
				}
				old := testDescriptor()
				if scenario == "unsupported" {
					old.Columns[1].LogicalType = "text"
					old.Columns[1].EnumValues = nil
				}
				initial := evaluatedTable(t, old)
				if _, err := m.Synchronize(ctx, []EvaluatedTable{initial}, SynchronizationOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := m.CompleteInitialization(ctx, map[string]string{"acme/orders": initial.SourceCommit}); err != nil {
					t.Fatal(err)
				}
				if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total) VALUES (7,'draft',123),(9,'confirmed',456)`); err != nil {
					t.Fatal(err)
				}
				d := testDescriptor()
				d.Columns[1].EnumValues = []string{"confirmed"}
				d.Columns[1].Default = nil
				scope := d.TableID + ".status"
				switch scenario {
				case "null":
					d.Columns[1].Nullable = true
				case "unique":
					d.Columns[1].Default = &DefaultDescriptor{Kind: "literal", Value: "confirmed"}
					d.Indexes[0].Unique = true
				case "unsupported":
					d.Columns[1].LogicalType = "integer"
					d.Columns[1].EnumValues = nil
					d.Columns[1].Nullable = true
				case "key":
					d.Columns[0].LogicalType = "text"
					d.Columns[0].Generated = false
					scope = d.TableID + ".id"
				}
				next := evaluatedTable(t, d)
				next.SourceCommit = strings.Repeat("b", 40)
				const id = "act-legality01"
				if _, err := m.BeginDeployment(ctx, id, []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: next.SourceCommit}}); err != nil {
					t.Fatal(err)
				}
				_, err := m.Synchronize(ctx, []EvaluatedTable{next}, SynchronizationOptions{ActivationID: id, ForceColumns: []string{scope}})
				if scenario == "null" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatalf("%s repair guessed or erased data", scenario)
				}
				var count int
				expected := `SELECT COUNT(*) FROM acme__orders__orders WHERE (id=7 AND status='draft' AND total=123) OR (id=9 AND status='confirmed' AND total=456)`
				if scenario == "null" {
					expected = `SELECT COUNT(*) FROM acme__orders__orders WHERE (id=7 AND status IS NULL AND total=123) OR (id=9 AND status='confirmed' AND total=456)`
				}
				if e := m.QueryRowContext(ctx, expected).Scan(&count); e != nil || count != 2 {
					t.Fatalf("unrelated cells/rows lost %d %v; change error %v", count, e, err)
				}
			})
		}
	}
}

func TestMetadataPreviewAndExecutionHaveNoDataProbes(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		t.Run(backend, func(t *testing.T) {
			var statements []string
			m := evolutionDatabase(t, backend, func(r StatementRequest) { statements = append(statements, r.Statement) })
			defer m.Close()
			ctx := t.Context()
			if _, err := m.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			old := testDescriptor()
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, old)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `INSERT INTO acme__orders__orders (id,status,total) VALUES (7,'draft',123)`); err != nil {
				t.Fatal(err)
			}
			d := testDescriptor()
			d.Columns[1].EnumValues = []string{"confirmed"}
			d.Columns[1].Default.Value = "confirmed"
			next := evaluatedTable(t, d)
			statements = nil
			if _, err := m.PlanSchema(ctx, []EvaluatedTable{next}); err != nil {
				t.Fatal(err)
			}
			for _, q := range statements {
				if strings.Contains(q, `FROM "acme__orders__orders"`) {
					t.Fatalf("preview analyzed rows: %s", q)
				}
			}
			statements = nil
			if _, err := m.Synchronize(ctx, []EvaluatedTable{next}, SynchronizationOptions{}); err == nil {
				t.Fatal("engine constraint was not enforced")
			}
			for _, q := range statements {
				if strings.Contains(q, `FROM "acme__orders__orders"`) && !strings.HasPrefix(q, "INSERT INTO") {
					t.Fatalf("framework data probe: %s", q)
				}
				if strings.Contains(q, " EXCEPT ") {
					t.Fatalf("preservation scan: %s", q)
				}
			}
		})
	}
}

func TestLargeRelaxationOnlyCopiesWhenBackendRequiresIt(t *testing.T) {
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		t.Run(backend, func(t *testing.T) {
			var queries []string
			m := evolutionDatabase(t, backend, func(r StatementRequest) { queries = append(queries, r.Statement) })
			defer m.Close()
			ctx := t.Context()
			if _, err := m.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			old := testDescriptor()
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, old)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ExecContext(ctx, `WITH RECURSIVE ids(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<50000) INSERT INTO acme__orders__orders (status,total) SELECT 'draft',123 FROM ids`); err != nil {
				t.Fatal(err)
			}
			next := testDescriptor()
			next.Columns[1].EnumValues = []string{"draft", "confirmed", "cancelled"}
			queries = nil
			if _, err := m.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, next)}, SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			copies := 0
			for _, q := range queries {
				if strings.HasPrefix(q, `INSERT INTO "__8020_rewrite_`) {
					copies++
				}
				if strings.Contains(q, `FROM "acme__orders__orders"`) && !strings.HasPrefix(q, "INSERT INTO") {
					t.Fatalf("data probe %s", q)
				}
			}
			want := 0
			if backend == BackendSQLite {
				want = 1
			}
			if copies != want {
				t.Fatalf("unnecessary/missing physical copy %d", copies)
			}
			var id int
			if err := m.QueryRowContext(ctx, `INSERT INTO acme__orders__orders (status,total) VALUES ('cancelled',456) RETURNING id`).Scan(&id); err != nil || id != 50001 {
				t.Fatalf("identity %d %v", id, err)
			}
		})
	}
}
