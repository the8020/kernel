package settings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func integerPointer(value int64) *int64 { return &value }
func testDefinitions() []Definition {
	return []Definition{
		{Key: "network.main_port", Type: TypeInteger, Storage: StorageNode, Default: int64(8080), Environment: "THE8020_NETWORK_MAIN_PORT", Minimum: integerPointer(1), Maximum: integerPointer(65535), RuntimeMutable: true, Description: "port"},
		{Key: "logging.enabled", Type: TypeBoolean, Storage: StorageNode, Default: true, Environment: "THE8020_LOGGING_ENABLED", RuntimeMutable: true, Description: "enabled"},
		{Key: "logging.split_period", Type: TypeEnum, Storage: StorageNode, Default: "day", Environment: "THE8020_LOGGING_SPLIT_PERIOD", Allowed: []string{"none", "day"}, RuntimeMutable: true, Description: "period"},
		{Key: "logging.max_file_size", Type: TypeByteSize, Storage: StorageNode, Default: "1GB", Environment: "THE8020_LOGGING_MAX_FILE_SIZE", RuntimeMutable: true, Description: "file"},
		{Key: "logging.max_total_size", Type: TypeByteSize, Storage: StorageNode, Default: "10GB", Environment: "THE8020_LOGGING_MAX_TOTAL_SIZE", RuntimeMutable: true, Description: "total"},
		{Key: "database.maximum_open_connections", Type: TypeInteger, Storage: StorageNode, Default: int64(32), Environment: "THE8020_DATABASE_MAXIMUM_OPEN_CONNECTIONS", Minimum: integerPointer(1), RuntimeMutable: true, Description: "open database connections"},
		{Key: "database.maximum_idle_connections", Type: TypeInteger, Storage: StorageNode, Default: int64(8), Environment: "THE8020_DATABASE_MAXIMUM_IDLE_CONNECTIONS", Minimum: integerPointer(0), RuntimeMutable: true, Description: "idle database connections"},
		{Key: "network.root_alias", Type: TypeString, Storage: StorageGlobal, Default: "the8020/uui/shell/", Environment: "THE8020_NETWORK_ROOT_ALIAS", Pattern: `^[A-Za-z0-9_-]+(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*/?$`, RestartRequired: true, Description: "root alias"},
		{Key: "platform.display_name", Type: TypeString, Storage: StorageGlobal, Default: "80|20", Environment: "THE8020_PLATFORM_DISPLAY_NAME", RestartRequired: true, Description: "display name"},
	}
}

type preparedTest struct{ committed, discarded *bool }

func (p preparedTest) Commit()  { *p.committed = true }
func (p preparedTest) Discard() { *p.discarded = true }

type testApplier struct {
	fail                 error
	committed, discarded bool
}

func (a *testApplier) Prepare(context.Context, Values) (Prepared, error) {
	if a.fail != nil {
		return nil, a.fail
	}
	return preparedTest{&a.committed, &a.discarded}, nil
}

func newPersistencePaths(t *testing.T) PersistencePaths {
	t.Helper()
	root := t.TempDir()
	paths := PersistencePaths{Node: filepath.Join(root, "kernel.toml")}
	if err := os.MkdirAll(filepath.Dir(paths.Node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return paths
}

type memoryGlobalStore struct {
	mu       sync.Mutex
	values   map[string]any
	revision uint64
	fail     error
}

func (s *memoryGlobalStore) Load(_ context.Context, definitions []Definition) (map[string]any, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, 0, s.fail
	}
	if s.values == nil {
		s.values = map[string]any{}
	}
	for _, definition := range definitions {
		if _, exists := s.values[definition.Key]; !exists {
			s.values[definition.Key] = definition.Default
		}
	}
	result := make(map[string]any, len(s.values))
	for key, value := range s.values {
		result[key] = value
	}
	return result, s.revision, nil
}

func (s *memoryGlobalStore) Set(_ context.Context, definition Definition, value any) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return 0, s.fail
	}
	if s.values == nil {
		s.values = map[string]any{}
	}
	s.values[definition.Key] = value
	s.revision++
	return s.revision, nil
}

func (s *memoryGlobalStore) Revision(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return 0, s.fail
	}
	return s.revision, nil
}

func attachGlobal(t *testing.T, manager *Manager, store GlobalStore) {
	t.Helper()
	if err := manager.AttachGlobal(context.Background(), store); err != nil {
		t.Fatal(err)
	}
}

func newTestManager(t *testing.T, startup map[string]string, environment map[string]string) (*Manager, PersistencePaths) {
	t.Helper()
	paths := newPersistencePaths(t)
	manager, err := New(testDefinitions(), paths, startup, func(name string) (string, bool) { value, ok := environment[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	attachGlobal(t, manager, &memoryGlobalStore{})
	return manager, paths
}

func TestPrecedenceAndNodePersistedRemoval(t *testing.T) {
	manager, paths := newTestManager(t, map[string]string{"network.main_port": "8082"}, map[string]string{"THE8020_NETWORK_MAIN_PORT": "8081"})
	applier := &testApplier{}
	if err := manager.RegisterApplier([]string{"network.main_port"}, applier); err != nil {
		t.Fatal(err)
	}
	info, _ := manager.Get("network.main_port")
	if info.ConfiguredValue != int64(8082) || info.Source != "startup_argument" {
		t.Fatalf("startup precedence: %#v", info)
	}
	if _, err := manager.Set(context.Background(), "network.main_port", "8083"); err != nil {
		t.Fatal(err)
	}
	info, _ = manager.Get("network.main_port")
	if info.ConfiguredValue != int64(8083) || info.Source != "persisted" || !applier.committed {
		t.Fatalf("persisted precedence: %#v", info)
	}
	data, err := os.ReadFile(paths.Node)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "main_port = 8083") {
		t.Fatalf("unexpected persistence: %s", data)
	}
	if temporary, _ := filepath.Glob(filepath.Join(filepath.Dir(paths.Node), ".settings-*.toml")); len(temporary) != 0 {
		t.Fatalf("temporary settings files remain: %v", temporary)
	}
	fileInfo, err := os.Stat(paths.Node)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v", fileInfo.Mode().Perm())
	}
	applier.committed = false
	if _, err := manager.Unset(context.Background(), "network.main_port"); err != nil {
		t.Fatal(err)
	}
	info, _ = manager.Get("network.main_port")
	if info.ConfiguredValue != int64(8082) || info.Source != "startup_argument" || !applier.committed {
		t.Fatalf("unset precedence: %#v", info)
	}
	data, err = os.ReadFile(paths.Node)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "main_port") {
		t.Fatalf("unset value remains persisted: %s", data)
	}
}

func TestGlobalSettingUsesGlobalStoreAndSurvivesRestart(t *testing.T) {
	paths := newPersistencePaths(t)
	store := &memoryGlobalStore{}
	manager, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	attachGlobal(t, manager, store)
	info, err := manager.Set(context.Background(), "network.root_alias", "example/demo/shell/")
	if err != nil {
		t.Fatal(err)
	}
	if info.Storage != StorageGlobal || info.ConfiguredValue != "example/demo/shell/" || info.ActiveValue != "the8020/uui/shell/" || !info.RestartPending {
		t.Fatalf("global pending state: %#v", info)
	}
	node, err := os.ReadFile(paths.Node)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(node), "root_alias") {
		t.Fatalf("global setting leaked into node configuration: %q", node)
	}
	restarted, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	attachGlobal(t, restarted, store)
	info, err = restarted.Get("network.root_alias")
	if err != nil {
		t.Fatal(err)
	}
	if info.ConfiguredValue != "example/demo/shell/" || info.ActiveValue != "example/demo/shell/" || info.Source != "persisted" || info.RestartPending {
		t.Fatalf("global state after restart: %#v", info)
	}
	info, err = restarted.Unset(context.Background(), "network.root_alias")
	if err != nil {
		t.Fatal(err)
	}
	if info.ConfiguredValue != "the8020/uui/shell/" || !info.RestartPending {
		t.Fatalf("global unset state: %#v", info)
	}
	store.mu.Lock()
	stored := store.values["network.root_alias"]
	store.mu.Unlock()
	if stored != "the8020/uui/shell/" {
		t.Fatalf("unset did not persist the recommended default: %#v", stored)
	}
}

func TestGlobalWritersMergeUnrelatedOverrides(t *testing.T) {
	paths := newPersistencePaths(t)
	store := &memoryGlobalStore{}
	first, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	attachGlobal(t, first, store)
	attachGlobal(t, second, store)
	if _, err := first.Set(context.Background(), "network.root_alias", "example/demo/shell/"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Set(context.Background(), "platform.display_name", "Example Platform"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	rootAlias, displayName := store.values["network.root_alias"], store.values["platform.display_name"]
	store.mu.Unlock()
	if rootAlias != "example/demo/shell/" || displayName != "Example Platform" {
		t.Fatalf("global values = %#v, %#v", rootAlias, displayName)
	}
	if _, err := first.Unset(context.Background(), "network.root_alias"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	rootAlias, displayName = store.values["network.root_alias"], store.values["platform.display_name"]
	store.mu.Unlock()
	if rootAlias != "the8020/uui/shell/" || displayName != "Example Platform" {
		t.Fatalf("global unset changed unrelated values: %#v, %#v", rootAlias, displayName)
	}
}

func TestRefreshGlobalObservesOnlyNewRevisions(t *testing.T) {
	store := &memoryGlobalStore{}
	paths := newPersistencePaths(t)
	first, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	attachGlobal(t, first, store)
	attachGlobal(t, second, store)
	if _, err := first.Set(context.Background(), "network.root_alias", "example/new/root/"); err != nil {
		t.Fatal(err)
	}
	changed, err := second.RefreshGlobal(context.Background())
	if err != nil || !changed {
		t.Fatalf("refresh changed=%t err=%v", changed, err)
	}
	info, _ := second.Get("network.root_alias")
	if info.ConfiguredValue != "example/new/root/" || info.ActiveValue != "the8020/uui/shell/" || !info.RestartPending {
		t.Fatalf("refreshed setting=%#v", info)
	}
	if changed, err := second.RefreshGlobal(context.Background()); err != nil || changed {
		t.Fatalf("unchanged refresh changed=%t err=%v", changed, err)
	}
}

func TestRefreshGlobalCommitsRuntimeMutableValues(t *testing.T) {
	definitions := append(testDefinitions(), Definition{Key: "platform.banner", Type: TypeString, Storage: StorageGlobal, Default: "first", Environment: "THE8020_PLATFORM_BANNER", RuntimeMutable: true, Description: "banner"})
	store := &memoryGlobalStore{}
	manager, err := New(definitions, newPersistencePaths(t), nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	attachGlobal(t, manager, store)
	applier := &testApplier{}
	if err := manager.RegisterApplier([]string{"platform.banner"}, applier); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.values["platform.banner"] = "second"
	store.revision++
	store.mu.Unlock()
	if changed, err := manager.RefreshGlobal(context.Background()); err != nil || !changed || !applier.committed {
		t.Fatalf("refresh changed=%t committed=%t err=%v", changed, applier.committed, err)
	}
	info, _ := manager.Get("platform.banner")
	if info.ConfiguredValue != "second" || info.ActiveValue != "second" || info.RestartPending {
		t.Fatalf("runtime refreshed setting=%#v", info)
	}
}

func TestPhaseOneDefaults(t *testing.T) {
	manager, _ := newTestManager(t, nil, nil)
	wants := map[string]any{"network.main_port": int64(8080), "logging.enabled": true, "logging.split_period": "day", "logging.max_file_size": "1GB", "logging.max_total_size": "10GB"}
	for key, want := range wants {
		info, err := manager.Get(key)
		if err != nil || info.ConfiguredValue != want || info.ActiveValue != want || info.Source != "default" {
			t.Errorf("%s default = %#v, error %v", key, info, err)
		}
	}
}

func TestPersistedHasHighestInitialPrecedence(t *testing.T) {
	paths := newPersistencePaths(t)
	if err := os.WriteFile(paths.Node, []byte("[network]\nmain_port = 8083\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(testDefinitions(), paths, map[string]string{"network.main_port": "8082"}, func(name string) (string, bool) {
		if name == "THE8020_NETWORK_MAIN_PORT" {
			return "8081", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := manager.Get("network.main_port")
	if info.ConfiguredValue != int64(8083) || info.ActiveValue != int64(8083) || info.Source != "persisted" {
		t.Fatalf("wrong initial state: %#v", info)
	}
}

func TestRestartRequiredSettingPersistsWithoutChangingActiveValue(t *testing.T) {
	paths := newPersistencePaths(t)
	definitions := append(testDefinitions(), Definition{
		Key:             "sandbox.runtime_name",
		Type:            TypeString,
		Storage:         StorageNode,
		Default:         "io.containerd.runsc.v1",
		Environment:     "THE8020_SANDBOX_RUNTIME_NAME",
		RestartRequired: true,
		Description:     "runtime",
	})
	manager, err := New(definitions, paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	info, err := manager.Set(context.Background(), "sandbox.runtime_name", "io.containerd.alt.v1")
	if err != nil {
		t.Fatal(err)
	}
	if info.ConfiguredValue != "io.containerd.alt.v1" || info.ActiveValue != "io.containerd.runsc.v1" || !info.RestartPending {
		t.Fatalf("pending restart state: %#v", info)
	}
	data, err := os.ReadFile(paths.Node)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `runtime_name = "io.containerd.alt.v1"`) {
		t.Fatalf("restart setting was not persisted: %s", data)
	}

	restarted, err := New(definitions, paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	info, err = restarted.Get("sandbox.runtime_name")
	if err != nil {
		t.Fatal(err)
	}
	if info.ConfiguredValue != "io.containerd.alt.v1" || info.ActiveValue != "io.containerd.alt.v1" || info.RestartPending {
		t.Fatalf("state after restart: %#v", info)
	}
}

func TestApplicationAndPersistenceFailuresPreserveState(t *testing.T) {
	manager, _ := newTestManager(t, nil, nil)
	applier := &testApplier{fail: errors.New("prepare failed")}
	_ = manager.RegisterApplier([]string{"network.main_port"}, applier)
	if _, err := manager.Set(context.Background(), "network.main_port", "8081"); err == nil {
		t.Fatal("expected preparation failure")
	}
	info, _ := manager.Get("network.main_port")
	if info.ConfiguredValue != int64(8080) || info.ActiveValue != int64(8080) {
		t.Fatalf("state changed after prepare failure: %#v", info)
	}

	root := t.TempDir()
	brokenPaths := PersistencePaths{Node: filepath.Join(root, "missing-node", "kernel.toml")}
	broken, err := New(testDefinitions(), brokenPaths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	prepared := &testApplier{}
	_ = broken.RegisterApplier([]string{"network.main_port"}, prepared)
	if _, err := broken.Set(context.Background(), "network.main_port", "8081"); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !prepared.discarded || prepared.committed {
		t.Fatalf("prepared resource lifecycle: %#v", prepared)
	}
	info, _ = broken.Get("network.main_port")
	if info.ConfiguredValue != int64(8080) || info.ActiveValue != int64(8080) {
		t.Fatalf("state changed after persistence failure: %#v", info)
	}
}

func TestGlobalPersistenceFailurePreservesState(t *testing.T) {
	paths := newPersistencePaths(t)
	store := &memoryGlobalStore{fail: errors.New("database unavailable")}
	manager, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	// Attach succeeds before simulating the database write failure.
	store.fail = nil
	attachGlobal(t, manager, store)
	store.fail = errors.New("database unavailable")
	if _, err := manager.Set(context.Background(), "network.root_alias", "example/demo/shell/"); err == nil {
		t.Fatal("expected global persistence failure")
	}
	info, _ := manager.Get("network.root_alias")
	if info.ConfiguredValue != "the8020/uui/shell/" || info.ActiveValue != "the8020/uui/shell/" || info.RestartPending {
		t.Fatalf("global state changed after persistence failure: %#v", info)
	}
}

func TestGlobalOverrideInNodeStoreFails(t *testing.T) {
	paths := newPersistencePaths(t)
	if err := os.WriteFile(paths.Node, []byte("[network]\nroot_alias = \"example/demo/shell/\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "belongs in the global settings store") {
		t.Fatalf("wrong-store error = %v", err)
	}
}

func TestInvalidStorageDefinitionsFail(t *testing.T) {
	base := Definition{Key: "test.value", Type: TypeString, Default: "value", Environment: "THE8020_TEST_VALUE", Description: "test"}
	for name, mutate := range map[string]func(*Definition){
		"missing": func(*Definition) {},
		"invalid": func(definition *Definition) { definition.Storage = "cluster" },
	} {
		t.Run(name, func(t *testing.T) {
			definition := base
			mutate(&definition)
			if _, err := ValidateDefinition(definition); err == nil {
				t.Fatalf("accepted definition: %#v", definition)
			}
		})
	}
}

func TestSettingEnvironmentPrefixIsRequired(t *testing.T) {
	definition := testDefinitions()[0]
	definition.Environment = "KERNEL_NETWORK_MAIN_PORT"
	if _, err := ValidateDefinition(definition); err == nil {
		t.Fatal("accepted setting environment variable without THE8020_ prefix")
	}
}

func TestConversionsAndCrossValidation(t *testing.T) {
	for input, want := range map[string]ByteSize{"0B": 0, "1KB": 1000, "10MB": 10_000_000, "1GB": 1_000_000_000, "123B": 123} {
		value, err := parseByteSize(input)
		if err != nil || value != want {
			t.Errorf("ParseByteSize(%q) = %d, %v", input, value, err)
		}
	}
	minimum := int64(0)
	if _, err := ValidateDefinition(Definition{Key: "runtime.node.auto_budget", Type: TypeByteSize, Storage: StorageNode, Default: "0B", Environment: "THE8020_RUNTIME_NODE_AUTO_BUDGET", Minimum: &minimum, RestartRequired: true, Description: "auto"}); err != nil {
		t.Fatalf("zero byte-size sentinel rejected: %v", err)
	}
	if _, err := ValidateDefinition(Definition{Key: "runtime.node.invalid_budget", Type: TypeByteSize, Storage: StorageNode, Default: "0B", Environment: "THE8020_RUNTIME_NODE_INVALID_BUDGET", RestartRequired: true, Description: "invalid"}); err == nil {
		t.Fatal("zero byte size accepted without explicit minimum")
	}
	definition := testDefinitions()[0]
	if _, err := parse(definition, "0"); err == nil {
		t.Error("accepted port zero")
	}
	if _, err := parse(definition, "65536"); err == nil {
		t.Error("accepted high port")
	}
	if _, err := parse(testDefinitions()[2], "hour"); err == nil {
		t.Error("accepted an enum value outside the definition")
	}
	manager, _ := newTestManager(t, nil, nil)
	applier := &testApplier{}
	_ = manager.RegisterApplier([]string{"logging.max_file_size", "logging.max_total_size"}, applier)
	if _, err := manager.Set(context.Background(), "logging.max_file_size", "11GB"); err == nil {
		t.Fatal("accepted file limit above total limit")
	}
	serviceDefaults := Values{
		"services.default_minimum_workers": int64(1), "services.default_maximum_workers": int64(4),
	}
	if err := validateSnapshot(serviceDefaults); err != nil {
		t.Fatalf("valid service defaults: %v", err)
	}
	serviceDefaults["services.default_minimum_workers"] = int64(5)
	if err := validateSnapshot(serviceDefaults); err == nil || !strings.Contains(err.Error(), "greater than or equal") {
		t.Fatalf("invalid service defaults error = %v", err)
	}
	serviceDefaults["services.default_maximum_workers"] = int64(0)
	if err := validateSnapshot(serviceDefaults); err != nil {
		t.Fatalf("unlimited service maximum rejected: %v", err)
	}
	databasePool := Values{
		"database.maximum_open_connections": int64(32), "database.maximum_idle_connections": int64(8),
	}
	if err := validateSnapshot(databasePool); err != nil {
		t.Fatalf("valid database pool: %v", err)
	}
	databasePool["database.maximum_idle_connections"] = int64(33)
	if err := validateSnapshot(databasePool); err == nil || !strings.Contains(err.Error(), "less than or equal") {
		t.Fatalf("invalid database pool error = %v", err)
	}
}

func TestUnknownPersistedSettingIsRejected(t *testing.T) {
	paths := newPersistencePaths(t)
	if err := os.WriteFile(paths.Node, []byte("[unsupported]\nvalue = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), `unknown node setting "unsupported.value"`) {
		t.Fatalf("unknown-setting error = %v", err)
	}
}

func TestStringPatternValidation(t *testing.T) {
	definition := Definition{
		Key: "network.root_alias", Type: TypeString, Storage: StorageGlobal,
		Default: "the8020/uui/shell/", Environment: "THE8020_NETWORK_ROOT_ALIAS",
		Pattern:         `^[A-Za-z0-9_-]+(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*/?$`,
		RestartRequired: true, Description: "root alias",
	}
	validated, err := ValidateDefinition(definition)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := parse(validated, "example/auth/login/"); err != nil || value != "example/auth/login/" {
		t.Fatalf("valid path = %#v, %v", value, err)
	}
	for _, value := range []string{"/absolute", "../escape", "example//shell", "example/shell?query=1"} {
		if _, err := parse(validated, value); err == nil {
			t.Errorf("accepted invalid path %q", value)
		}
	}
	definition.Pattern = "["
	if _, err := ValidateDefinition(definition); err == nil {
		t.Fatal("accepted invalid setting pattern")
	}
	definition.Pattern = "^value$"
	definition.Type = TypeInteger
	definition.Default = int64(1)
	if _, err := ValidateDefinition(definition); err == nil {
		t.Fatal("accepted pattern on non-string setting")
	}
}

func TestPersistedUnknownAndInvalidValuesFailLoading(t *testing.T) {
	for _, content := range []string{"[unknown]\nvalue = 1\n", "[network]\nmain_port = 70000\n"} {
		paths := newPersistencePaths(t)
		_ = os.WriteFile(paths.Node, []byte(content), 0o600)
		if _, err := New(testDefinitions(), paths, nil, func(string) (string, bool) { return "", false }); err == nil {
			t.Fatalf("accepted %q", content)
		}
	}
}
