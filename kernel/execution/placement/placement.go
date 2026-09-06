// Package placement selects compatible sandboxes.
package placement

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

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

type Candidate struct {
	SandboxID    string
	WorkloadType model.WorkloadType
	GroupKey     string
	ProfileHash  string
	Owners       []string
	ServiceIDs   []string
	State        model.SandboxState
	Healthy      bool
	WorkerCount  int
}

type Selection struct {
	GroupKey    string
	ProfileHash string
	SandboxID   string
	Existing    bool
}

func Select(request Request, existing []Candidate) (Selection, error) {
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
	candidates := append([]Candidate(nil), existing...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SandboxID < candidates[j].SandboxID })
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
		selection.SandboxID, selection.Existing = group.SandboxID, true
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
