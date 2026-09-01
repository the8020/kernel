// Package sandbox provides shared presentation helpers for sandbox commands.
package sandbox

import (
	"sort"
	"strings"

	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

// Reason returns the shortest stable explanation of why a sandbox exists.
func Reason(inspection manager.Inspection) string {
	if inspection.Spec.Lifecycle.Warm {
		return "warm pool"
	}
	if inspection.Spec.WorkloadType == model.WorkloadService && len(inspection.Spec.ServiceIDs) > 0 {
		values := append([]string(nil), inspection.Spec.ServiceIDs...)
		sort.Strings(values)
		for index := range values {
			values[index] = "service:" + values[index]
		}
		return strings.Join(values, ", ")
	}
	owners := map[string]bool{}
	for _, worker := range inspection.Workers {
		if worker.OwnerID != "" {
			owners[worker.OwnerID] = true
		}
	}
	if len(owners) > 0 {
		values := make([]string, 0, len(owners))
		for owner := range owners {
			values = append(values, owner)
		}
		sort.Strings(values)
		return strings.Join(values, ", ")
	}
	if inspection.Spec.GroupKey != "" {
		return inspection.Spec.GroupKey
	}
	return string(inspection.Spec.WorkloadType)
}
