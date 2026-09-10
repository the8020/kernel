//go:build ignore

package main

import (
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/fsgofer/extension"
)

func TestOrdinarySandboxKeepsStockFilesystemAndFilter(t *testing.T) {
	e := &probeExtension{}
	spec := &specs.Spec{}
	prepared, err := e.PrepareGofer(extension.GoferPrepareContext{Spec: spec})
	if err != nil || len(prepared.FlagOverrides) != 0 || e.fs != nil {
		t.Fatalf("ordinary sandbox enabled development state: %+v, %v", prepared, err)
	}
	fs, _, err := e.TryHandleMount(spec, &specs.Mount{Destination: probeMount}, probeMount, true)
	if err != nil || fs != nil || e.SeccompRules().Size() != 0 {
		t.Fatalf("ordinary mount or security filter was changed: %v", err)
	}
	e.fs = &probeFS{}
	if e.SeccompRules().Size() == 0 {
		t.Fatal("development filesystem lost its required security rules")
	}
}
