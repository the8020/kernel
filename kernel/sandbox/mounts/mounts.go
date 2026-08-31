// Package mounts validates controlled host-to-sandbox mounts.
package mounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"the8020/kernel/sandbox/model"
)

type Policy struct {
	allowedRoots      []string
	kernelState       string
	containerdSocket  string
	requireOwnerScope bool
}

func NewPolicy(allowedRoots []string, kernelState, containerdSocket string, requireOwnerScope bool) (Policy, error) {
	policy := Policy{kernelState: filepath.Clean(kernelState), containerdSocket: filepath.Clean(containerdSocket), requireOwnerScope: requireOwnerScope}
	for _, root := range allowedRoots {
		canonical, err := canonicalExisting(root)
		if err != nil {
			return Policy{}, fmt.Errorf("allowed mount root %q: %w", root, err)
		}
		if canonical == string(filepath.Separator) || protectedHostPath(canonical) {
			return Policy{}, fmt.Errorf("unsafe allowed mount root %q", root)
		}
		policy.allowedRoots = append(policy.allowedRoots, canonical)
	}
	if len(policy.allowedRoots) == 0 {
		return Policy{}, errors.New("at least one allowed mount root is required")
	}
	return policy, nil
}

func (p Policy) Validate(request model.Mount) (model.Mount, error) {
	target := filepath.Clean(request.Target)
	if !filepath.IsAbs(target) || target != request.Target {
		return model.Mount{}, errors.New("mount target must be a canonical absolute path")
	}
	if !allowedTarget(target) {
		return model.Mount{}, fmt.Errorf("mount target %q is outside controlled sandbox directories", target)
	}
	if request.Purpose == "" || request.Persistence == "" {
		return model.Mount{}, errors.New("mount purpose and persistence policy are required")
	}
	if p.requireOwnerScope && request.OwnerScope == "" {
		return model.Mount{}, errors.New("grouped-owner mount requires owner scope")
	}
	if request.Purpose == "temporary" {
		if request.Source != "" || request.ReadOnly || request.MaximumSize <= 0 || !beneath(target, "/tmp") {
			return model.Mount{}, errors.New("temporary mounts must be bounded writable tmpfs beneath /tmp with no host source")
		}
		request.Target = target
		return request, nil
	}
	if request.Source == "" {
		return model.Mount{}, errors.New("non-temporary mount source is required")
	}
	sourcePath := request.Source
	if !filepath.IsAbs(sourcePath) {
		sourcePath = filepath.Join(p.allowedRoots[0], sourcePath)
	}
	source, err := canonicalExisting(sourcePath)
	if err != nil {
		return model.Mount{}, fmt.Errorf("mount source: %w", err)
	}
	if source == string(filepath.Separator) || protectedHostPath(source) || beneath(source, p.kernelState) || beneath(p.kernelState, source) || source == p.containerdSocket || beneath(p.containerdSocket, source) {
		return model.Mount{}, fmt.Errorf("mount source %q is protected", source)
	}
	allowed := false
	for _, root := range p.allowedRoots {
		if beneath(source, root) {
			allowed = true
			break
		}
	}
	if !allowed {
		return model.Mount{}, fmt.Errorf("mount source %q is outside configured roots", source)
	}
	if request.Purpose == "artifact" && !request.ReadOnly {
		return model.Mount{}, errors.New("artifact mounts must be read-only")
	}
	if request.Purpose == "workspace" {
		info, err := os.Stat(source)
		if err != nil {
			return model.Mount{}, fmt.Errorf("inspect workspace source: %w", err)
		}
		if !info.IsDir() {
			return model.Mount{}, errors.New("workspace mounts require a directory source")
		}
	}
	request.Source, request.Target = source, target
	return request, nil
}

func canonicalExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(canonical); err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func protectedHostPath(path string) bool {
	for _, protected := range []string{"/proc", "/sys", "/dev"} {
		if beneath(path, protected) {
			return true
		}
	}
	return false
}

func allowedTarget(path string) bool {
	if beneath(path, "/opt/runtime") || beneath(path, "/proc") || beneath(path, "/sys") || beneath(path, "/dev") {
		return false
	}
	for _, root := range []string{"/artifacts", "/workspace", "/runtime-cache", "/tmp"} {
		if beneath(path, root) {
			return true
		}
	}
	return false
}

func beneath(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	if path == root {
		return true
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
