package evaluator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"the8020/kernel/database"
	"the8020/kernel/database/schematest"
	"the8020/kernel/deployment"
)

func TestRollbackRecoveryPreservesDataAndDiagnosesDrift(t *testing.T) {
	for _, state := range []string{"compatible", "invalid_data", "catalog_drift"} {
		t.Run(state, func(t *testing.T) {
			e, runner, manager, root := testEvaluator(t, 1)
			ctx := context.Background()
			commitRepository(t, root, "active enum source")
			runner.descriptor = func(item evaluationItem) schematest.Descriptor {
				return schematest.Descriptor{FormatVersion: 1, TableID: item.ExpectedTableID,
					Columns: []schematest.ColumnDescriptor{
						{Name: "id", LogicalType: "text", PrimaryKey: true},
						{Name: "direction", LogicalType: "enum", EnumValues: []string{"IN", "OUT"}},
					}, PrimaryKey: []string{"id"}, Indexes: []schematest.IndexDescriptor{}}
			}
			if _, err := e.SynchronizeAll(ctx, false); err != nil {
				t.Fatal(err)
			}
			definitions, err := manager.EvaluateDefinitions(ctx, []string{"acme/orders"})
			if err != nil {
				t.Fatal(err)
			}
			text := definitions.Tables[0]
			descriptor := schematest.Decode(text.Descriptor)
			descriptor.Columns[1].LogicalType, descriptor.Columns[1].EnumValues = "text", nil
			encoded, _ := json.Marshal(descriptor)
			digest := sha256.Sum256(encoded)
			text.Descriptor, text.DescriptorJSON, text.DescriptorHash = schematest.Transport(descriptor), string(encoded), hex.EncodeToString(digest[:])
			// Simulate an external, internally consistent schema change through its owner.
			if _, err := manager.Synchronize(ctx, []database.EvaluatedTable{text}, database.SynchronizationOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.ExecContext(ctx, `INSERT INTO acme__orders__table0000(id,direction) VALUES ('keep','OUT')`); err != nil {
				t.Fatal(err)
			}
			const id = "act-recovery01"
			if _, err := manager.BeginDeployment(ctx, id, []database.DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "candidate"}}); err != nil {
				t.Fatal(err)
			}
			if state == "invalid_data" {
				if _, err := manager.ExecContext(ctx, `INSERT INTO acme__orders__table0000(id,direction) VALUES ('invalid','OTHER')`); err != nil {
					t.Fatal(err)
				}
			} else if state == "catalog_drift" {
				if _, err := manager.ExecContext(ctx, `UPDATE _8020_columns SET logical_type='enum' WHERE table_id='acme__orders__table0000' AND column_name='direction'`); err != nil {
					t.Fatal(err)
				}
			}
			if state != "compatible" {
				for retry := 0; retry < 2; retry++ {
					err := e.Complete(ctx, id, false)
					if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "rollback") || !strings.Contains(err.Error(), "before retrying") {
						t.Fatalf("missing recovery diagnostics: %v", err)
					}
					pending, exists, err := manager.PendingDeploymentFor(ctx, id)
					if err != nil || !exists || pending.Stage != "rollback_failed" || !strings.Contains(pending.Error, id) {
						t.Fatalf("lost failed deployment: %+v, %t, %v", pending, exists, err)
					}
				}
				if _, err := manager.BeginDeployment(ctx, "act-next000001", []database.DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "next"}}); err == nil {
					t.Fatal("overlapping activation bypassed failed recovery")
				}
				if state == "invalid_data" {
					var bad string
					if err := manager.QueryRowContext(ctx, `SELECT direction FROM acme__orders__table0000 WHERE id='invalid'`).Scan(&bad); err != nil || bad != "OTHER" {
						t.Fatalf("recovery erased incompatible data: %q, %v", bad, err)
					}
					// Explicit application cleanup, never a catalogue or journal bypass.
					if _, err := manager.ExecContext(ctx, `UPDATE acme__orders__table0000 SET direction='IN' WHERE id='invalid'`); err != nil {
						t.Fatal(err)
					}
				}
			}
			if state != "catalog_drift" {
				if err := e.Complete(ctx, id, false); err != nil {
					t.Fatal(err)
				}
				if _, exists, err := manager.PendingDeploymentFor(ctx, id); err != nil || exists {
					t.Fatalf("recovery did not settle: pending=%t, %v", exists, err)
				}
				if err := e.Prepare(ctx, "act-next000001", []deployment.Candidate{{PackageID: "acme/orders", Root: root, Commit: "next"}}); err != nil {
					t.Fatalf("later activation stayed blocked: %v", err)
				}
				if err := e.Complete(ctx, "act-next000001", true); err != nil {
					t.Fatal(err)
				}
			}
			var kept string
			if err := manager.QueryRowContext(ctx, `SELECT direction FROM acme__orders__table0000 WHERE id='keep'`).Scan(&kept); err != nil || kept != "OUT" {
				t.Fatalf("unrelated data changed: %q, %v", kept, err)
			}
		})
	}
}
