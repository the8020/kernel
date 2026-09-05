// Package groups selects compatible runtime groups and tracks clean warm capacity.
package groups

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"the8020/kernel/sandbox/model"
)

type Request struct {
	WorkloadType     model.WorkloadType
	OwnerID          string
	ExecutionID      string
	Namespace        string
	ExplicitGroupKey string
	PlacementGroup   *string
	LogicalServiceID string
	RequestedWorkers int
	MaximumWorkers   int
	Strategy         model.GroupingStrategy
	Profile          model.RuntimeProfile
}

type Group struct {
	RuntimeGroupID string
	WorkloadType   model.WorkloadType
	GroupKey       string
	ProfileHash    string
	Owners         []string
	ServiceIDs     []string
	State          model.SandboxState
	Healthy        bool
	WorkerCount    int
}

type Selection struct {
	GroupKey       string
	ProfileHash    string
	RuntimeGroupID string
	Existing       bool
}

func Select(request Request, existing []Group) (Selection, error) {
	if !request.WorkloadType.Valid() || request.OwnerID == "" || !request.Strategy.Valid() || request.RequestedWorkers < 0 {
		return Selection{}, errors.New("valid workload type, owner, and grouping strategy are required")
	}
	if request.Profile.WorkloadType != request.WorkloadType {
		return Selection{}, errors.New("runtime profile workload type does not match request")
	}
	profileHash, err := request.Profile.Hash()
	if err != nil {
		return Selection{}, fmt.Errorf("runtime profile: %w", err)
	}
	groupKey, err := selectKey(request)
	if err != nil {
		return Selection{}, err
	}
	selection := Selection{GroupKey: groupKey, ProfileHash: profileHash}
	candidates := append([]Group(nil), existing...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].RuntimeGroupID < candidates[j].RuntimeGroupID })
	for _, group := range candidates {
		if group.GroupKey != groupKey || group.WorkloadType != request.WorkloadType || group.ProfileHash != profileHash || !group.Healthy {
			continue
		}
		if group.State != model.StateReady && group.State != model.StateActive {
			continue
		}
		if request.MaximumWorkers > 0 && group.WorkerCount+request.RequestedWorkers > request.MaximumWorkers {
			continue
		}
		if request.LogicalServiceID != "" && slices.Contains(group.ServiceIDs, request.LogicalServiceID) {
			continue
		}
		selection.RuntimeGroupID, selection.Existing = group.RuntimeGroupID, true
		return selection, nil
	}
	return selection, nil
}

func selectKey(request Request) (string, error) {
	if request.PlacementGroup != nil {
		if len(*request.PlacementGroup) > 256 || strings.ContainsRune(*request.PlacementGroup, '\x00') {
			return "", errors.New("sandbox placement group is invalid")
		}
		return string(request.WorkloadType) + ":placement:" + base64.RawURLEncoding.EncodeToString([]byte(*request.PlacementGroup)), nil
	}
	if request.ExplicitGroupKey != "" {
		return string(request.WorkloadType) + ":explicit:" + request.ExplicitGroupKey, nil
	}
	switch request.Strategy {
	case model.GroupingIsolated:
		if request.ExecutionID == "" {
			return "", errors.New("isolated grouping requires execution ID")
		}
		return string(request.WorkloadType) + ":isolated:" + request.ExecutionID, nil
	case model.GroupingOwner:
		return string(request.WorkloadType) + ":owner:" + request.OwnerID, nil
	case model.GroupingNamespace:
		if request.Namespace == "" {
			return "", errors.New("namespace grouping requires namespace")
		}
		return string(request.WorkloadType) + ":namespace:" + request.Namespace, nil
	case model.GroupingShared:
		return string(request.WorkloadType) + ":shared", nil
	default:
		return "", fmt.Errorf("unsupported grouping strategy %q", request.Strategy)
	}
}

type WarmState string

const (
	WarmCreating WarmState = "CREATING"
	WarmReady    WarmState = "READY"
	WarmReserved WarmState = "RESERVED"
	WarmAssigned WarmState = "ASSIGNED"
	WarmFailed   WarmState = "FAILED"
)

type WarmGroup struct {
	RuntimeGroupID string    `json:"runtime_group_id"`
	ProfileHash    string    `json:"profile_hash"`
	State          WarmState `json:"state"`
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
	mu      sync.Mutex
	desired map[string]int
	groups  map[string]WarmGroup
}

func NewWarmPool() *WarmPool {
	return &WarmPool{desired: map[string]int{}, groups: map[string]WarmGroup{}}
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

func (p *WarmPool) Add(group WarmGroup) error {
	if group.RuntimeGroupID == "" || !strings.HasPrefix(group.ProfileHash, "sha256:") || (group.State != WarmCreating && group.State != WarmReady) {
		return errors.New("new warm group requires identity, profile hash, and creating/ready state")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.groups[group.RuntimeGroupID]; exists {
		return fmt.Errorf("warm group %s already exists", group.RuntimeGroupID)
	}
	p.groups[group.RuntimeGroupID] = group
	return nil
}

// Restore reconstructs durable warm-pool accounting after kernel restart.
func (p *WarmPool) Restore(group WarmGroup) error {
	if group.RuntimeGroupID == "" || !strings.HasPrefix(group.ProfileHash, "sha256:") || (group.State != WarmReady && group.State != WarmAssigned && group.State != WarmFailed) {
		return errors.New("restored warm group requires identity, profile hash, and ready/assigned/failed state")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.groups[group.RuntimeGroupID]; exists {
		return fmt.Errorf("warm group %s is already tracked", group.RuntimeGroupID)
	}
	p.groups[group.RuntimeGroupID] = group
	return nil
}

func (p *WarmPool) SetState(runtimeGroupID string, state WarmState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	group, ok := p.groups[runtimeGroupID]
	if !ok {
		return fmt.Errorf("unknown warm group %s", runtimeGroupID)
	}
	allowed := map[WarmState]map[WarmState]bool{
		WarmCreating: {WarmReady: true, WarmFailed: true},
		WarmReady:    {WarmReserved: true, WarmFailed: true},
		WarmReserved: {WarmReady: true, WarmAssigned: true, WarmFailed: true},
		WarmAssigned: {}, WarmFailed: {},
	}
	if !allowed[group.State][state] {
		return fmt.Errorf("invalid warm-group transition %s -> %s", group.State, state)
	}
	group.State = state
	p.groups[runtimeGroupID] = group
	return nil
}

func (p *WarmPool) Reserve(profileHash string) (WarmGroup, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.groups))
	for id := range p.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		group := p.groups[id]
		if group.ProfileHash == profileHash && group.State == WarmReady {
			group.State = WarmReserved
			p.groups[id] = group
			return group, true
		}
	}
	return WarmGroup{}, false
}

func (p *WarmPool) Destroy(runtimeGroupID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.groups[runtimeGroupID]; !ok {
		return fmt.Errorf("unknown warm group %s", runtimeGroupID)
	}
	delete(p.groups, runtimeGroupID)
	return nil
}

func (p *WarmPool) Desired(profileHash string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.desired[profileHash]
}

func (p *WarmPool) Groups(profileHash string, state WarmState) []WarmGroup {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]WarmGroup, 0)
	for _, group := range p.groups {
		if (profileHash == "" || group.ProfileHash == profileHash) && (state == "" || group.State == state) {
			result = append(result, group)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RuntimeGroupID < result[j].RuntimeGroupID })
	return result
}

func (p *WarmPool) Status() []PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	profiles := map[string]bool{}
	for profile := range p.desired {
		profiles[profile] = true
	}
	for _, group := range p.groups {
		profiles[group.ProfileHash] = true
	}
	keys := make([]string, 0, len(profiles))
	for profile := range profiles {
		keys = append(keys, profile)
	}
	sort.Strings(keys)
	result := make([]PoolStatus, 0, len(keys))
	for _, profile := range keys {
		status := PoolStatus{ProfileHash: profile, Desired: p.desired[profile]}
		for _, group := range p.groups {
			if group.ProfileHash != profile {
				continue
			}
			switch group.State {
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
