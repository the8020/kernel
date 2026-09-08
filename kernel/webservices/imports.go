package webservices

import (
	"context"
	"errors"
	"os"
	"slices"

	"the8020/kernel/execution/workers"
)

// MatchingImports scans only currently routed service Workers. Import sets stay
// in their owning Workers; neither routing nor periodic maintenance reads them.
func (m *Manager) MatchingImports(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if m.matchImports == nil {
		return nil, errors.New("Worker import inspection is unavailable")
	}
	type selection struct {
		service, sandbox string
		workers          []string
	}
	m.mu.Lock()
	var selections []selection
	for id, service := range m.services {
		if service.status.State == StateDraining || !service.definition.Enabled {
			continue
		}
		for _, sandbox := range service.sandboxes {
			if sandbox.status.Version == service.status.LoadedVersion && len(sandbox.status.WorkerIDs) > 0 {
				selections = append(selections, selection{id, sandbox.status.SandboxID, slices.Clone(sandbox.status.WorkerIDs)})
			}
		}
	}
	m.mu.Unlock()
	matched := map[string]bool{}
	for _, selected := range selections {
		if matched[selected.service] {
			continue
		}
		matches, err := m.matchImports(ctx, selected.sandbox, selected.workers, paths)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, workers.ErrRuntimeUnavailable) {
			continue
		}
		if err != nil {
			return nil, err
		}
		matched[selected.service] = len(matches) > 0
	}
	var result []string
	for id, match := range matched {
		if match {
			result = append(result, id)
		}
	}
	slices.Sort(result)
	return result, nil
}
