package settings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"
)

// ErrorKind identifies a settings-owned failure without coupling the package to transport errors.
type ErrorKind string

const (
	ErrorUnknown     ErrorKind = "unknown"
	ErrorInvalid     ErrorKind = "invalid"
	ErrorNotMutable  ErrorKind = "not_mutable"
	ErrorPersistence ErrorKind = "persistence"
	ErrorApplication ErrorKind = "application"
)

// OperationError describes a failed setting operation.
type OperationError struct {
	Kind ErrorKind
	Key  string
	Err  error
}

func (e *OperationError) Error() string { return e.Err.Error() }
func (e *OperationError) Unwrap() error { return e.Err }

// Values is an immutable-by-convention configured settings snapshot.
type Values map[string]any

// Prepared is a runtime resource prepared before settings persistence.
// Commit must only swap already-prepared resources and cannot fail.
type Prepared interface {
	Commit()
	Discard()
}

// Applier prepares runtime state for a complete candidate snapshot.
type Applier interface {
	Prepare(context.Context, Values) (Prepared, error)
}

type sourceValues struct {
	environment, startup, persisted          any
	hasEnvironment, hasStartup, hasPersisted bool
}

// Info is the complete observable state of one setting.
type Info struct {
	Key              string  `json:"key"`
	Description      string  `json:"description"`
	Storage          Storage `json:"storage"`
	ConfiguredValue  any     `json:"configured_value"`
	ActiveValue      any     `json:"active_value"`
	Source           string  `json:"source"`
	DefaultValue     any     `json:"default_value"`
	PersistedValue   any     `json:"persisted_value,omitempty"`
	EnvironmentValue any     `json:"environment_value,omitempty"`
	StartupValue     any     `json:"startup_argument_value,omitempty"`
	RuntimeMutable   bool    `json:"runtime_mutable"`
	RestartRequired  bool    `json:"restart_required"`
	RestartPending   bool    `json:"restart_pending"`
}

type applierBinding struct {
	keys    map[string]struct{}
	applier Applier
}

// Manager serializes settings transactions and owns configured and active values.
type Manager struct {
	mu          sync.RWMutex
	files       PersistencePaths
	definitions map[string]Definition
	ordered     []string
	sources     map[string]sourceValues
	configured  Values
	active      Values
	appliers    []applierBinding
}

// PersistencePaths separates node-local and shared global override files.
type PersistencePaths struct {
	Node   string
	Global string
}

// New loads all setting sources with the required precedence.
func New(definitions []Definition, files PersistencePaths, startup map[string]string, lookupEnv func(string) (string, bool)) (*Manager, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if files.Node == "" || files.Global == "" || files.Node == files.Global {
		return nil, errors.New("distinct node and global settings files are required")
	}
	manager := &Manager{files: files, definitions: make(map[string]Definition), sources: make(map[string]sourceValues), configured: Values{}, active: Values{}}
	for _, raw := range definitions {
		definition, err := ValidateDefinition(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := manager.definitions[definition.Key]; exists {
			return nil, fmt.Errorf("duplicate setting %s", definition.Key)
		}
		manager.definitions[definition.Key] = definition
		manager.ordered = append(manager.ordered, definition.Key)
	}
	sort.Strings(manager.ordered)
	for key := range startup {
		if _, ok := manager.definitions[key]; !ok {
			return nil, &OperationError{Kind: ErrorUnknown, Key: key, Err: fmt.Errorf("unknown setting %q", key)}
		}
	}
	nodePersisted, legacyGlobal, err := loadPersisted(files.Node, manager.definitions, StorageNode, true)
	if err != nil {
		return nil, err
	}
	globalPersisted, _, err := loadPersisted(files.Global, manager.definitions, StorageGlobal, false)
	if err != nil {
		return nil, err
	}
	if len(legacyGlobal) > 0 {
		globalPersisted, err = mergeGlobalPersisted(context.Background(), files.Global, manager.definitions, legacyGlobal)
		if err != nil {
			return nil, fmt.Errorf("migrate global settings: %w", err)
		}
	}
	for _, key := range manager.ordered {
		definition := manager.definitions[key]
		source := sourceValues{}
		if raw, ok := lookupEnv(definition.Environment); ok {
			value, err := parse(definition, raw)
			if err != nil {
				return nil, &OperationError{Kind: ErrorInvalid, Key: key, Err: fmt.Errorf("invalid %s: %w", definition.Environment, err)}
			}
			source.environment, source.hasEnvironment = value, true
		}
		if raw, ok := startup[key]; ok {
			value, err := parse(definition, raw)
			if err != nil {
				return nil, &OperationError{Kind: ErrorInvalid, Key: key, Err: fmt.Errorf("invalid startup setting %s: %w", key, err)}
			}
			source.startup, source.hasStartup = value, true
		}
		persisted := nodePersisted
		if definition.Storage == StorageGlobal {
			persisted = globalPersisted
		}
		if value, ok := persisted[key]; ok {
			source.persisted, source.hasPersisted = value, true
		}
		manager.sources[key] = source
		value, _ := configuredValue(definition, source)
		manager.configured[key], manager.active[key] = value, value
	}
	if err := validateSnapshot(manager.configured); err != nil {
		return nil, err
	}
	if len(legacyGlobal) > 0 {
		if err := persist(files.Node, manager.definitions, manager.sources, StorageNode); err != nil {
			return nil, fmt.Errorf("remove migrated global settings from node store: %w", err)
		}
	}
	return manager, nil
}

// RegisterApplier assigns a responsible runtime owner to the listed keys.
func (m *Manager) RegisterApplier(keys []string, applier Applier) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if applier == nil {
		return errors.New("nil settings applier")
	}
	binding := applierBinding{keys: make(map[string]struct{}, len(keys)), applier: applier}
	for _, key := range keys {
		definition, ok := m.definitions[key]
		if !ok {
			return fmt.Errorf("register applier for unknown setting %s", key)
		}
		if !definition.RuntimeMutable {
			return fmt.Errorf("register applier for non-runtime setting %s", key)
		}
		for _, existing := range m.appliers {
			if _, duplicate := existing.keys[key]; duplicate {
				return fmt.Errorf("duplicate applier for %s", key)
			}
		}
		binding.keys[key] = struct{}{}
	}
	m.appliers = append(m.appliers, binding)
	return nil
}

// Snapshot returns a copy of current configured values.
func (m *Manager) Snapshot() Values {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneValues(m.configured)
}

// Active returns one internal active value for service composition.
func (m *Manager) Active(key string) (any, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.active[key]
	return value, ok
}

// List returns complete setting state in key order.
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Info, 0, len(m.ordered))
	for _, key := range m.ordered {
		result = append(result, m.infoLocked(key))
	}
	return result
}

// Get returns complete state for one setting.
func (m *Manager) Get(key string) (Info, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.definitions[key]; !ok {
		return Info{}, &OperationError{Kind: ErrorUnknown, Key: key, Err: fmt.Errorf("unknown setting %q", key)}
	}
	return m.infoLocked(key), nil
}

// Set creates or replaces the highest-precedence persisted override.
func (m *Manager) Set(ctx context.Context, key, raw string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	definition, ok := m.definitions[key]
	if !ok {
		return Info{}, &OperationError{Kind: ErrorUnknown, Key: key, Err: fmt.Errorf("unknown setting %q", key)}
	}
	value, err := parse(definition, raw)
	if err != nil {
		return Info{}, &OperationError{Kind: ErrorInvalid, Key: key, Err: fmt.Errorf("invalid value for %s: %w", key, err)}
	}
	sources := cloneSources(m.sources)
	source := sources[key]
	source.persisted, source.hasPersisted = value, true
	sources[key] = source
	return m.applyLocked(ctx, key, sources)
}

// Unset removes the persisted override and reveals the next lower-precedence source.
func (m *Manager) Unset(ctx context.Context, key string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.definitions[key]; !ok {
		return Info{}, &OperationError{Kind: ErrorUnknown, Key: key, Err: fmt.Errorf("unknown setting %q", key)}
	}
	sources := cloneSources(m.sources)
	source := sources[key]
	source.persisted, source.hasPersisted = nil, false
	sources[key] = source
	return m.applyLocked(ctx, key, sources)
}

func (m *Manager) applyLocked(ctx context.Context, changed string, sources map[string]sourceValues) (Info, error) {
	candidate := Values{}
	for _, key := range m.ordered {
		value, _ := configuredValue(m.definitions[key], sources[key])
		candidate[key] = value
	}
	if err := validateSnapshot(candidate); err != nil {
		return Info{}, &OperationError{Kind: ErrorInvalid, Key: changed, Err: err}
	}
	definition := m.definitions[changed]
	activeChanges := !equal(candidate[changed], m.active[changed])
	if activeChanges && !definition.RuntimeMutable && !definition.RestartRequired {
		return Info{}, &OperationError{Kind: ErrorNotMutable, Key: changed, Err: fmt.Errorf("setting %s is not runtime mutable", changed)}
	}
	var prepared Prepared
	if activeChanges && definition.RuntimeMutable {
		binding := m.applierFor(changed)
		if binding == nil {
			return Info{}, &OperationError{Kind: ErrorApplication, Key: changed, Err: fmt.Errorf("setting %s has no runtime owner", changed)}
		}
		var err error
		prepared, err = binding.Prepare(ctx, cloneValues(candidate))
		if err != nil {
			return Info{}, &OperationError{Kind: ErrorApplication, Key: changed, Err: err}
		}
	}
	var persistErr error
	if definition.Storage == StorageGlobal {
		persistErr = persistGlobalChange(ctx, m.files.Global, m.definitions, sources, changed)
	} else {
		persistErr = persist(m.files.Node, m.definitions, sources, StorageNode)
	}
	if persistErr != nil {
		if prepared != nil {
			prepared.Discard()
		}
		return Info{}, &OperationError{Kind: ErrorPersistence, Key: changed, Err: persistErr}
	}
	if prepared != nil {
		prepared.Commit()
	}
	m.sources, m.configured = sources, candidate
	if definition.RuntimeMutable {
		m.active[changed] = candidate[changed]
	}
	return m.infoLocked(changed), nil
}

func (m *Manager) applierFor(key string) Applier {
	for _, binding := range m.appliers {
		if _, ok := binding.keys[key]; ok {
			return binding.applier
		}
	}
	return nil
}

func configuredValue(definition Definition, source sourceValues) (any, string) {
	if source.hasPersisted {
		return source.persisted, "persisted"
	}
	if source.hasStartup {
		return source.startup, "startup_argument"
	}
	if source.hasEnvironment {
		return source.environment, "environment"
	}
	return definition.Default, "default"
}

func (m *Manager) infoLocked(key string) Info {
	definition, source := m.definitions[key], m.sources[key]
	_, sourceName := configuredValue(definition, source)
	info := Info{Key: key, Description: definition.Description, Storage: definition.Storage, ConfiguredValue: externalValue(m.configured[key]), ActiveValue: externalValue(m.active[key]), Source: sourceName, DefaultValue: externalValue(definition.Default), RuntimeMutable: definition.RuntimeMutable, RestartRequired: definition.RestartRequired, RestartPending: !equal(m.configured[key], m.active[key])}
	if source.hasPersisted {
		info.PersistedValue = externalValue(source.persisted)
	}
	if source.hasEnvironment {
		info.EnvironmentValue = externalValue(source.environment)
	}
	if source.hasStartup {
		info.StartupValue = externalValue(source.startup)
	}
	return info
}

func cloneValues(values Values) Values {
	result := make(Values, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
func cloneSources(values map[string]sourceValues) map[string]sourceValues {
	result := make(map[string]sourceValues, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func validateSnapshot(values Values) error {
	file, fileOK := values["logging.max_file_size"].(ByteSize)
	total, totalOK := values["logging.max_total_size"].(ByteSize)
	if fileOK && totalOK && total < file {
		return errors.New("logging.max_total_size must be greater than or equal to logging.max_file_size")
	}
	for _, workload := range []string{"service", "job"} {
		high, highOK := values["sandbox.resources."+workload+".memory_high"].(ByteSize)
		maximum, maximumOK := values["sandbox.resources."+workload+".memory_maximum"].(ByteSize)
		if highOK && maximumOK && high > maximum {
			return fmt.Errorf("sandbox.resources.%s.memory_high must not exceed memory_maximum", workload)
		}
	}
	minimumWorkers, minimumOK := values["service.default.minimum_workers"].(int64)
	maximumWorkers, maximumOK := values["service.default.maximum_workers"].(int64)
	if minimumOK && maximumOK && minimumWorkers > maximumWorkers {
		return errors.New("service.default.minimum_workers must not exceed service.default.maximum_workers")
	}
	for _, capacity := range []string{"replicas", "workers_per_replica"} {
		minimum, minimumOK := values["services.default_"+capacity+"_minimum"].(int64)
		maximum, maximumOK := values["services.default_"+capacity+"_maximum"].(int64)
		if minimumOK && maximumOK && minimum > maximum {
			return fmt.Errorf("services.default_%s capacity must satisfy minimum <= maximum", capacity)
		}
	}
	return nil
}

func loadPersisted(path string, definitions map[string]Definition, storage Storage, acceptLegacyGlobals bool) (map[string]any, map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s settings: %w", storage, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, map[string]any{}, nil
	}
	var nested map[string]any
	if err := toml.Unmarshal(data, &nested); err != nil {
		return nil, nil, fmt.Errorf("parse %s settings: %w", storage, err)
	}
	flat := map[string]any{}
	if err := flatten("", nested, flat); err != nil {
		return nil, nil, err
	}
	legacyGlobal := map[string]any{}
	for key, raw := range flat {
		definition, ok := definitions[key]
		if !ok {
			return nil, nil, &OperationError{Kind: ErrorUnknown, Key: key, Err: fmt.Errorf("unknown %s setting %q", storage, key)}
		}
		value, err := normalizeValue(definition, raw)
		if err != nil {
			return nil, nil, &OperationError{Kind: ErrorInvalid, Key: key, Err: fmt.Errorf("invalid %s setting %s: %w", storage, key, err)}
		}
		if definition.Storage != storage {
			if acceptLegacyGlobals && storage == StorageNode && definition.Storage == StorageGlobal {
				legacyGlobal[key] = value
				delete(flat, key)
				continue
			}
			return nil, nil, &OperationError{Kind: ErrorPersistence, Key: key, Err: fmt.Errorf("setting %s belongs in the %s settings store, not %s", key, definition.Storage, storage)}
		}
		flat[key] = value
	}
	return flat, legacyGlobal, nil
}

func flatten(prefix string, nested map[string]any, output map[string]any) error {
	for key, value := range nested {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if child, ok := value.(map[string]any); ok {
			if err := flatten(path, child, output); err != nil {
				return err
			}
			continue
		}
		output[path] = value
	}
	return nil
}
