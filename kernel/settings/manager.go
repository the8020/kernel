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

// GlobalStore persists the explicit value of every system-wide setting.
type GlobalStore interface {
	Load(context.Context, []Definition) (map[string]any, uint64, error)
	Set(context.Context, Definition, any) (uint64, error)
}

type globalRevisionStore interface {
	Revision(context.Context) (uint64, error)
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
	global      GlobalStore
	revision    uint64
	definitions map[string]Definition
	ordered     []string
	sources     map[string]sourceValues
	configured  Values
	active      Values
	appliers    []applierBinding
}

// PersistencePaths identifies the sole node-local settings file.
type PersistencePaths struct {
	Node string
}

// New loads all setting sources with the required precedence.
func New(definitions []Definition, files PersistencePaths, startup map[string]string, lookupEnv func(string) (string, bool)) (*Manager, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if files.Node == "" {
		return nil, errors.New("node settings file is required")
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
	nodePersisted, err := loadPersisted(files.Node, manager.definitions, StorageNode)
	if err != nil {
		return nil, err
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
		if value, ok := nodePersisted[key]; ok && definition.Storage == StorageNode {
			source.persisted, source.hasPersisted = value, true
		}
		manager.sources[key] = source
		value, _ := configuredValue(definition, source)
		manager.configured[key], manager.active[key] = value, value
	}
	if err := validateSnapshot(manager.configured); err != nil {
		return nil, err
	}
	return manager, nil
}

// AttachGlobal initializes missing defaults and replaces provisional global
// defaults with the database-authoritative values. It must run before runtime
// appliers and the public service plane are started.
func (m *Manager) AttachGlobal(ctx context.Context, store GlobalStore) error {
	if store == nil {
		return errors.New("global settings store is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.appliers) != 0 {
		return errors.New("global settings must attach before runtime appliers")
	}
	definitions := make([]Definition, 0)
	for _, key := range m.ordered {
		if definition := m.definitions[key]; definition.Storage == StorageGlobal {
			definitions = append(definitions, definition)
		}
	}
	values, revision, err := store.Load(ctx, definitions)
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		raw, exists := values[definition.Key]
		if !exists {
			return fmt.Errorf("global setting %s is missing after initialization", definition.Key)
		}
		value, err := normalizeValue(definition, raw)
		if err != nil {
			return &OperationError{Kind: ErrorInvalid, Key: definition.Key, Err: fmt.Errorf("invalid database setting %s: %w", definition.Key, err)}
		}
		source := m.sources[definition.Key]
		source.persisted, source.hasPersisted = value, true
		m.sources[definition.Key] = source
	}
	for _, key := range m.ordered {
		value, _ := configuredValue(m.definitions[key], m.sources[key])
		m.configured[key], m.active[key] = value, value
	}
	if err := validateSnapshot(m.configured); err != nil {
		return err
	}
	m.global, m.revision = store, revision
	return nil
}

// RefreshGlobal observes a shared revision before loading values, then applies
// only a newer database snapshot. Restart-required values become visibly
// pending; runtime-mutable values use the same prepare/commit boundary as a
// local settings command.
func (m *Manager) RefreshGlobal(ctx context.Context) (bool, error) {
	m.mu.RLock()
	store, currentRevision := m.global, m.revision
	definitions := make([]Definition, 0)
	for _, key := range m.ordered {
		if definition := m.definitions[key]; definition.Storage == StorageGlobal {
			definitions = append(definitions, definition)
		}
	}
	m.mu.RUnlock()
	if store == nil {
		return false, errors.New("global settings database is unavailable")
	}
	if revisions, ok := store.(globalRevisionStore); ok {
		revision, err := revisions.Revision(ctx)
		if err != nil {
			return false, err
		}
		if revision <= currentRevision {
			return false, nil
		}
	}
	values, revision, err := store.Load(ctx, definitions)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if revision <= m.revision {
		return false, nil
	}
	sources := cloneSources(m.sources)
	for _, definition := range definitions {
		value, exists := values[definition.Key]
		if !exists {
			return false, fmt.Errorf("global setting %s is missing after refresh", definition.Key)
		}
		value, err = normalizeValue(definition, value)
		if err != nil {
			return false, &OperationError{Kind: ErrorInvalid, Key: definition.Key, Err: err}
		}
		source := sources[definition.Key]
		source.persisted, source.hasPersisted = value, true
		sources[definition.Key] = source
	}
	candidate := Values{}
	for _, key := range m.ordered {
		candidate[key], _ = configuredValue(m.definitions[key], sources[key])
	}
	if err := validateSnapshot(candidate); err != nil {
		return false, err
	}
	prepared := make([]Prepared, 0)
	for _, binding := range m.appliers {
		changed := false
		for key := range binding.keys {
			definition := m.definitions[key]
			if definition.Storage == StorageGlobal && definition.RuntimeMutable && !equal(candidate[key], m.active[key]) {
				changed = true
				break
			}
		}
		if !changed {
			continue
		}
		change, err := binding.applier.Prepare(ctx, cloneValues(candidate))
		if err != nil {
			for _, item := range prepared {
				item.Discard()
			}
			return false, err
		}
		prepared = append(prepared, change)
	}
	for _, definition := range definitions {
		if definition.RuntimeMutable && !equal(candidate[definition.Key], m.active[definition.Key]) && m.applierFor(definition.Key) == nil {
			for _, item := range prepared {
				item.Discard()
			}
			return false, fmt.Errorf("setting %s has no runtime owner", definition.Key)
		}
	}
	for _, item := range prepared {
		item.Commit()
	}
	m.sources, m.configured, m.revision = sources, candidate, revision
	for _, definition := range definitions {
		if definition.RuntimeMutable {
			m.active[definition.Key] = candidate[definition.Key]
		}
	}
	return true, nil
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
	if m.definitions[key].Storage == StorageGlobal {
		source.persisted, source.hasPersisted = m.definitions[key].Default, true
	} else {
		source.persisted, source.hasPersisted = nil, false
	}
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
		if m.global == nil {
			persistErr = errors.New("global settings database is unavailable")
		} else {
			m.revision, persistErr = m.global.Set(ctx, definition, sources[changed].persisted)
		}
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
	minimum, minimumOK := values["services.default_minimum_workers"].(int64)
	maximum, maximumOK := values["services.default_maximum_workers"].(int64)
	if minimumOK && maximumOK && maximum != 0 && minimum > maximum {
		return errors.New("services.default_maximum_workers must be zero or greater than or equal to services.default_minimum_workers")
	}
	maximumOpenConnections, openOK := values["database.maximum_open_connections"].(int64)
	maximumIdleConnections, idleOK := values["database.maximum_idle_connections"].(int64)
	if openOK && idleOK && maximumIdleConnections > maximumOpenConnections {
		return errors.New("database.maximum_idle_connections must be less than or equal to database.maximum_open_connections")
	}
	return nil
}

func loadPersisted(path string, definitions map[string]Definition, storage Storage) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s settings: %w", storage, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var nested map[string]any
	if err := toml.Unmarshal(data, &nested); err != nil {
		return nil, fmt.Errorf("parse %s settings: %w", storage, err)
	}
	flat := map[string]any{}
	if err := flatten("", nested, flat); err != nil {
		return nil, err
	}
	for key, raw := range flat {
		definition, ok := definitions[key]
		if !ok {
			return nil, &OperationError{Kind: ErrorUnknown, Key: key, Err: fmt.Errorf("unknown %s setting %q", storage, key)}
		}
		value, err := normalizeValue(definition, raw)
		if err != nil {
			return nil, &OperationError{Kind: ErrorInvalid, Key: key, Err: fmt.Errorf("invalid %s setting %s: %w", storage, key, err)}
		}
		if definition.Storage != storage {
			return nil, &OperationError{Kind: ErrorPersistence, Key: key, Err: fmt.Errorf("setting %s belongs in the %s settings store, not %s", key, definition.Storage, storage)}
		}
		flat[key] = value
	}
	return flat, nil
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
