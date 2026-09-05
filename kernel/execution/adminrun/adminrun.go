// Package adminrun materializes bounded artifacts for runtime eval and run commands.
package adminrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/model"
)

type JobRunner interface {
	Run(context.Context, string, string, jobs.Options) (jobs.Record, error)
}
type Config struct {
	InstanceRoot  string
	ArtifactsRoot string
	Jobs          JobRunner
	MaximumFiles  int
	MaximumBytes  int64
}
type Manager struct {
	instanceRoot  string
	artifactsRoot string
	jobs          JobRunner
	maximumFiles  int
	maximumBytes  int64
}
type Options struct {
	WorkloadType      model.WorkloadType
	OwnerID           string
	GroupKey          string
	Namespace         string
	Timeout           time.Duration
	Detached          bool
	Input             any
	Reuse             *bool
	Permissions       *supervisor.WorkerPermissions
	Workspace         string
	WorkspaceWritable bool
}
type Result struct {
	ArtifactID string      `json:"artifact_id"`
	Entrypoint string      `json:"entrypoint"`
	Execution  jobs.Record `json:"execution"`
}

func New(config Config) (*Manager, error) {
	instance, err := filepath.EvalSymlinks(config.InstanceRoot)
	if err != nil {
		return nil, err
	}
	artifacts, err := filepath.Abs(config.ArtifactsRoot)
	if err != nil {
		return nil, err
	}
	if config.Jobs == nil {
		return nil, errors.New("job runner is required")
	}
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(artifacts, 0o755); err != nil {
		return nil, err
	}
	if config.MaximumFiles <= 0 {
		config.MaximumFiles = 10000
	}
	if config.MaximumBytes <= 0 {
		config.MaximumBytes = 64 << 20
	}
	return &Manager{instanceRoot: instance, artifactsRoot: artifacts, jobs: config.Jobs, maximumFiles: config.MaximumFiles, maximumBytes: config.MaximumBytes}, nil
}

func (m *Manager) Eval(ctx context.Context, code string, options Options) (Result, error) {
	if strings.TrimSpace(code) == "" {
		return Result{}, errors.New("evaluation source is required")
	}
	artifactID, err := model.NewID("artifact")
	if err != nil {
		return Result{}, err
	}
	directory := filepath.Join(m.artifactsRoot, artifactID)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		return Result{}, err
	}
	module := filepath.Join(directory, "module.ts")
	entry := filepath.Join(directory, "entry.ts")
	if err := os.WriteFile(module, []byte(code+"\n"), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(module, 0o644); err != nil {
		return Result{}, err
	}
	wrapper := `import value from "./module.ts";
export default async function evaluate(...arguments_: unknown[]): Promise<unknown> {
  return typeof value === "function" ? await value(...arguments_) : value;
}
`
	if err := os.WriteFile(entry, []byte(wrapper), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(entry, 0o644); err != nil {
		return Result{}, err
	}
	return m.submit(ctx, "runtime-eval", artifactID, "entry.ts", options)
}

func (m *Manager) Run(ctx context.Context, path string, options Options) (Result, error) {
	source, err := m.canonicalSource(path)
	if err != nil {
		return Result{}, err
	}
	artifactID, err := model.NewID("artifact")
	if err != nil {
		return Result{}, err
	}
	destination := filepath.Join(m.artifactsRoot, artifactID)
	if err := m.copyTree(filepath.Dir(source), destination); err != nil {
		return Result{}, err
	}
	return m.submit(ctx, "runtime-run", artifactID, filepath.Base(source), options)
}

func (m *Manager) submit(ctx context.Context, jobID, artifactID, relative string, options Options) (Result, error) {
	if options.WorkloadType == "" {
		options.WorkloadType = model.WorkloadJob
	}
	if options.WorkloadType != model.WorkloadJob {
		return Result{}, fmt.Errorf("administrative execution workload type must be %q", model.WorkloadJob)
	}
	entrypoint := "file:///artifacts/" + artifactID + "/" + filepath.ToSlash(relative)
	permissions := artifactPermissions(options.Permissions, "/artifacts/"+artifactID)
	arguments := []any(nil)
	if options.Input != nil {
		arguments = []any{options.Input}
	}
	record, err := m.jobs.Run(ctx, jobID, entrypoint, jobs.Options{User: execution.DefaultUser(ctx), OwnerID: options.OwnerID, Arguments: arguments, Detached: options.Detached, GroupKey: options.GroupKey, Namespace: options.Namespace, Timeout: options.Timeout, Reuse: options.Reuse, Permissions: permissions, ReleaseID: artifactID, Workspace: options.Workspace, WorkspaceWritable: options.WorkspaceWritable})
	if err != nil {
		return Result{ArtifactID: artifactID, Entrypoint: entrypoint, Execution: record}, err
	}
	return Result{ArtifactID: artifactID, Entrypoint: entrypoint, Execution: record}, nil
}

func artifactPermissions(requested *supervisor.WorkerPermissions, artifactRoot string) *supervisor.WorkerPermissions {
	permissions := supervisor.WorkerPermissions{}
	if requested != nil {
		permissions = supervisor.WorkerPermissions{
			Read:   append([]string(nil), requested.Read...),
			Write:  append([]string(nil), requested.Write...),
			Net:    append([]string(nil), requested.Net...),
			Import: append([]string(nil), requested.Import...),
			Env:    append([]string(nil), requested.Env...),
			Sys:    append([]string(nil), requested.Sys...),
		}
	}
	for _, allowed := range permissions.Read {
		clean := path.Clean(allowed)
		if clean == artifactRoot || strings.HasPrefix(artifactRoot, clean+"/") {
			return &permissions
		}
	}
	permissions.Read = append(permissions.Read, artifactRoot)
	return &permissions
}

func (m *Manager) canonicalSource(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.instanceRoot, path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(m.instanceRoot, canonical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("runtime module is outside the instance root")
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "node" || part == ".git" {
			return "", errors.New("runtime module is inside a protected control directory")
		}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("runtime module must be a regular file")
	}
	return canonical, nil
}

func (m *Manager) copyTree(source, destination string) error {
	count := 0
	var total int64
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source tree contains symlink %s", relative)
		}
		if entry.IsDir() {
			if relative != "." && (entry.Name() == "node" || entry.Name() == ".git") {
				return filepath.SkipDir
			}
			directory := filepath.Join(destination, relative)
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return err
			}
			return os.Chmod(directory, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source tree contains non-regular file %s", relative)
		}
		count++
		total += info.Size()
		if count > m.maximumFiles || total > m.maximumBytes {
			return errors.New("source tree exceeds artifact file or byte limit")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(filepath.Join(destination, relative), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := errors.Join(output.Close(), input.Close())
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		return os.Chmod(filepath.Join(destination, relative), 0o644)
	})
}
