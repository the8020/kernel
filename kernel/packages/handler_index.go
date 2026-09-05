package packages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"the8020/kernel/deployment"
)

type packageHandlers struct {
	events []EventListener
	hooks  map[string][]HookDefinition
}

type handlerIndex struct {
	reindexMu sync.Mutex
	mu        sync.RWMutex
	packages  map[string]packageHandlers
	events    map[string][]EventListener
}

type HandlerReport struct {
	Events int `json:"events"`
	Hooks  int `json:"hooks"`
}

// EventListeners is a memory-only lookup in the last successfully published index.
func (s *Store) EventListeners(event string) []EventListener {
	s.handlers.mu.RLock()
	defer s.handlers.mu.RUnlock()
	return slices.Clone(s.handlers.events[event])
}

func (s *Store) PackageHooks(packageID, hook string) []HookDefinition {
	s.handlers.mu.RLock()
	defer s.handlers.mu.RUnlock()
	return slices.Clone(s.handlers.packages[packageID].hooks[hook])
}

// Hooks selects a deterministic chain across declaring packages without I/O.
func (s *Store) Hooks(hook string) []HookDefinition {
	s.handlers.mu.RLock()
	defer s.handlers.mu.RUnlock()
	var handlers []HookDefinition
	for _, item := range s.handlers.packages {
		handlers = append(handlers, item.hooks[hook]...)
	}
	sortHooks(handlers)
	return handlers
}

func (s *Store) handlerSnapshot() map[string]packageHandlers {
	s.handlers.mu.RLock()
	defer s.handlers.mu.RUnlock()
	result := make(map[string]packageHandlers, len(s.handlers.packages))
	for id, item := range s.handlers.packages {
		result[id] = item
	}
	return result
}

// ReindexHandlers replaces both handler indexes together. An empty selection
// reads all ready packages; a selection reads only those declaration folders.
// References into selected packages are refreshed from cached declarations.
func (s *Store) ReindexHandlers(ctx context.Context, packageIDs ...string) (HandlerReport, error) {
	s.handlers.reindexMu.Lock()
	defer s.handlers.reindexMu.Unlock()
	selected := map[string]bool{}
	for _, id := range packageIDs {
		if _, err := ParsePackageID(id); err != nil {
			return HandlerReport{}, err
		}
		selected[id] = true
	}
	var entries []PackageIndex
	var err error
	indexed := s.handlerSnapshot()
	if len(packageIDs) == 0 {
		indexed = map[string]packageHandlers{}
		entries, err = s.index.List(ctx)
	} else {
		for _, id := range uniqueSorted(packageIDs) {
			entry, exists, getErr := s.index.Get(ctx, id)
			if getErr != nil {
				return HandlerReport{}, getErr
			}
			delete(indexed, id)
			if exists {
				entries = append(entries, entry)
			}
		}
	}
	if err != nil {
		return HandlerReport{}, err
	}
	for _, entry := range entries {
		if entry.State != "ready" || entry.ActiveCommit == "" {
			continue
		}
		root, exists, err := s.packageDestination(entry.PackageID)
		if err != nil {
			return HandlerReport{}, err
		}
		if !exists {
			return HandlerReport{}, fmt.Errorf("package is not installed: %s", entry.PackageID)
		}
		item, err := readPackageHandlers(root, entry.PackageID)
		if err != nil {
			return HandlerReport{}, err
		}
		indexed[entry.PackageID] = item
	}
	resolved := map[string]ProgramDefinition{}
	for id, item := range indexed {
		changed := selected
		if len(packageIDs) == 0 || selected[id] {
			changed = nil
		}
		item, err = s.resolveHandlerPrograms(ctx, item, nil, changed, resolved)
		if err != nil {
			return HandlerReport{}, fmt.Errorf("%s handlers: %w", id, err)
		}
		indexed[id] = item
	}
	events := map[string][]EventListener{}
	var report HandlerReport
	for _, item := range indexed {
		report.Events += len(item.events)
		for _, handlers := range item.hooks {
			report.Hooks += len(handlers)
		}
		for _, listener := range item.events {
			events[listener.Event] = append(events[listener.Event], listener)
		}
	}
	if report.Events > 2048 {
		return HandlerReport{}, errors.New("event catalog exceeds 2048 listeners")
	}
	for _, listeners := range events {
		sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })
	}
	if err := ctx.Err(); err != nil {
		return HandlerReport{}, err
	}
	s.handlers.mu.Lock()
	s.handlers.packages, s.handlers.events = indexed, events
	s.handlers.mu.Unlock()
	return report, nil
}

func readPackageHandlers(root, packageID string) (packageHandlers, error) {
	events, err := ValidateEventListeners(root, packageID)
	if err != nil {
		return packageHandlers{}, fmt.Errorf("%s events: %w", packageID, err)
	}
	hooks, err := HookHandlers(root)
	if err != nil {
		return packageHandlers{}, fmt.Errorf("%s hooks: %w", packageID, err)
	}
	for _, handlers := range hooks {
		for i := range handlers {
			handlers[i].ID = packageID + "/" + handlers[i].ID
		}
	}
	return packageHandlers{events: events, hooks: hooks}, nil
}

func (s *Store) resolveHandlerPrograms(ctx context.Context, item packageHandlers, candidates map[string]deployment.Candidate, changed map[string]bool, resolved map[string]ProgramDefinition) (packageHandlers, error) {
	resolve := func(id string) (ProgramDefinition, bool, error) {
		identity, _, err := ParseProgramID(id)
		if err != nil {
			return ProgramDefinition{}, false, err
		}
		if changed != nil && !changed[identity.PackageID()] {
			return ProgramDefinition{}, false, nil
		}
		if program, exists := resolved[id]; exists {
			return program, true, nil
		}
		program, err := s.resolveProgram(ctx, id, candidates)
		if err == nil {
			resolved[id] = program
		}
		return program, true, err
	}
	item.events = slices.Clone(item.events)
	for i, event := range item.events {
		program, updated, err := resolve(event.ProgramID)
		if err != nil {
			return packageHandlers{}, err
		}
		if updated {
			item.events[i].ProgramCommit = program.Commit
		}
	}
	hooks := make(map[string][]HookDefinition, len(item.hooks))
	for trigger, handlers := range item.hooks {
		handlers = slices.Clone(handlers)
		for i, handler := range handlers {
			program, updated, err := resolve(handler.ProgramID)
			if err != nil {
				return packageHandlers{}, err
			}
			if updated {
				handlers[i].Program = program
			}
		}
		hooks[trigger] = handlers
	}
	item.hooks = hooks
	return item, ctx.Err()
}

// Candidate indexes are isolated from live dispatch until source publication.
// Both synchronous phases consume this validated snapshot, including on recovery.
func (s *Store) indexCandidateHandlers(ctx context.Context, candidates []deployment.Candidate) (map[string]packageHandlers, error) {
	selected := map[string]deployment.Candidate{}
	changed := map[string]bool{}
	for _, candidate := range candidates {
		selected[candidate.PackageID], changed[candidate.PackageID] = candidate, true
	}
	result := map[string]packageHandlers{}
	resolved := map[string]ProgramDefinition{}
	for _, candidate := range candidates {
		item, err := readPackageHandlers(candidate.Root, candidate.PackageID)
		if err == nil {
			item, err = s.resolveHandlerPrograms(ctx, item, selected, nil, resolved)
		}
		if err != nil {
			return nil, fmt.Errorf("%s handlers: %w", candidate.PackageID, err)
		}
		result[candidate.PackageID] = item
	}
	for id, item := range s.handlerSnapshot() {
		if changed[id] {
			continue
		}
		if _, err := s.resolveHandlerPrograms(ctx, item, selected, changed, resolved); err != nil {
			return nil, fmt.Errorf("%s handlers: %w", id, err)
		}
	}
	return result, ctx.Err()
}
