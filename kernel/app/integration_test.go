package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"the8020/kernel/admin"
	"the8020/kernel/cbus/client"
	getcommand "the8020/kernel/cbus/commands/settings/get"
	listcommand "the8020/kernel/cbus/commands/settings/list"
	setcommand "the8020/kernel/cbus/commands/settings/set"
	unsetcommand "the8020/kernel/cbus/commands/settings/unset"
	restartcommand "the8020/kernel/cbus/commands/system/restart"
	shutdowncommand "the8020/kernel/cbus/commands/system/shutdown"
	statuscommand "the8020/kernel/cbus/commands/system/status"
	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	"the8020/kernel/instance"
	mainnetwork "the8020/kernel/network"
	"the8020/kernel/services"
	"the8020/kernel/settings"
	settingsdb "the8020/kernel/settings/dbstore"
)

func pointer(value int64) *int64 { return &value }
func definitions() []settings.Definition {
	return []settings.Definition{
		{Key: "node.id", Type: settings.TypeString, Storage: settings.StorageNode, Default: "00000000-0000-0000-0000-000000000000", Environment: "THE8020_NODE_ID", Pattern: `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`, Description: "Stable node identity."},
		{Key: "network.main_port", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(8080), Environment: "THE8020_NETWORK_MAIN_PORT", Minimum: pointer(1), Maximum: pointer(65535), RuntimeMutable: true, Description: "Main HTTP listener port."},
		{Key: "network.ssh_port", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(2222), Environment: "THE8020_NETWORK_SSH_PORT", Minimum: pointer(1), Maximum: pointer(65535), RuntimeMutable: true, Description: "SSH port."},
		{Key: "logging.enabled", Type: settings.TypeBoolean, Storage: settings.StorageNode, Default: true, Environment: "THE8020_LOGGING_ENABLED", RuntimeMutable: true, Description: "Logging enabled."},
		{Key: "logging.split_period", Type: settings.TypeEnum, Storage: settings.StorageNode, Default: "day", Environment: "THE8020_LOGGING_SPLIT_PERIOD", Allowed: []string{"none", "minute", "hour", "day", "week", "month", "year"}, RuntimeMutable: true, Description: "Split period."},
		{Key: "logging.max_file_size", Type: settings.TypeByteSize, Storage: settings.StorageNode, Default: "1GB", Environment: "THE8020_LOGGING_MAX_FILE_SIZE", RuntimeMutable: true, Description: "File size."},
		{Key: "logging.max_total_size", Type: settings.TypeByteSize, Storage: settings.StorageNode, Default: "10GB", Environment: "THE8020_LOGGING_MAX_TOTAL_SIZE", RuntimeMutable: true, Description: "Total size."},
		{Key: "database.maximum_open_connections", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(32), Environment: "THE8020_DATABASE_MAXIMUM_OPEN_CONNECTIONS", Minimum: pointer(1), RuntimeMutable: true, Description: "Maximum open database connections."},
		{Key: "database.maximum_idle_connections", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(8), Environment: "THE8020_DATABASE_MAXIMUM_IDLE_CONNECTIONS", Minimum: pointer(0), RuntimeMutable: true, Description: "Maximum idle database connections."},
		{Key: "database.maximum_result_rows", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(10_000), Environment: "THE8020_DATABASE_MAXIMUM_RESULT_ROWS", Minimum: pointer(1), RuntimeMutable: true, Description: "Maximum database result rows."},
		{Key: "database.maximum_result_bytes", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(10 << 20), Environment: "THE8020_DATABASE_MAXIMUM_RESULT_BYTES", Minimum: pointer(1), RuntimeMutable: true, Description: "Maximum database result bytes."},
		{Key: "network.root_alias", Type: settings.TypeString, Storage: settings.StorageGlobal, Default: "the8020/uui/shell/", Environment: "THE8020_NETWORK_ROOT_ALIAS", Pattern: `^[A-Za-z0-9_-]+(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*/?$`, RestartRequired: true, Description: "Root alias."},
	}
}
func catalog() []core.Command {
	return []core.Command{
		{Version: 1, ID: "kernel.config.get", Path: []string{"kernel.config.get"}, Parameters: []core.Parameter{{Name: "key", Type: "string", Required: true}}},
		{Version: 1, ID: "kernel.config.list", Path: []string{"kernel.config.list"}, Parameters: []core.Parameter{{Name: "view", Type: "string", Required: false}}},
		{Version: 1, ID: "kernel.config.set", Path: []string{"kernel.config.set"}, Parameters: []core.Parameter{{Name: "key", Type: "string", Required: true}, {Name: "value", Type: "string", Position: 1, Required: true}}},
		{Version: 1, ID: "kernel.config.unset", Path: []string{"kernel.config.unset"}, Parameters: []core.Parameter{{Name: "key", Type: "string", Required: true}}},
		{Version: 1, ID: "kernel.restart", Path: []string{"kernel.restart"}, RestartBehavior: "restarts_kernel"},
		{Version: 1, ID: "kernel.shutdown", Path: []string{"kernel.shutdown"}, RestartBehavior: "stops_kernel"},
		{Version: 1, ID: "kernel.status", Path: []string{"kernel.status"}},
	}
}
func register(registry *core.Registry, serviceSet *services.Services) error {
	commands := catalog()
	handlers := []core.Handler{getcommand.New(serviceSet), listcommand.New(serviceSet), setcommand.New(serviceSet), unsetcommand.New(serviceSet), restartcommand.New(serviceSet), shutdowncommand.New(serviceSet), statuscommand.New(serviceSet)}
	for index := range commands {
		if err := registry.Register(commands[index], handlers[index]); err != nil {
			return err
		}
	}
	return nil
}
func availablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}
func startKernel(t *testing.T, root string, startupPort int) <-chan error {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	preparedPaths, err := instance.Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Initialize(preparedPaths); err != nil {
		t.Fatal(err)
	}
	errorsChannel := make(chan error, 1)
	sshPort := availablePort(t)
	go func() {
		errorsChannel <- Run(context.Background(), Config{
			Root: root, Startup: map[string]string{"network.main_port": stringInt(startupPort), "network.ssh_port": stringInt(sshPort)},
			Definitions: definitions(), Register: register, BuildID: "integration-test", initialize: initializeIntegrationRuntime,
		})
	}()
	paths := instance.NewPaths(root)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		commandClient := client.New(paths.Socket)
		response, err := commandClient.Execute(context.Background(), core.Request{CommandID: "kernel.status"})
		commandClient.Close()
		if err == nil && response.Success && resultObject(response)["runtime_ready"] == true {
			return errorsChannel
		}
		select {
		case err := <-errorsChannel:
			t.Fatalf("kernel failed before ready: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("kernel did not become ready")
	return nil
}

func initializeIntegrationRuntime(ctx context.Context, settingManager *settings.Manager, databaseManager *database.Manager, serviceSet *services.Services) (*services.RuntimeServices, runtimeCleanupFunc) {
	cleanup := &runtimeCleanup{}
	fail := func(err error) (*services.RuntimeServices, runtimeCleanupFunc) {
		return &services.RuntimeServices{Failure: err.Error()}, cleanup.Close
	}
	if _, err := databaseManager.InitializeCatalog(ctx); err != nil {
		return fail(err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS "the8020__system__settings" ("key" TEXT PRIMARY KEY, "value" TEXT NOT NULL, "definitionHash" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE IF NOT EXISTS "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
	} {
		if _, err := databaseManager.ExecContext(ctx, statement); err != nil {
			return fail(err)
		}
	}
	global, err := settingsdb.New(databaseManager)
	if err != nil {
		return fail(err)
	}
	if err := settingManager.AttachGlobal(ctx, global); err != nil {
		return fail(err)
	}
	if err := settingManager.RegisterApplier([]string{"logging.enabled", "logging.split_period", "logging.max_file_size", "logging.max_total_size"}, serviceSet.Logging); err != nil {
		return fail(err)
	}
	if err := settingManager.RegisterApplier([]string{
		"database.maximum_open_connections", "database.maximum_idle_connections",
		"database.maximum_result_rows", "database.maximum_result_bytes",
	}, databaseManager); err != nil {
		return fail(err)
	}
	mainPort, _ := settingManager.Active("network.main_port")
	rootAlias, _ := settingManager.Active("network.root_alias")
	publicNetwork, err := mainnetwork.New(int(mainPort.(int64)), rootAlias.(string))
	if err != nil {
		return fail(err)
	}
	cleanup.publicNetwork = publicNetwork
	if err := settingManager.RegisterApplier([]string{"network.main_port"}, publicNetwork); err != nil {
		return fail(err)
	}
	sshPort, _ := settingManager.Active("network.ssh_port")
	sshListener, err := newIntegrationPort(int(sshPort.(int64)))
	if err != nil {
		return fail(err)
	}
	if err := settingManager.RegisterApplier([]string{"network.ssh_port"}, sshListener); err != nil {
		_ = sshListener.Close()
		return fail(err)
	}
	serviceSet.PublishPlatform(services.PlatformServices{Network: publicNetwork})
	if err := databaseManager.CompleteInitialization(ctx, map[string]string{}); err != nil {
		return fail(err)
	}
	return &services.RuntimeServices{}, func(ctx context.Context, report shutdownProgressFunc) error {
		return errors.Join(sshListener.Close(), cleanup.Close(ctx, report))
	}
}

type integrationPort struct {
	mu       sync.Mutex
	listener net.Listener
}

func newIntegrationPort(port int) (*integrationPort, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:"+stringInt(port))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", mainnetwork.ErrPortUnavailable, err)
	}
	return &integrationPort{listener: listener}, nil
}

func (p *integrationPort) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	port, ok := values["network.ssh_port"].(int64)
	if !ok {
		return nil, errors.New("network.ssh_port is not an integer")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+stringInt(int(port)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", mainnetwork.ErrPortUnavailable, err)
	}
	return &integrationPreparedPort{owner: p, listener: listener}, nil
}

func (p *integrationPort) Close() error {
	p.mu.Lock()
	listener := p.listener
	p.listener = nil
	p.mu.Unlock()
	if listener != nil {
		return listener.Close()
	}
	return nil
}

type integrationPreparedPort struct {
	owner    *integrationPort
	listener net.Listener
	once     sync.Once
}

func (p *integrationPreparedPort) Commit() {
	p.once.Do(func() {
		p.owner.mu.Lock()
		previous := p.owner.listener
		p.owner.listener = p.listener
		p.owner.mu.Unlock()
		if previous != nil {
			_ = previous.Close()
		}
	})
}

func (p *integrationPreparedPort) Discard() {
	p.once.Do(func() { _ = p.listener.Close() })
}

func stringInt(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	buffer := make([]byte, 0, 5)
	for value > 0 {
		buffer = append(buffer, digits[value%10])
		value /= 10
	}
	for left, right := 0, len(buffer)-1; left < right; left, right = left+1, right-1 {
		buffer[left], buffer[right] = buffer[right], buffer[left]
	}
	return string(buffer)
}

func assertRootRedirect(t *testing.T, port int, want string) {
	t.Helper()
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("http://127.0.0.1:" + stringInt(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || response.Header.Get("Location") != want {
		t.Fatalf("root redirect = %d %q, want %d %q", response.StatusCode, response.Header.Get("Location"), http.StatusTemporaryRedirect, want)
	}
}
func execute(t *testing.T, commandClient *client.Client, id string, arguments map[string]any) core.Response {
	t.Helper()
	response, err := commandClient.Execute(context.Background(), core.Request{CommandID: id, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func shutdownAndWait(t *testing.T, commandClient *client.Client, done <-chan error) {
	t.Helper()
	response := execute(t, commandClient, "kernel.shutdown", nil)
	if !response.Success || resultObject(response)["requested"] != true {
		t.Fatalf("shutdown: %#v", response)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kernel did not stop")
	}
}

func TestKernelRestartCommandSelectsSelfReplacement(t *testing.T) {
	root := t.TempDir()
	done := startKernel(t, root, availablePort(t))
	var output bytes.Buffer
	if code := admin.Main([]string{"--root", root, "kernel.restart"}, strings.NewReader(""), &output, &output); code != 0 || !strings.Contains(output.String(), "requested: true") {
		t.Fatalf("restart code=%d output=%q", code, output.String())
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartRequested) {
			t.Fatalf("restart outcome = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kernel did not finish graceful restart cleanup")
	}
	paths := instance.NewPaths(root)
	for _, path := range []string{paths.Socket, paths.PIDFile} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runtime file remains %s: %v", path, err)
		}
	}
}

func TestFullCommandBusLifecycleAndAdministrativeModes(t *testing.T) {
	root := t.TempDir()
	environmentPort, startupPort, runtimePort, restartStartupPort, runtimeSSHPort := availablePort(t), availablePort(t), availablePort(t), availablePort(t), availablePort(t)
	t.Setenv("THE8020_NETWORK_MAIN_PORT", stringInt(environmentPort))
	done := startKernel(t, root, startupPort)
	paths := instance.NewPaths(root)
	commandClient := client.New(paths.Socket)
	defer func() { commandClient.Close() }()
	status := execute(t, commandClient, "kernel.status", nil)
	statusResult := resultObject(status)
	if !status.Success || statusResult["main_port"] != json.Number(stringInt(startupPort)) || statusResult["build_id"] != "integration-test" || statusResult["database_pool_maximum_open_connections"] != json.Number("32") || statusResult["database_pool_maximum_idle_connections"] != json.Number("8") {
		t.Fatalf("status: %#v", status)
	}
	if _, exists := statusResult["database_result_maximum_rows"]; exists {
		t.Fatalf("system status exposes database result limits: %#v", status.Result)
	}
	if _, exists := statusResult["database_result_maximum_bytes"]; exists {
		t.Fatalf("system status exposes database result limits: %#v", status.Result)
	}
	get := execute(t, commandClient, "kernel.config.get", map[string]any{"key": "network.main_port"})
	setting := resultObject(get)["setting"].(map[string]any)
	if setting["source"] != "startup_argument" || setting["storage"] != "node" || setting["environment_value"] != json.Number(stringInt(environmentPort)) {
		t.Fatalf("precedence: %#v", setting)
	}
	listed := execute(t, commandClient, "kernel.config.list", nil)
	listedSettings := resultObject(listed)["settings"].([]any)
	if !listed.Success || len(listedSettings) != len(definitions())-1 {
		t.Fatalf("settings list: %#v", listed)
	}
	for _, item := range listedSettings {
		fields := item.(map[string]any)
		if len(fields) != 2 || fields["key"] == nil || fields["description"] == nil {
			t.Fatalf("non-compact settings list item: %#v", fields)
		}
	}
	detailedList := execute(t, commandClient, "kernel.config.list", map[string]any{"view": "detail"})
	detailedSetting := resultObject(detailedList)["settings"].([]any)[0].(map[string]any)
	if !detailedList.Success || detailedSetting["storage"] == nil || detailedSetting["configured_value"] == nil || detailedSetting["active_value"] == nil {
		t.Fatalf("detailed settings list: %#v", detailedList)
	}
	invalidList := execute(t, commandClient, "kernel.config.list", map[string]any{"view": "verbose"})
	if invalidList.Success || invalidList.Error.Code != core.CodeInvalidArguments {
		t.Fatalf("invalid settings list view: %#v", invalidList)
	}

	currentDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	var oneShot bytes.Buffer
	if code := admin.Main([]string{"kernel.status"}, strings.NewReader(""), &oneShot, &oneShot); code != 0 || !strings.Contains(oneShot.String(), "main_port") {
		t.Fatalf("one-shot code %d: %s", code, oneShot.String())
	}
	if err := os.Chdir(currentDirectory); err != nil {
		t.Fatal(err)
	}
	var interactive bytes.Buffer
	if code := admin.Main([]string{"--root", root}, strings.NewReader("help\nkernel.status\nexit\n"), &interactive, &interactive); code != 0 || !strings.Contains(interactive.String(), "main_port") || !strings.Contains(interactive.String(), "exit                     Exit the interactive console") {
		t.Fatalf("interactive code %d: %s", code, interactive.String())
	}
	var compactList bytes.Buffer
	if code := admin.Main([]string{"--root", root, "kernel.config.list"}, strings.NewReader(""), &compactList, &compactList); code != 0 {
		t.Fatalf("compact list code %d: %s", code, compactList.String())
	}
	if strings.Contains(compactList.String(), "key:") || strings.Contains(compactList.String(), "description:") || strings.Contains(compactList.String(), "settings:") {
		t.Fatalf("compact list contains field labels: %s", compactList.String())
	}
	var detailedListOutput bytes.Buffer
	if code := admin.Main([]string{"--root", root, "kernel.config.list", "detail"}, strings.NewReader(""), &detailedListOutput, &detailedListOutput); code != 0 {
		t.Fatalf("detailed list code %d: %s", code, detailedListOutput.String())
	}
	detailText := detailedListOutput.String()
	if keyIndex, descriptionIndex, storageIndex, valueIndex := strings.Index(detailText, "key:"), strings.Index(detailText, "description:"), strings.Index(detailText, "storage:"), strings.Index(detailText, "configured_value:"); keyIndex < 0 || descriptionIndex <= keyIndex || storageIndex <= descriptionIndex || valueIndex <= storageIndex {
		t.Fatalf("detailed list field order: %s", detailText)
	}

	set := execute(t, commandClient, "kernel.config.set", map[string]any{"key": "network.main_port", "value": stringInt(runtimePort)})
	if !set.Success {
		t.Fatalf("set port: %#v", set)
	}
	response, err := http.Get("http://127.0.0.1:" + stringInt(runtimePort) + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(body) != "OK" {
		t.Fatalf("health = %d %q", response.StatusCode, body)
	}
	assertRootRedirect(t, runtimePort, "/the8020/uui/shell/")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	failed := execute(t, commandClient, "kernel.config.set", map[string]any{"key": "network.main_port", "value": stringInt(occupiedPort)})
	_ = occupied.Close()
	if failed.Success || failed.Error.Code != core.CodePortUnavailable {
		t.Fatalf("occupied port response: %#v", failed)
	}
	afterFailure := execute(t, commandClient, "kernel.config.get", map[string]any{"key": "network.main_port"})
	if resultObject(afterFailure)["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(runtimePort)) {
		t.Fatalf("failed change altered setting: %#v", afterFailure)
	}
	sshChange := execute(t, commandClient, "kernel.config.set", map[string]any{"key": "network.ssh_port", "value": stringInt(runtimeSSHPort)})
	if !sshChange.Success {
		t.Fatalf("set SSH port: %#v", sshChange)
	}
	sshSetting := resultObject(sshChange)["setting"].(map[string]any)
	if sshSetting["configured_value"] != json.Number(stringInt(runtimeSSHPort)) || sshSetting["active_value"] != json.Number(stringInt(runtimeSSHPort)) || sshSetting["restart_pending"] != false {
		t.Fatalf("set SSH port: %#v", sshChange)
	}
	sshConnection, err := net.DialTimeout("tcp", "127.0.0.1:"+stringInt(runtimeSSHPort), time.Second)
	if err != nil {
		t.Fatalf("connect to replacement SSH port: %v", err)
	}
	_ = sshConnection.Close()
	occupiedSSH, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	occupiedSSHPort := occupiedSSH.Addr().(*net.TCPAddr).Port
	failedSSH := execute(t, commandClient, "kernel.config.set", map[string]any{"key": "network.ssh_port", "value": stringInt(occupiedSSHPort)})
	_ = occupiedSSH.Close()
	if failedSSH.Success || failedSSH.Error.Code != core.CodePortUnavailable {
		t.Fatalf("occupied SSH port response: %#v", failedSSH)
	}
	afterSSHFailure := execute(t, commandClient, "kernel.config.get", map[string]any{"key": "network.ssh_port"})
	if resultObject(afterSSHFailure)["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(runtimeSSHPort)) {
		t.Fatalf("failed SSH change altered setting: %#v", afterSSHFailure)
	}
	loggingChange := execute(t, commandClient, "kernel.config.set", map[string]any{"key": "logging.enabled", "value": "false"})
	if !loggingChange.Success {
		t.Fatalf("disable logging: %#v", loggingChange)
	}
	for key, value := range map[string]string{"logging.split_period": "hour", "logging.max_file_size": "2GB", "logging.max_total_size": "3GB", "logging.enabled": "true"} {
		change := execute(t, commandClient, "kernel.config.set", map[string]any{"key": key, "value": value})
		if !change.Success {
			t.Fatalf("change %s: %#v", key, change)
		}
	}
	for key, value := range map[string]string{"database.maximum_open_connections": "64", "database.maximum_idle_connections": "16"} {
		change := execute(t, commandClient, "kernel.config.set", map[string]any{"key": key, "value": value})
		if !change.Success || resultObject(change)["setting"].(map[string]any)["restart_pending"] != false {
			t.Fatalf("change %s: %#v", key, change)
		}
	}
	poolStatus := execute(t, commandClient, "kernel.status", nil)
	poolStatusResult := resultObject(poolStatus)
	if poolStatusResult["database_pool_maximum_open_connections"] != json.Number("64") || poolStatusResult["database_pool_maximum_idle_connections"] != json.Number("16") {
		t.Fatalf("resized database pool status: %#v", poolStatus)
	}
	invalidPool := execute(t, commandClient, "kernel.config.set", map[string]any{"key": "database.maximum_idle_connections", "value": "65"})
	if invalidPool.Success || invalidPool.Error.Code != core.CodeInvalidSettingValue {
		t.Fatalf("invalid database pool response: %#v", invalidPool)
	}
	globalThroughKernel := execute(t, commandClient, "kernel.config.set", map[string]any{"key": "network.root_alias", "value": "example/auth/login/"})
	if globalThroughKernel.Success || globalThroughKernel.Error == nil || globalThroughKernel.Error.Code != core.CodeInvalidArguments {
		t.Fatalf("kernel config accepted a global setting: %#v", globalThroughKernel)
	}
	nodeData, err := os.ReadFile(paths.NodeSettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nodeData), "main_port = ") || !strings.Contains(string(nodeData), "ssh_port = ") || !strings.Contains(string(nodeData), "maximum_open_connections = 64") || !strings.Contains(string(nodeData), "maximum_idle_connections = 16") || strings.Contains(string(nodeData), "root_alias") {
		t.Fatalf("node settings=%q", nodeData)
	}
	shutdownAndWait(t, commandClient, done)
	for _, path := range []string{paths.Socket, paths.PIDFile} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runtime file remains %s: %v", path, err)
		}
	}

	done = startKernel(t, root, restartStartupPort)
	commandClient.Close()
	commandClient = client.New(paths.Socket)
	restarted := execute(t, commandClient, "kernel.config.get", map[string]any{"key": "network.main_port"})
	if resultObject(restarted)["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(runtimePort)) || resultObject(restarted)["setting"].(map[string]any)["source"] != "persisted" {
		t.Fatalf("persisted restart: %#v", restarted)
	}
	restartedSSH := execute(t, commandClient, "kernel.config.get", map[string]any{"key": "network.ssh_port"})
	restartedSSHSetting := resultObject(restartedSSH)["setting"].(map[string]any)
	if restartedSSHSetting["configured_value"] != json.Number(stringInt(runtimeSSHPort)) || restartedSSHSetting["active_value"] != json.Number(stringInt(runtimeSSHPort)) || restartedSSHSetting["source"] != "persisted" || restartedSSHSetting["restart_pending"] != false {
		t.Fatalf("persisted SSH restart: %#v", restartedSSH)
	}
	restartedPool := execute(t, commandClient, "kernel.status", nil)
	restartedPoolResult := resultObject(restartedPool)
	if restartedPoolResult["database_pool_maximum_open_connections"] != json.Number("64") || restartedPoolResult["database_pool_maximum_idle_connections"] != json.Number("16") {
		t.Fatalf("persisted database pool status: %#v", restartedPool)
	}
	sshConnection, err = net.DialTimeout("tcp", "127.0.0.1:"+stringInt(runtimeSSHPort), time.Second)
	if err != nil {
		t.Fatalf("connect to persisted SSH port: %v", err)
	}
	_ = sshConnection.Close()
	assertRootRedirect(t, runtimePort, "/the8020/uui/shell/")
	unset := execute(t, commandClient, "kernel.config.unset", map[string]any{"key": "network.main_port"})
	if !unset.Success || resultObject(unset)["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(restartStartupPort)) {
		t.Fatalf("unset: %#v", unset)
	}
	shutdownAndWait(t, commandClient, done)
	if _, err := os.Stat(filepath.Join(root, "kernel.toml")); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{filepath.Join(root, "config"), filepath.Join(root, "state")} {
		if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy runtime store exists at %s: %v", legacy, err)
		}
	}
}
