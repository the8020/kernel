// Package profile derives runtime profiles for explicit Worker permissions.
package profile

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/model"
)

type MountPolicy interface {
	Validate(model.Mount) (model.Mount, error)
}

type Workspace struct {
	Source   string
	OwnerID  string
	Writable bool
}

// ForWorkerWithWorkspace adds one explicitly requested, policy-approved
// development workspace before deriving the compatible Worker profile.
func ForWorkerWithWorkspace(base model.RuntimeProfile, requested *supervisor.WorkerPermissions, workspace Workspace, policy MountPolicy) (model.RuntimeProfile, error) {
	if workspace.Source == "" {
		if workspace.Writable {
			return model.RuntimeProfile{}, errors.New("development workspace write access requires a workspace source")
		}
		return ForWorker(base, requested)
	}
	if policy == nil {
		return model.RuntimeProfile{}, errors.New("development workspace mounts are unavailable")
	}
	if strings.TrimSpace(workspace.OwnerID) == "" {
		return model.RuntimeProfile{}, errors.New("development workspace owner is required")
	}
	mount, err := policy.Validate(model.Mount{Source: workspace.Source, Target: "/workspace", ReadOnly: !workspace.Writable, OwnerScope: workspace.OwnerID, Purpose: "workspace", Persistence: "development"})
	if err != nil {
		return model.RuntimeProfile{}, fmt.Errorf("development workspace: %w", err)
	}
	base.Mounts = append(append([]model.Mount(nil), base.Mounts...), mount)
	base.Permissions.ReadPaths = union(base.Permissions.ReadPaths, []string{mount.Target})
	if workspace.Writable {
		base.Permissions.WritePaths = union(base.Permissions.WritePaths, []string{mount.Target})
	}
	return ForWorker(base, requested)
}

var reservedEnvironment = map[string]bool{
	"SANDBOX_ID": true, "RUNTIME_GROUP_ID": true, "WORKLOAD_TYPE": true,
	"IMAGE_DIGEST": true, "INTERNAL_API_TOKEN": true, "KERNEL_CALLBACK_ADDRESS": true,
	"DEPENDENCY_MODE": true,
	"SUPERVISOR_HOST": true, "SUPERVISOR_PORT": true, "INSPECTOR_PORT": true, "RUNTIME_PROFILE_HASH": true, "HEARTBEAT_INTERVAL_MS": true,
	"WORKER_STOP_GRACE_MS": true,
}

func ForWorker(base model.RuntimeProfile, requested *supervisor.WorkerPermissions) (model.RuntimeProfile, error) {
	if requested == nil {
		return base, nil
	}
	for _, check := range []struct {
		name               string
		requested, allowed []string
	}{{"read", requested.Read, base.Permissions.ReadPaths}, {"write", requested.Write, base.Permissions.WritePaths}} {
		for _, path := range check.requested {
			if !withinAny(path, check.allowed) {
				return model.RuntimeProfile{}, fmt.Errorf("Worker %s path %q is outside the mounted runtime profile", check.name, path)
			}
		}
	}
	for _, host := range append(append([]string(nil), requested.Net...), requested.Import...) {
		if !base.EgressAllowed {
			return model.RuntimeProfile{}, errors.New("sandbox network egress is disabled")
		}
		if err := validateHost(host); err != nil {
			return model.RuntimeProfile{}, err
		}
	}
	for _, name := range requested.Env {
		if name == "" || strings.ContainsAny(name, "=\x00") || reservedEnvironment[name] {
			return model.RuntimeProfile{}, fmt.Errorf("Worker environment permission %q is reserved or invalid", name)
		}
	}
	derived := base
	derived.Permissions.NetworkHosts = union(base.Permissions.NetworkHosts, requested.Net)
	derived.Permissions.ImportHosts = union(base.Permissions.ImportHosts, requested.Import)
	derived.Permissions.Environment = union(base.Permissions.Environment, requested.Env)
	derived.Permissions.SystemInfo = base.Permissions.SystemInfo || len(requested.Sys) > 0
	if len(requested.Import) > 0 {
		derived.DependencyMode = model.DependencyOnline
	}
	return derived, nil
}

func withinAny(path string, roots []string) bool {
	if !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return false
	}
	for _, root := range roots {
		relative, err := filepath.Rel(filepath.Clean(root), path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func validateHost(value string) error {
	if value == "" || strings.ContainsAny(value, "/*?#@") {
		return fmt.Errorf("network/import host %q is invalid", value)
	}
	host := value
	if parsedHost, port, err := net.SplitHostPort(value); err == nil {
		if parsedHost == "" || port == "" {
			return fmt.Errorf("network/import host %q is invalid", value)
		}
		host = parsedHost
	} else if strings.Count(value, ":") > 1 && net.ParseIP(value) == nil {
		return fmt.Errorf("network/import host %q is invalid", value)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Hostname() == "" || parsed.Hostname() != host || strings.Contains(host, "..") {
		return fmt.Errorf("network/import host %q is invalid", value)
	}
	return nil
}

func union(left, right []string) []string {
	values := map[string]bool{}
	for _, value := range append(append([]string(nil), left...), right...) {
		if value != "" {
			values[value] = true
		}
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
