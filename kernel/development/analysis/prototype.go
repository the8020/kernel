//go:build ignore

// Included only by run.py prototype. The installed kernel remains unchanged.
package development

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	workspacepackages "the8020/kernel/packages"
)

type analysisPrototypeDriver struct {
	*RunscDriver
	users     string
	sandboxes sync.Map
}

func analysisPrototype(config Config) SandboxDriver {
	base, ok := config.Driver.(*RunscDriver)
	if !ok {
		return config.Driver
	}
	native := *base
	native.config.RunscPath = "WORKFLOW_PROTOTYPE_RUNSC"
	return &analysisPrototypeDriver{RunscDriver: &native, users: config.UsersRoot}
}

func (d *analysisPrototypeDriver) Start(ctx context.Context, start SandboxStart) error {
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

func (m *Manager) Preview(ctx context.Context, user string, options ActivationOptions) (ActivationPreview, error) {
	if err := validatePreviewFile(options); err != nil {
		return ActivationPreview{}, err
	}
	unlock := m.lockUser(user)
	defer unlock()
	result := ActivationPreview{Packages: []ActivationPackagePreview{}}
	sandbox, err := m.loadSandbox(user)
	if err != nil {
		return result, err
	}
	d, ok := m.analysisSparseFor(sandbox.SandboxID)
	if !ok {
		return result, errors.New("development sandbox is not running")
	}
	ids := map[string]bool{}
	for _, root := range []string{d.shared, filepath.Join(d.storage, "upper"), filepath.Join(d.storage, "git")} {
		for _, id := range packageDirectories(root) {
			ids[id] = true
		}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		if len(options.SelectedPackages) != 0 && !slices.Contains(options.SelectedPackages, id) {
			continue
		}
		err := func() error {
			if changed, err := analysisPackageChanged(ctx, d, id); err != nil || !changed {
				return err
			}
			validate, release, err := workspacepackages.ObserveSources(ctx, d.shared, []string{id})
			if err != nil {
				return err
			}
			defer release()
			head, err := m.analysisSharedHead(id)
			if err != nil {
				return err
			}
			if eligible, err := analysisActivationEligible(d, id, head); err != nil || !eligible {
				return err
			}
			if err := analysisEnsurePackageGit(ctx, d, sandbox, id, head, validate); err != nil {
				return err
			}
			item, err := m.analysisCapturePackage(ctx, d, sandbox, id, "", head, false)
			if err != nil {
				return err
			}
			if err := validate(); err != nil {
				return err
			}
			if len(item.Captures) == 0 && len(item.Directories) == 0 {
				return nil
			}
			preview := ActivationPackagePreview{PackageID: id, Selected: true, SharedCommit: head, ActivationReady: true, Files: []ActivationFile{}, ChangedFiles: len(item.Captures)}
			if preview.ChangedFiles == 0 {
				preview.ChangedFiles = len(item.Directories)
			}
			// ponytail: line counts cover changed regular files up to 1 MiB;
			// larger files still appear in the changed-file count.
			for _, capture := range item.Captures {
				change := "modified"
				paths := []string{}
				countLines := true
				for _, side := range []string{"base", "upper"} {
					ref := capture.BaseReference
					if side == "upper" {
						ref = capture.FileReference
					}
					filename := filepath.Join(d.storage, side, id, capture.Path)
					info, err := os.Lstat(filename)
					if errors.Is(err, os.ErrNotExist) && ref != nil {
						countLines = false
						continue
					}
					if errors.Is(err, os.ErrNotExist) {
						paths = append(paths, os.DevNull)
						if side == "base" {
							change = "added"
						} else {
							change = "deleted"
						}
						continue
					}
					if err != nil {
						return err
					}
					if !info.Mode().IsRegular() || info.Size() > 1<<20 {
						countLines = false
					}
					paths = append(paths, filename)
				}
				if countLines {
					stats, err := gitCommand(ctx, d.storage, nil, "diff", "--no-index", "--numstat", "--", paths[0], paths[1])
					var exit *exec.ExitError
					if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
						return err
					}
					values := strings.Fields(stats)
					if len(values) >= 2 {
						added, _ := strconv.Atoi(values[0])
						removed, _ := strconv.Atoi(values[1])
						preview.AddedRows += added
						preview.RemovedRows += removed
					}
				}
				file := ActivationFile{Path: capture.Path, Change: change}
				if options.PreviewFile == file.Path {
					file.Diff, err = analysisPreviewFileDiff(ctx, d, id, capture)
					if err != nil {
						return err
					}
				}
				preview.Files = append(preview.Files, file)
			}
			result.Packages = append(result.Packages, preview)
			return nil
		}()
		if err != nil {
			return result, err
		}
	}
	return result, nil
}
