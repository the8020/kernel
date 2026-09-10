//go:build ignore

// Included by the installer and run.py prototype from the same build inputs.
package development

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
)

type analysisPrototypeDriver struct {
	*RunscDriver
	users     string
	sandboxes sync.Map
}

func analysisPrototype(config Config) (SandboxDriver, error) {
	base, ok := config.Driver.(*RunscDriver)
	if !ok {
		return config.Driver, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	native := *base
	// Docker and native installation copy this complete executable directory.
	native.config.RunscPath = filepath.Join(filepath.Dir(executable), "runsc")
	return &analysisPrototypeDriver{RunscDriver: &native, users: config.UsersRoot}, nil
}

func (d *analysisPrototypeDriver) Start(ctx context.Context, start SandboxStart) error {
	var legacy overlayStateDocument
	if err := readTOML(filepath.Join(d.users, start.UserID, "dev-sandbox", "runtime", "overlay", "state.toml"), &legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(legacy.Packages) != 0 {
		return errors.New("development sandbox has legacy private edits; activate or export them using the previous kernel before upgrading")
	}
	storage := filepath.Join(d.users, start.UserID, "dev-sandbox", "workspace")
	for _, name := range []string{"lower", "upper", "base", "deleted", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(storage, name), 0700); err != nil {
			return err
		}
	}
	socket := filepath.Join(storage, "control.sock")
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	native := *d.RunscDriver
	wrapper := filepath.Join(storage, "runsc")
	script := "#!/bin/sh\nset -eu\nmount --bind " + shellQuote(start.Packages) + " " + shellQuote(filepath.Join(storage, "lower")) + "\nmount -o remount,bind,ro " + shellQuote(filepath.Join(storage, "lower")) + "\nexec " + shellQuote(d.config.RunscPath) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		listener.Close()
		return err
	}
	native.config.RunscPath = wrapper
	sparse := &analysisSparseDriver{RunscDriver: &native, storage: storage, shared: start.Packages, listener: listener}
	if err := sparse.Start(ctx, start); err != nil {
		listener.Close()
		return err
	}
	d.sandboxes.Store(start.SandboxID, sparse)
	return nil
}

func (d *analysisPrototypeDriver) Delete(ctx context.Context, id string) error {
	if err := d.RunscDriver.Delete(ctx, id); err != nil {
		return err
	}
	if value, ok := d.sandboxes.LoadAndDelete(id); ok {
		sparse := value.(*analysisSparseDriver)
		if sparse.control != nil {
			sparse.control.Close()
		}
		sparse.listener.Close()
	}
	return nil
}

func (m *Manager) analysisSparseFor(id string) (*analysisSparseDriver, bool) {
	d, ok := m.driver.(*analysisPrototypeDriver)
	if !ok {
		return nil, false
	}
	value, ok := d.sandboxes.Load(id)
	if !ok {
		return nil, false
	}
	return value.(*analysisSparseDriver), true
}

func (m *Manager) analysisResetWorkspace(sandbox *Sandbox) error {
	if _, err := os.Stat(filepath.Join(m.sandboxRoot(*sandbox), "activation/active.json")); err == nil {
		return errors.New("finish the pending activation before resetting source")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sandbox.LastActivationResult, sandbox.LastActivationStatus = nil, ""
	return os.RemoveAll(filepath.Join(m.sandboxRoot(*sandbox), "workspace"))
}
