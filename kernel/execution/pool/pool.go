// Package pool provisions and assigns clean warm runtime groups.
package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"the8020/kernel/execution/groups"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type Sandboxes interface {
	NewSandboxID() (string, error)
	ReleaseSandboxID(string)
	List() ([]manager.Inspection, error)
	Create(context.Context, model.SandboxSpec) (manager.Inspection, error)
	AssignWarm(context.Context, string, string, string) (manager.Inspection, error)
	Delete(context.Context, string) error
}

type Template struct {
	Profile   model.RuntimeProfile
	Resources model.ResourceLimits
	Lifecycle model.LifecyclePolicy
	Network   string
}

type Controller struct {
	pool      *groups.WarmPool
	sandboxes Sandboxes
	templates map[string]Template
	logger    *slog.Logger
	reconcile sync.Mutex
	lifecycle sync.Mutex
	started   bool
	cancel    context.CancelFunc
	queue     chan string
	wait      sync.WaitGroup
}

func New(sandboxes Sandboxes, templates []Template, logger *slog.Logger) (*Controller, error) {
	if sandboxes == nil || len(templates) == 0 {
		return nil, errors.New("sandbox manager and at least one warm profile template are required")
	}
	controller := &Controller{pool: groups.NewWarmPool(), sandboxes: sandboxes, templates: map[string]Template{}, logger: logger, queue: make(chan string, len(templates)*2)}
	for _, template := range templates {
		hash, err := template.Profile.Hash()
		if err != nil {
			return nil, fmt.Errorf("warm profile: %w", err)
		}
		if err := template.Resources.Validate(); err != nil {
			return nil, fmt.Errorf("warm resources for %s: %w", hash, err)
		}
		if template.Network == "" {
			template.Network = "the8020"
		}
		if _, duplicate := controller.templates[hash]; duplicate {
			return nil, fmt.Errorf("duplicate warm profile %s", hash)
		}
		controller.templates[hash] = template
	}
	return controller, nil
}

func (c *Controller) Start(ctx context.Context, desired int) error {
	if desired < 0 {
		return errors.New("warm-pool size cannot be negative")
	}
	items, err := c.sandboxes.List()
	if err != nil {
		return fmt.Errorf("list reconciled sandboxes for warm-pool restore: %w", err)
	}
	for _, item := range items {
		if _, registered := c.templates[item.Spec.ProfileHash]; !registered {
			continue
		}
		state := groups.WarmState("")
		switch {
		case item.Spec.Lifecycle.Warm && item.Status.ObservedState == model.StateReady && item.Status.SupervisorHealthy && item.Status.WorkerCount == 0:
			state = groups.WarmReady
		case item.Spec.Lifecycle.Warm:
			state = groups.WarmFailed
		case item.Spec.Labels["the8020.assigned_at"] != "":
			state = groups.WarmAssigned
		default:
			continue
		}
		if err := c.pool.Restore(groups.WarmGroup{RuntimeGroupID: item.Spec.RuntimeGroupID, ProfileHash: item.Spec.ProfileHash, State: state}); err != nil {
			return fmt.Errorf("restore warm-pool group %s: %w", item.Spec.RuntimeGroupID, err)
		}
	}
	c.lifecycle.Lock()
	if c.started {
		c.lifecycle.Unlock()
		return errors.New("warm-pool controller is already started")
	}
	background, cancel := context.WithCancel(context.Background())
	c.cancel, c.started = cancel, true
	c.wait.Add(1)
	go c.run(background)
	c.lifecycle.Unlock()
	for hash := range c.templates {
		if err := c.pool.Resize(hash, desired); err != nil {
			return err
		}
		if err := c.reconcileProfile(ctx, hash); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) Resize(profileHash string, count int) error {
	if _, ok := c.templates[profileHash]; !ok {
		return fmt.Errorf("warm-pool profile %q is not registered", profileHash)
	}
	if err := c.pool.Resize(profileHash, count); err != nil {
		return err
	}
	c.trigger(profileHash)
	return nil
}

func (c *Controller) Status() []groups.PoolStatus { return c.pool.Status() }

func (c *Controller) Forget(runtimeGroupID string) error {
	for _, group := range c.pool.Groups("", "") {
		if group.RuntimeGroupID != runtimeGroupID {
			continue
		}
		if err := c.pool.Destroy(runtimeGroupID); err != nil {
			return err
		}
		c.trigger(group.ProfileHash)
		return nil
	}
	return nil
}

func (c *Controller) Assign(ctx context.Context, profileHash, groupKey, ownerID string) (manager.Inspection, bool, error) {
	if _, ok := c.templates[profileHash]; !ok {
		return manager.Inspection{}, false, nil
	}
	warm, ok := c.pool.Reserve(profileHash)
	if !ok {
		return manager.Inspection{}, false, nil
	}
	assigned, err := c.sandboxes.AssignWarm(ctx, warm.RuntimeGroupID, groupKey, ownerID)
	if err != nil {
		_ = c.pool.SetState(warm.RuntimeGroupID, groups.WarmFailed)
		c.trigger(profileHash)
		return manager.Inspection{}, false, err
	}
	if err := c.pool.SetState(warm.RuntimeGroupID, groups.WarmAssigned); err != nil {
		return manager.Inspection{}, false, err
	}
	c.trigger(profileHash)
	return assigned, true, nil
}

func (c *Controller) Close() error {
	c.lifecycle.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.lifecycle.Unlock()
	c.wait.Wait()
	return nil
}

func (c *Controller) run(ctx context.Context) {
	defer c.wait.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case hash := <-c.queue:
			if err := c.reconcileProfile(ctx, hash); err != nil {
				if c.logger != nil {
					c.logger.Error("warm-pool reconciliation failed", "profile_hash", hash, "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
					c.trigger(hash)
				}
			}
		}
	}
}

func (c *Controller) trigger(profileHash string) {
	c.lifecycle.Lock()
	started := c.started
	c.lifecycle.Unlock()
	if !started {
		return
	}
	select {
	case c.queue <- profileHash:
	default:
	}
}

func (c *Controller) reconcileProfile(ctx context.Context, profileHash string) error {
	c.reconcile.Lock()
	defer c.reconcile.Unlock()
	template, ok := c.templates[profileHash]
	if !ok {
		return fmt.Errorf("warm-pool profile %q is not registered", profileHash)
	}
	status := statusFor(c.pool.Status(), profileHash)
	for status.Ready+status.Creating < status.Desired {
		if err := c.create(ctx, profileHash, template); err != nil {
			return err
		}
		status = statusFor(c.pool.Status(), profileHash)
	}
	ready := c.pool.Groups(profileHash, groups.WarmReady)
	for len(ready) > status.Desired {
		candidate := ready[len(ready)-1]
		if err := c.sandboxes.Delete(ctx, candidate.RuntimeGroupID); err != nil {
			_ = c.pool.SetState(candidate.RuntimeGroupID, groups.WarmFailed)
			return fmt.Errorf("trim warm group %s: %w", candidate.RuntimeGroupID, err)
		}
		if err := c.pool.Destroy(candidate.RuntimeGroupID); err != nil {
			return err
		}
		ready = ready[:len(ready)-1]
	}
	return nil
}

func (c *Controller) create(ctx context.Context, profileHash string, template Template) error {
	runtimeGroupID, err := model.NewRuntimeGroupID()
	if err != nil {
		return err
	}
	sandboxID, err := c.sandboxes.NewSandboxID()
	if err != nil {
		return err
	}
	defer c.sandboxes.ReleaseSandboxID(sandboxID)
	token, err := model.NewID("token")
	if err != nil {
		return err
	}
	if err := c.pool.Add(groups.WarmGroup{RuntimeGroupID: runtimeGroupID, ProfileHash: profileHash, State: groups.WarmCreating}); err != nil {
		return err
	}
	lifecycle := template.Lifecycle
	lifecycle.Warm = true
	lifecycle.DestroyWhenIdle = false
	spec := model.SandboxSpec{
		SandboxID: sandboxID, RuntimeGroupID: runtimeGroupID, WorkloadType: template.Profile.WorkloadType,
		ImageDigest: template.Profile.ImageDigest, RuntimeProfile: template.Profile, ProfileHash: profileHash,
		ResourceLimits: template.Resources,
		Network:        model.NetworkConfiguration{Mode: "netstack", NetworkName: template.Network, EgressEnabled: len(template.Profile.Permissions.EgressHosts()) > 0, AllowedHosts: template.Profile.Permissions.EgressHosts()},
		InternalPorts:  []int{8000, 9229}, Mounts: append([]model.Mount(nil), template.Profile.Mounts...), Permissions: template.Profile.Permissions,
		DependencyMode: template.Profile.DependencyMode, Lifecycle: lifecycle,
		Labels: map[string]string{"the8020.warm": "true", "the8020.created_at": time.Now().UTC().Format(time.RFC3339Nano)}, InternalToken: token,
	}
	if _, err := c.sandboxes.Create(ctx, spec); err != nil {
		_ = c.pool.SetState(runtimeGroupID, groups.WarmFailed)
		return fmt.Errorf("create warm runtime group: %w", err)
	}
	if err := c.pool.SetState(runtimeGroupID, groups.WarmReady); err != nil {
		return err
	}
	return nil
}

func statusFor(statuses []groups.PoolStatus, profileHash string) groups.PoolStatus {
	for _, status := range statuses {
		if status.ProfileHash == profileHash {
			return status
		}
	}
	return groups.PoolStatus{ProfileHash: profileHash}
}
