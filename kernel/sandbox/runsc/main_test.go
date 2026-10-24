package main

import (
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/cmd/sandboxsetup"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/fsgofer/extension"
)

func TestOrdinarySandboxKeepsStockFilesystemAndFilter(t *testing.T) {
	e := &workspaceExtension{}
	spec := &specs.Spec{}
	prepared, err := e.PrepareGofer(extension.GoferPrepareContext{Spec: spec})
	if err != nil || len(prepared.FlagOverrides) != 0 || e.fs != nil {
		t.Fatalf("ordinary sandbox enabled development state: %+v, %v", prepared, err)
	}
	fs, _, err := e.TryHandleMount(spec, &specs.Mount{Destination: workspaceMount}, workspaceMount, true)
	if err != nil || fs != nil || e.SeccompRules().Size() != 0 {
		t.Fatalf("ordinary mount or security filter was changed: %v", err)
	}
	e.fs = &workspaceFS{}
	if e.SeccompRules().Size() == 0 {
		t.Fatal("development filesystem lost its required security rules")
	}
}

func TestReadOnlyPerOpenConfigOnOverlayHost(t *testing.T) {
	root := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	for _, options := range [][]string{{"ro", "disable_file_handle_sharing"}, {"ro"}, {"rw", "disable_file_handle_sharing"}} {
		got, err := sandboxsetup.AdjustMountOptions(&config.Config{}, root, options)
		if err != nil {
			t.Fatal(err)
		}
		wantOverlay := fs.Type == unix.OVERLAYFS_SUPER_MAGIC && !(slices.Contains(options, "ro") && slices.Contains(options, "disable_file_handle_sharing"))
		if slices.Contains(got, "overlayfs_stale_read") != wantOverlay {
			t.Fatalf("options %v became %v", options, got)
		}
	}
}
