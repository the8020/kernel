package settings

import (
	"context"
	"testing"
)

func TestGlobalAttachPreservesEarlyNodeRuntimeOwnerAndRestartState(t *testing.T) {
	defs := testDefinitions()
	defs = append(defs, Definition{Key: "test.node_boot", Type: TypeString, Storage: StorageNode, Default: "before", Environment: "THE8020_TEST_NODE_BOOT", RestartRequired: true, Description: "Node restart policy."})
	m, err := New(defs, newPersistencePaths(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	applier := &testApplier{}
	if err := m.RegisterApplier([]string{"logging.enabled"}, applier); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Set(context.Background(), "logging.enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Set(context.Background(), "test.node_boot", "after"); err != nil {
		t.Fatal(err)
	}
	if err := m.AttachGlobal(context.Background(), &memoryGlobalStore{values: map[string]any{"platform.display_name": "Configured"}}); err != nil {
		t.Fatal(err)
	}
	if active, _ := m.Active("logging.enabled"); active != false || !applier.committed {
		t.Fatal("global attach replaced active node logging", active)
	}
	node, _ := m.Get("test.node_boot")
	if !node.RestartPending || node.ActiveValue != "before" || node.ConfiguredValue != "after" {
		t.Fatal("global attach reset node restart state", node)
	}
	global, _ := m.Get("platform.display_name")
	if global.ActiveValue != "Configured" || global.RestartPending {
		t.Fatal("global attach did not publish authoritative boot value", global)
	}
}

func TestFailedGlobalAttachDoesNotPartiallyPublish(t *testing.T) {
	m, err := New(testDefinitions(), newPersistencePaths(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryGlobalStore{values: map[string]any{"network.root_alias": "valid/path/", "platform.display_name": int64(1)}}
	if err := m.AttachGlobal(context.Background(), store); err == nil {
		t.Fatal("accepted invalid global snapshot")
	}
	alias, _ := m.Get("network.root_alias")
	if alias.ConfiguredValue != "the8020/uui/shell/" || alias.Source == "persisted" || m.global != nil {
		t.Fatal("failed attach changed settings", alias)
	}
}
