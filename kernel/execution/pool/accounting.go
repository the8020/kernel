// Warm accounting is owned by the pool alongside provisioning and assignment.
package pool

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type WarmState string

const (
	WarmCreating WarmState = "CREATING"
	WarmReady    WarmState = "READY"
	WarmReserved WarmState = "RESERVED"
	WarmAssigned WarmState = "ASSIGNED"
	WarmFailed   WarmState = "FAILED"
)

type WarmSandbox struct {
	SandboxID   string    `json:"sandbox_id"`
	ProfileHash string    `json:"profile_hash"`
	State       WarmState `json:"state"`
}

type PoolStatus struct {
	ProfileHash string `json:"profile_hash"`
	Desired     int    `json:"desired_warm_count"`
	Ready       int    `json:"ready_warm_count"`
	Creating    int    `json:"creating_count"`
	Reserved    int    `json:"reserved_count"`
	Assigned    int    `json:"assigned_count"`
	Failed      int    `json:"failed_count"`
	Replenish   int    `json:"replenish_count"`
}

type WarmPool struct {
	mu        sync.Mutex
	desired   map[string]int
	sandboxes map[string]WarmSandbox
}

func NewWarmPool() *WarmPool {
	return &WarmPool{desired: map[string]int{}, sandboxes: map[string]WarmSandbox{}}
}

func (p *WarmPool) Resize(profileHash string, count int) error {
	if !strings.HasPrefix(profileHash, "sha256:") || count < 0 {
		return errors.New("valid profile hash and non-negative desired count are required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.desired[profileHash] = count
	return nil
}

func (p *WarmPool) Add(sandbox WarmSandbox) error {
	if sandbox.SandboxID == "" || !strings.HasPrefix(sandbox.ProfileHash, "sha256:") || (sandbox.State != WarmCreating && sandbox.State != WarmReady) {
		return errors.New("new warm sandbox requires identity, profile hash, and creating/ready state")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.sandboxes[sandbox.SandboxID]; exists {
		return fmt.Errorf("warm sandbox %s already exists", sandbox.SandboxID)
	}
	p.sandboxes[sandbox.SandboxID] = sandbox
	return nil
}

// Restore reconstructs durable warm-pool accounting after kernel restart.
func (p *WarmPool) Restore(sandbox WarmSandbox) error {
	if sandbox.SandboxID == "" || !strings.HasPrefix(sandbox.ProfileHash, "sha256:") || (sandbox.State != WarmReady && sandbox.State != WarmAssigned && sandbox.State != WarmFailed) {
		return errors.New("restored warm sandbox requires identity, profile hash, and ready/assigned/failed state")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.sandboxes[sandbox.SandboxID]; exists {
		return fmt.Errorf("warm sandbox %s is already tracked", sandbox.SandboxID)
	}
	p.sandboxes[sandbox.SandboxID] = sandbox
	return nil
}

func (p *WarmPool) SetState(sandboxID string, state WarmState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sandbox, ok := p.sandboxes[sandboxID]
	if !ok {
		return fmt.Errorf("unknown warm sandbox %s", sandboxID)
	}
	allowed := map[WarmState]map[WarmState]bool{
		WarmCreating: {WarmReady: true, WarmFailed: true},
		WarmReady:    {WarmReserved: true, WarmFailed: true},
		WarmReserved: {WarmReady: true, WarmAssigned: true, WarmFailed: true},
		WarmAssigned: {}, WarmFailed: {},
	}
	if !allowed[sandbox.State][state] {
		return fmt.Errorf("invalid warm-sandbox transition %s -> %s", sandbox.State, state)
	}
	sandbox.State = state
	p.sandboxes[sandboxID] = sandbox
	return nil
}

func (p *WarmPool) Reserve(profileHash string) (WarmSandbox, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.sandboxes))
	for id := range p.sandboxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		sandbox := p.sandboxes[id]
		if sandbox.ProfileHash == profileHash && sandbox.State == WarmReady {
			sandbox.State = WarmReserved
			p.sandboxes[id] = sandbox
			return sandbox, true
		}
	}
	return WarmSandbox{}, false
}

func (p *WarmPool) Destroy(sandboxID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sandboxes[sandboxID]; !ok {
		return fmt.Errorf("unknown warm sandbox %s", sandboxID)
	}
	delete(p.sandboxes, sandboxID)
	return nil
}

func (p *WarmPool) Desired(profileHash string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.desired[profileHash]
}

func (p *WarmPool) Sandboxes(profileHash string, state WarmState) []WarmSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]WarmSandbox, 0)
	for _, sandbox := range p.sandboxes {
		if (profileHash == "" || sandbox.ProfileHash == profileHash) && (state == "" || sandbox.State == state) {
			result = append(result, sandbox)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SandboxID < result[j].SandboxID })
	return result
}

func (p *WarmPool) Status() []PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	profiles := map[string]bool{}
	for profile := range p.desired {
		profiles[profile] = true
	}
	for _, sandbox := range p.sandboxes {
		profiles[sandbox.ProfileHash] = true
	}
	keys := make([]string, 0, len(profiles))
	for profile := range profiles {
		keys = append(keys, profile)
	}
	sort.Strings(keys)
	result := make([]PoolStatus, 0, len(keys))
	for _, profile := range keys {
		status := PoolStatus{ProfileHash: profile, Desired: p.desired[profile]}
		for _, sandbox := range p.sandboxes {
			if sandbox.ProfileHash != profile {
				continue
			}
			switch sandbox.State {
			case WarmCreating:
				status.Creating++
			case WarmReady:
				status.Ready++
			case WarmReserved:
				status.Reserved++
			case WarmAssigned:
				status.Assigned++
			case WarmFailed:
				status.Failed++
			}
		}
		availableSoon := status.Ready + status.Creating
		if availableSoon < status.Desired {
			status.Replenish = status.Desired - availableSoon
		}
		result = append(result, status)
	}
	return result
}
