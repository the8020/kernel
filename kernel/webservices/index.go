package webservices

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/packages"
)

// Specification is resolved application input, not a service declaration or a
// database row. The kernel supplies no defaults and persists no configuration.
// Durations use the runtime's existing JSON nanosecond convention.
type Specification struct {
	ServiceID     string                     `json:"service_id"`
	Version       uint64                     `json:"version"`
	CodeRevision  string                     `json:"code_revision"`
	EntrypointURL string                     `json:"entrypoint"`
	Enabled       bool                       `json:"enabled"`
	Description   string                     `json:"description,omitempty"`
	OpenAPI       supervisor.OpenAPIMetadata `json:"openapi"`
	Access        AccessPolicy               `json:"access"`
	Effective     Configuration              `json:"configuration"`

	Identity packages.Identity `json:"-"`
	Release  string            `json:"-"`
}

type AccessPolicy struct {
	Mode            string                `json:"mode"`
	Unauthenticated UnauthenticatedPolicy `json:"unauthenticated"`
}

type UnauthenticatedPolicy struct {
	Action      string `json:"action"`
	Status      int    `json:"status"`
	Message     string `json:"message,omitempty"`
	RedirectURL string `json:"redirect_url,omitempty"`
}

type Configuration struct {
	Execution ExecutionConfiguration `json:"execution"`
	Lifecycle LifecycleConfiguration `json:"lifecycle"`
	Scaling   ScalingConfiguration   `json:"scaling"`
	Placement PlacementConfiguration `json:"placement"`
	Timeouts  TimeoutConfiguration   `json:"timeouts"`
}

type ExecutionConfiguration struct {
	AnonymousUser string `json:"anonymous_user"`
}

type LifecycleConfiguration struct {
	ServiceType      string        `json:"service_type"`
	SessionKeepAlive time.Duration `json:"session_keep_alive"`
}

type ScalingConfiguration struct {
	MinimumWorkers       int           `json:"minimum_workers"`
	MaximumWorkers       int           `json:"maximum_workers"`
	ConcurrencyPerWorker int           `json:"concurrency_per_worker"`
	TargetUtilization    float64       `json:"target_utilization"`
	WorkerKeepAlive      time.Duration `json:"worker_keep_alive"`
}

type PlacementConfiguration struct {
	SandboxGroup      string `json:"sandbox_group"`
	MinimumSandboxes  int    `json:"minimum_sandboxes"`
	WorkersPerSandbox int    `json:"workers_per_sandbox"`
}

type TimeoutConfiguration struct {
	Request time.Duration `json:"request"`
	Drain   time.Duration `json:"drain"`
	Idle    time.Duration `json:"idle"`
}

// Index contains only accepted immutable package fragments. Dispatch reads this
// derived memory index; the services package owns all durable desired state.
type Index struct {
	mu       sync.RWMutex
	services map[string]Specification
}

func NewIndex() *Index { return &Index{services: map[string]Specification{}} }

// ReplacePackage validates the whole draft before publishing any of it. Scope
// comes from the invocation, never from a mutable field returned by a hook.
func (i *Index) ReplacePackage(packageID string, draft []Specification, chainRevision string) ([]string, error) {
	if _, err := packages.ParsePackageID(packageID); err != nil {
		return nil, err
	}
	next := make(map[string]Specification, len(draft))
	for _, spec := range draft {
		identity, err := packages.ParseServiceID(spec.ServiceID)
		if err != nil || identity.PackageID() != packageID {
			return nil, fmt.Errorf("service %q is outside indexed package %s", spec.ServiceID, packageID)
		}
		if _, exists := next[spec.ServiceID]; exists {
			return nil, fmt.Errorf("duplicate indexed service %s", spec.ServiceID)
		}
		if err := validateSpecification(spec); err != nil {
			return nil, fmt.Errorf("service %s: %w", spec.ServiceID, err)
		}
		encoded, err := json.Marshal(struct {
			Specification
			ChainRevision string
		}{spec, chainRevision})
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(encoded)
		spec.Identity, spec.Release = identity, hex.EncodeToString(digest[:])
		next[spec.ServiceID] = spec
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	var removed []string
	for id, spec := range i.services {
		if spec.Identity.PackageID() == packageID {
			if _, exists := next[id]; !exists {
				removed = append(removed, id)
			}
			delete(i.services, id)
		}
	}
	for id, spec := range next {
		i.services[id] = spec
	}
	slices.Sort(removed)
	return removed, nil
}

func (i *Index) ReadService(serviceID string) (Specification, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	spec, exists := i.services[serviceID]
	if !exists {
		return Specification{}, fmt.Errorf("service %s is not indexed: %w", serviceID, os.ErrNotExist)
	}
	return spec, nil
}

func (i *Index) ServiceIDs() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	ids := make([]string, 0, len(i.services))
	for id := range i.services {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (i *Index) PackageIDs() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	seen := map[string]bool{}
	for _, spec := range i.services {
		seen[spec.Identity.PackageID()] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func validateSpecification(spec Specification) error {
	if spec.Version == 0 || strings.TrimSpace(spec.CodeRevision) == "" {
		return errors.New("a positive version and code revision are required")
	}
	entry, err := url.Parse(spec.EntrypointURL)
	if err != nil || entry.Scheme == "" || strings.ContainsAny(spec.EntrypointURL, "\x00\r\n") {
		return errors.New("entrypoint must be an absolute module URL")
	}
	if spec.Access.Mode != "public" && spec.Access.Mode != "authenticated" {
		return errors.New("access mode must be public or authenticated")
	}
	if spec.Access.Mode == "authenticated" {
		policy := spec.Access.Unauthenticated
		switch policy.Action {
		case "reject":
			if policy.Status < 400 || policy.Status > 599 {
				return errors.New("unauthenticated reject status must be 400..599")
			}
		case "redirect":
			if policy.Status < 300 || policy.Status > 399 || policy.RedirectURL == "" || strings.ContainsAny(policy.RedirectURL, "\r\n\x00") {
				return errors.New("unauthenticated redirect requires a 300..399 status and URL")
			}
			if _, err := url.Parse(policy.RedirectURL); err != nil {
				return fmt.Errorf("unauthenticated redirect URL: %w", err)
			}
		default:
			return errors.New("unauthenticated action must be reject or redirect")
		}
	}
	value := spec.Effective
	if err := execution.ValidateUsername(value.Execution.AnonymousUser); err != nil {
		return err
	}
	if value.Lifecycle.ServiceType != "stateless" && value.Lifecycle.ServiceType != "session" {
		return errors.New("lifecycle must be stateless or session")
	}
	if value.Lifecycle.SessionKeepAlive <= 0 || value.Scaling.WorkerKeepAlive <= 0 || value.Timeouts.Request <= 0 || value.Timeouts.Drain <= 0 || value.Timeouts.Idle < 0 {
		return errors.New("keepalives and request/drain timeouts must be positive; idle timeout cannot be negative")
	}
	if value.Scaling.MinimumWorkers < 0 || value.Scaling.MaximumWorkers < 0 || value.Scaling.MaximumWorkers > 0 && value.Scaling.MinimumWorkers > value.Scaling.MaximumWorkers {
		return errors.New("worker bounds require minimum >= 0 and maximum = 0 or maximum >= minimum")
	}
	if value.Scaling.ConcurrencyPerWorker < 1 || math.IsNaN(value.Scaling.TargetUtilization) || value.Scaling.TargetUtilization <= 0 || value.Scaling.TargetUtilization > 1 {
		return errors.New("concurrency must be positive and target utilization must be in (0, 1]")
	}
	if value.Placement.MinimumSandboxes < 0 || value.Placement.WorkersPerSandbox < 1 || value.Placement.SandboxGroup != strings.TrimSpace(value.Placement.SandboxGroup) || strings.ContainsRune(value.Placement.SandboxGroup, '\x00') {
		return errors.New("invalid sandbox placement bounds or group")
	}
	return nil
}
