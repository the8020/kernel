package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/admin"
	"the8020/kernel/app"
	"the8020/kernel/cbus/client"
	getcommand "the8020/kernel/cbus/commands/settings/get"
	listcommand "the8020/kernel/cbus/commands/settings/list"
	setcommand "the8020/kernel/cbus/commands/settings/set"
	unsetcommand "the8020/kernel/cbus/commands/settings/unset"
	restartcommand "the8020/kernel/cbus/commands/system/restart"
	shutdowncommand "the8020/kernel/cbus/commands/system/shutdown"
	statuscommand "the8020/kernel/cbus/commands/system/status"
	"the8020/kernel/cbus/core"
	"the8020/kernel/instance"
	"the8020/kernel/services"
	"the8020/kernel/settings"
)

func pointer(value int64) *int64 { return &value }
func definitions() []settings.Definition {
	return []settings.Definition{
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
		{Version: 1, ID: "settings.get", Path: []string{"settings", "get"}, Parameters: []core.Parameter{{Name: "key", Type: "string", Required: true}}},
		{Version: 1, ID: "settings.list", Path: []string{"settings", "list"}, Parameters: []core.Parameter{{Name: "view", Type: "string", Required: false}}},
		{Version: 1, ID: "settings.set", Path: []string{"settings", "set"}, Parameters: []core.Parameter{{Name: "key", Type: "string", Required: true}, {Name: "value", Type: "string", Position: 1, Required: true}}},
		{Version: 1, ID: "settings.unset", Path: []string{"settings", "unset"}, Parameters: []core.Parameter{{Name: "key", Type: "string", Required: true}}},
		{Version: 1, ID: "system.restart", Path: []string{"system", "restart"}, Aliases: [][]string{{"restart"}}, RestartBehavior: "restarts_kernel"},
		{Version: 1, ID: "system.shutdown", Path: []string{"system", "shutdown"}, Aliases: [][]string{{"shutdown"}}, RestartBehavior: "stops_kernel"},
		{Version: 1, ID: "system.status", Path: []string{"system", "status"}, Aliases: [][]string{{"status"}}},
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
	if _, err := instance.WriteLayout(root, instance.Layout{
		Packages: filepath.Join(root, "packages"), Config: filepath.Join(root, "config"),
		State: filepath.Join(root, "state"), Users: filepath.Join(root, "users"),
	}); err != nil {
		t.Fatal(err)
	}
	errorsChannel := make(chan error, 1)
	sshPort := availablePort(t)
	go func() {
		errorsChannel <- app.Run(context.Background(), app.Config{Root: root, Startup: map[string]string{"network.main_port": stringInt(startupPort), "network.ssh_port": stringInt(sshPort)}, Definitions: definitions(), Register: register, BuildID: "integration-test"})
	}()
	paths := instance.NewPaths(root)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		commandClient := client.New(paths.Socket)
		response, err := commandClient.Execute(context.Background(), core.Request{CommandID: "system.status"})
		commandClient.Close()
		if err == nil && response.Success {
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
	response := execute(t, commandClient, "system.shutdown", nil)
	if !response.Success || response.Result["requested"] != true {
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
	if code := admin.Main(catalog(), []string{"--root", root, "restart"}, strings.NewReader(""), &output, &output); code != 0 || !strings.Contains(output.String(), "requested: true") {
		t.Fatalf("restart code=%d output=%q", code, output.String())
	}
	select {
	case err := <-done:
		if !errors.Is(err, app.ErrRestartRequested) {
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
	status := execute(t, commandClient, "system.status", nil)
	if !status.Success || status.Result["main_port"] != json.Number(stringInt(startupPort)) || status.Result["build_id"] != "integration-test" || status.Result["database_pool_maximum_open_connections"] != json.Number("32") || status.Result["database_pool_maximum_idle_connections"] != json.Number("8") {
		t.Fatalf("status: %#v", status)
	}
	get := execute(t, commandClient, "settings.get", map[string]any{"key": "network.main_port"})
	setting := get.Result["setting"].(map[string]any)
	if setting["source"] != "startup_argument" || setting["storage"] != "node" || setting["environment_value"] != json.Number(stringInt(environmentPort)) {
		t.Fatalf("precedence: %#v", setting)
	}
	listed := execute(t, commandClient, "settings.list", nil)
	if !listed.Success || len(listed.Result["settings"].([]any)) != len(definitions()) {
		t.Fatalf("settings list: %#v", listed)
	}
	for _, item := range listed.Result["settings"].([]any) {
		fields := item.(map[string]any)
		if len(fields) != 2 || fields["key"] == nil || fields["description"] == nil {
			t.Fatalf("non-compact settings list item: %#v", fields)
		}
	}
	detailedList := execute(t, commandClient, "settings.list", map[string]any{"view": "detail"})
	detailedSetting := detailedList.Result["settings"].([]any)[0].(map[string]any)
	if !detailedList.Success || detailedSetting["storage"] == nil || detailedSetting["configured_value"] == nil || detailedSetting["active_value"] == nil {
		t.Fatalf("detailed settings list: %#v", detailedList)
	}
	invalidList := execute(t, commandClient, "settings.list", map[string]any{"view": "verbose"})
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
	if code := admin.Main(catalog(), []string{"status"}, strings.NewReader(""), &oneShot, &oneShot); code != 0 || !strings.Contains(oneShot.String(), "main_port") {
		t.Fatalf("one-shot code %d: %s", code, oneShot.String())
	}
	if err := os.Chdir(currentDirectory); err != nil {
		t.Fatal(err)
	}
	var interactive bytes.Buffer
	if code := admin.Main(catalog(), []string{"--root", root}, strings.NewReader("help\nsystem status\nexit\n"), &interactive, &interactive); code != 0 || !strings.Contains(interactive.String(), "main_port") || !strings.Contains(interactive.String(), "exit                     Exit the interactive console") {
		t.Fatalf("interactive code %d: %s", code, interactive.String())
	}
	var compactList bytes.Buffer
	if code := admin.Main(catalog(), []string{"--root", root, "settings", "list"}, strings.NewReader(""), &compactList, &compactList); code != 0 {
		t.Fatalf("compact list code %d: %s", code, compactList.String())
	}
	if strings.Contains(compactList.String(), "key:") || strings.Contains(compactList.String(), "description:") || strings.Contains(compactList.String(), "settings:") {
		t.Fatalf("compact list contains field labels: %s", compactList.String())
	}
	var detailedListOutput bytes.Buffer
	if code := admin.Main(catalog(), []string{"--root", root, "settings", "list", "detail"}, strings.NewReader(""), &detailedListOutput, &detailedListOutput); code != 0 {
		t.Fatalf("detailed list code %d: %s", code, detailedListOutput.String())
	}
	detailText := detailedListOutput.String()
	if keyIndex, descriptionIndex, storageIndex, valueIndex := strings.Index(detailText, "key:"), strings.Index(detailText, "description:"), strings.Index(detailText, "storage:"), strings.Index(detailText, "configured_value:"); keyIndex < 0 || descriptionIndex <= keyIndex || storageIndex <= descriptionIndex || valueIndex <= storageIndex {
		t.Fatalf("detailed list field order: %s", detailText)
	}

	set := execute(t, commandClient, "settings.set", map[string]any{"key": "network.main_port", "value": stringInt(runtimePort)})
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
	failed := execute(t, commandClient, "settings.set", map[string]any{"key": "network.main_port", "value": stringInt(occupiedPort)})
	_ = occupied.Close()
	if failed.Success || failed.Error.Code != core.CodePortUnavailable {
		t.Fatalf("occupied port response: %#v", failed)
	}
	afterFailure := execute(t, commandClient, "settings.get", map[string]any{"key": "network.main_port"})
	if afterFailure.Result["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(runtimePort)) {
		t.Fatalf("failed change altered setting: %#v", afterFailure)
	}
	sshChange := execute(t, commandClient, "settings.set", map[string]any{"key": "network.ssh_port", "value": stringInt(runtimeSSHPort)})
	if !sshChange.Success {
		t.Fatalf("set SSH port: %#v", sshChange)
	}
	sshSetting := sshChange.Result["setting"].(map[string]any)
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
	failedSSH := execute(t, commandClient, "settings.set", map[string]any{"key": "network.ssh_port", "value": stringInt(occupiedSSHPort)})
	_ = occupiedSSH.Close()
	if failedSSH.Success || failedSSH.Error.Code != core.CodePortUnavailable {
		t.Fatalf("occupied SSH port response: %#v", failedSSH)
	}
	afterSSHFailure := execute(t, commandClient, "settings.get", map[string]any{"key": "network.ssh_port"})
	if afterSSHFailure.Result["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(runtimeSSHPort)) {
		t.Fatalf("failed SSH change altered setting: %#v", afterSSHFailure)
	}
	loggingChange := execute(t, commandClient, "settings.set", map[string]any{"key": "logging.enabled", "value": "false"})
	if !loggingChange.Success {
		t.Fatalf("disable logging: %#v", loggingChange)
	}
	for key, value := range map[string]string{"logging.split_period": "hour", "logging.max_file_size": "2GB", "logging.max_total_size": "3GB", "logging.enabled": "true"} {
		change := execute(t, commandClient, "settings.set", map[string]any{"key": key, "value": value})
		if !change.Success {
			t.Fatalf("change %s: %#v", key, change)
		}
	}
	for key, value := range map[string]string{"database.maximum_open_connections": "64", "database.maximum_idle_connections": "16"} {
		change := execute(t, commandClient, "settings.set", map[string]any{"key": key, "value": value})
		if !change.Success || change.Result["setting"].(map[string]any)["restart_pending"] != false {
			t.Fatalf("change %s: %#v", key, change)
		}
	}
	poolStatus := execute(t, commandClient, "system.status", nil)
	if poolStatus.Result["database_pool_maximum_open_connections"] != json.Number("64") || poolStatus.Result["database_pool_maximum_idle_connections"] != json.Number("16") {
		t.Fatalf("resized database pool status: %#v", poolStatus)
	}
	invalidPool := execute(t, commandClient, "settings.set", map[string]any{"key": "database.maximum_idle_connections", "value": "65"})
	if invalidPool.Success || invalidPool.Error.Code != core.CodeInvalidSettingValue {
		t.Fatalf("invalid database pool response: %#v", invalidPool)
	}
	rootAliasChange := execute(t, commandClient, "settings.set", map[string]any{"key": "network.root_alias", "value": "example/auth/login/"})
	if !rootAliasChange.Success {
		t.Fatalf("set global root alias: %#v", rootAliasChange)
	}
	rootAliasSetting := rootAliasChange.Result["setting"].(map[string]any)
	if rootAliasSetting["storage"] != "global" || rootAliasSetting["configured_value"] != "example/auth/login/" || rootAliasSetting["active_value"] != "the8020/uui/shell/" || rootAliasSetting["restart_pending"] != true {
		t.Fatalf("set global root alias: %#v", rootAliasChange)
	}
	invalidRootAlias := execute(t, commandClient, "settings.set", map[string]any{"key": "network.root_alias", "value": "/absolute/path"})
	if invalidRootAlias.Success || invalidRootAlias.Error.Code != core.CodeInvalidSettingValue {
		t.Fatalf("invalid root alias response: %#v", invalidRootAlias)
	}
	nodeData, err := os.ReadFile(paths.NodeSettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	globalData, err := os.ReadFile(paths.GlobalSettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nodeData), "main_port = ") || !strings.Contains(string(nodeData), "ssh_port = ") || !strings.Contains(string(nodeData), "maximum_open_connections = 64") || !strings.Contains(string(nodeData), "maximum_idle_connections = 16") || !strings.Contains(string(globalData), `root_alias = "example/auth/login/"`) || strings.Contains(string(globalData), "main_port") || strings.Contains(string(globalData), "ssh_port") || strings.Contains(string(globalData), "maximum_open_connections") {
		t.Fatalf("node settings=%q global settings=%q", nodeData, globalData)
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
	restarted := execute(t, commandClient, "settings.get", map[string]any{"key": "network.main_port"})
	if restarted.Result["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(runtimePort)) || restarted.Result["setting"].(map[string]any)["source"] != "persisted" {
		t.Fatalf("persisted restart: %#v", restarted)
	}
	restartedSSH := execute(t, commandClient, "settings.get", map[string]any{"key": "network.ssh_port"})
	restartedSSHSetting := restartedSSH.Result["setting"].(map[string]any)
	if restartedSSHSetting["configured_value"] != json.Number(stringInt(runtimeSSHPort)) || restartedSSHSetting["active_value"] != json.Number(stringInt(runtimeSSHPort)) || restartedSSHSetting["source"] != "persisted" || restartedSSHSetting["restart_pending"] != false {
		t.Fatalf("persisted SSH restart: %#v", restartedSSH)
	}
	restartedPool := execute(t, commandClient, "system.status", nil)
	if restartedPool.Result["database_pool_maximum_open_connections"] != json.Number("64") || restartedPool.Result["database_pool_maximum_idle_connections"] != json.Number("16") {
		t.Fatalf("persisted database pool status: %#v", restartedPool)
	}
	sshConnection, err = net.DialTimeout("tcp", "127.0.0.1:"+stringInt(runtimeSSHPort), time.Second)
	if err != nil {
		t.Fatalf("connect to persisted SSH port: %v", err)
	}
	_ = sshConnection.Close()
	restartedRootAlias := execute(t, commandClient, "settings.get", map[string]any{"key": "network.root_alias"})
	restartedRootAliasSetting := restartedRootAlias.Result["setting"].(map[string]any)
	if restartedRootAliasSetting["configured_value"] != "example/auth/login/" || restartedRootAliasSetting["active_value"] != "example/auth/login/" || restartedRootAliasSetting["source"] != "persisted" || restartedRootAliasSetting["storage"] != "global" || restartedRootAliasSetting["restart_pending"] != false {
		t.Fatalf("persisted root alias restart: %#v", restartedRootAlias)
	}
	assertRootRedirect(t, runtimePort, "/example/auth/login/")
	unset := execute(t, commandClient, "settings.unset", map[string]any{"key": "network.main_port"})
	if !unset.Success || unset.Result["setting"].(map[string]any)["configured_value"] != json.Number(stringInt(restartStartupPort)) {
		t.Fatalf("unset: %#v", unset)
	}
	unsetRootAlias := execute(t, commandClient, "settings.unset", map[string]any{"key": "network.root_alias"})
	if !unsetRootAlias.Success || unsetRootAlias.Result["setting"].(map[string]any)["configured_value"] != "the8020/uui/shell/" || unsetRootAlias.Result["setting"].(map[string]any)["restart_pending"] != true {
		t.Fatalf("unset root alias: %#v", unsetRootAlias)
	}
	globalData, err = os.ReadFile(paths.GlobalSettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(globalData), "root_alias") {
		t.Fatalf("global setting remains after unset: %s", globalData)
	}
	shutdownAndWait(t, commandClient, done)
	if _, err := os.Stat(filepath.Join(root, "node", "kernel", "instance.toml")); err != nil {
		t.Fatal(err)
	}
}
