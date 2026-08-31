package network

import (
	"context"
	"testing"

	"the8020/kernel/sandbox/model"
)

func TestLoopbackManagerAllocatesDistinctPersistentControlPorts(t *testing.T) {
	manager, err := NewLoopback(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policy := model.NetworkConfiguration{Mode: "netstack"}
	first, err := manager.Allocate(context.Background(), "group-one", "sandbox-one", policy)
	if err != nil {
		t.Fatal(err)
	}
	if first.NetworkName != "rootless-host" || len(first.IPs) != 1 || first.IPs[0] != "127.0.0.1" || first.SupervisorPort < 1 || first.InspectorPort < 1 || first.SupervisorPort == first.InspectorPort {
		t.Fatalf("allocation=%#v", first)
	}
	repeated, err := manager.Allocate(context.Background(), "group-one", "sandbox-one", policy)
	if err != nil || repeated.SupervisorPort != first.SupervisorPort || repeated.InspectorPort != first.InspectorPort {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
	second, err := manager.Allocate(context.Background(), "group-two", "sandbox-two", policy)
	if err != nil {
		t.Fatal(err)
	}
	if second.SupervisorPort == first.SupervisorPort || second.InspectorPort == first.InspectorPort {
		t.Fatalf("allocations collided: %#v %#v", first, second)
	}
	if err := manager.Check(context.Background(), "group-one"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), "group-one"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Check(context.Background(), "group-one"); err == nil {
		t.Fatal("released allocation remained available")
	}
}
