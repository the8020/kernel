// Package app composes and coordinates the complete Phase 1 kernel lifecycle.
package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/cbus/server"
	"the8020/kernel/database"
	"the8020/kernel/instance"
	"the8020/kernel/lifecycle"
	"the8020/kernel/logging"
	runtimehost "the8020/kernel/runtime"
	"the8020/kernel/services"
	"the8020/kernel/settings"
)

// RegisterHandlers is generated static handler registration.
type RegisterHandlers func(*core.Registry, *services.Services) error

// Config contains generated catalogs and process-level startup inputs.
type Config struct {
	Root        string
	Startup     map[string]string
	InitOnly    bool
	Definitions []settings.Definition
	Register    RegisterHandlers
	BuildID     string
	initialize  func(context.Context, *settings.Manager, *database.Manager, *services.Services) (*services.RuntimeServices, runtimeCleanupFunc)
}

const gracefulShutdownSteps = 9

// ErrRestartRequested tells the process entrypoint to replace the current
// kernel executable after graceful cleanup.
var ErrRestartRequested = errors.New("kernel restart requested")

type repeatedSettings map[string]string

func (s *repeatedSettings) String() string {
	parts := make([]string, 0, len(*s))
	for key, value := range *s {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ",")
}
func (s *repeatedSettings) Set(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || key == "" {
		return errors.New("--set requires key=value")
	}
	if *s == nil {
		*s = map[string]string{}
	}
	(*s)[key] = value
	return nil
}

// Main parses the generic kernel process arguments and runs the kernel.
func Main(args []string, definitions []settings.Definition, register RegisterHandlers, buildID string) int {
	flags := flag.NewFlagSet("kernel", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("root", "", "explicit instance root")
	initOnly := flags.Bool("init-only", false, "initialize the node and exit")
	initDefaults := flags.Bool("init-defaults", false, "initialize noninteractively with default paths")
	startup := repeatedSettings{}
	flags.Var(&startup, "set", "startup setting override (key=value, repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "error: unexpected arguments")
		return 2
	}
	resolved, err := instance.ResolveRoot(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kernel: %s\n", err)
		return 1
	}
	if _, err := instance.LoadPaths(resolved); errors.Is(err, instance.ErrNotInitialized) {
		if *initDefaults {
			if err := initializeDefaultLayout(resolved); err != nil {
				fmt.Fprintf(os.Stderr, "kernel: %s\n", err)
				return 1
			}
		} else {
			resolved, err = initializeInteractive(resolved, os.Stdin, os.Stdout)
			if err != nil {
				fmt.Fprintf(os.Stderr, "kernel: %s\n", err)
				return 1
			}
		}
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "kernel: inspect node initialization: %s\n", err)
		return 1
	}
	err = Run(context.Background(), Config{Root: resolved, Startup: startup, InitOnly: *initOnly, Definitions: definitions, Register: register, BuildID: buildID})
	if errors.Is(err, ErrRestartRequested) {
		if err := replaceCurrentProcess(args); err != nil {
			fmt.Fprintf(os.Stderr, "kernel: restart: %s\n", err)
			return 1
		}
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kernel: %s\n", err)
		return 1
	}
	return 0
}

func initializeInteractive(root string, input io.Reader, output io.Writer) (string, error) {
	reader := bufio.NewReader(input)
	fmt.Fprintf(output, "No 80|20 node is initialized in %s. Initialize it? [Y/n] ", root)
	answer, err := reader.ReadString('\n')
	if err != nil && len(answer) == 0 {
		return "", fmt.Errorf("read initialization confirmation: %w", err)
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "" && answer != "y" && answer != "yes" {
		return "", errors.New("initialization cancelled")
	}
	readPath := func(label, fallback string) (string, error) {
		fmt.Fprintf(output, "%s [%s]: ", label, fallback)
		value, readErr := reader.ReadString('\n')
		if readErr != nil && len(value) == 0 {
			return "", readErr
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = fallback
		}
		return value, nil
	}
	root, err = readPath("Initialization directory", root)
	if err != nil {
		return "", fmt.Errorf("read initialization directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create initialization directory: %w", err)
	}
	root, err = instance.ResolveRoot(root)
	if err != nil {
		return "", err
	}
	if err := initializeDefaultLayout(root); err != nil {
		return "", err
	}
	fmt.Fprintf(output, "Initialized 80|20 node in %s.\n", root)
	return root, nil
}

func initializeDefaultLayout(root string) error {
	paths, err := instance.Prepare(root)
	if err != nil {
		return err
	}
	for _, target := range []struct{ name, path string }{
		{name: "node", path: paths.Node},
		{name: "packages", path: paths.Packages},
		{name: "user data", path: paths.Users},
	} {
		if err := instance.CheckUnixPermissions(target.path); err != nil {
			return fmt.Errorf("%s directory does not support Unix permissions required by sandboxes: %w", target.name, err)
		}
	}
	_, err = instance.Initialize(paths)
	return err
}

func replaceCurrentProcess(args []string) error {
	executable := os.Args[0]
	var err error
	if strings.ContainsRune(executable, os.PathSeparator) {
		executable, err = filepath.Abs(executable)
	} else {
		executable, err = exec.LookPath(executable)
	}
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	return syscall.Exec(executable, append([]string{executable}, args...), os.Environ())
}

// Run initializes, starts, and gracefully stops one kernel instance.
func Run(parent context.Context, config Config) error {
	root, err := instance.ResolveRoot(config.Root)
	if err != nil {
		return err
	}
	paths, err := instance.LoadPaths(root)
	if err != nil {
		return fmt.Errorf("load node layout: %w", err)
	}
	uuid, err := instance.Initialize(paths)
	if err != nil {
		return err
	}
	if config.InitOnly {
		return nil
	}
	lock, err := instance.Acquire(paths)
	if err != nil {
		return err
	}
	defer lock.Release()
	startedAt := time.Now().UTC()
	settingManager, err := settings.New(config.Definitions, settings.PersistencePaths{Node: paths.NodeSettingsFile}, config.Startup, nil)
	if err != nil {
		return err
	}
	policy, err := logging.PolicyFromValues(settingManager.Snapshot())
	if err != nil {
		return err
	}
	loggingManager, err := logging.New(paths.Logs, policy)
	if err != nil {
		return err
	}
	defer loggingManager.Close()
	logger := loggingManager.Logger()
	databaseManager := database.New(database.Config{
		Backend:                activeString(settingManager, "database.backend", database.BackendSQLite),
		Location:               activeString(settingManager, "database.location", database.InstanceRootPlaceholder+"/database/system.db"),
		Username:               activeString(settingManager, "database.username", ""),
		Password:               activeString(settingManager, "database.password", ""),
		InstanceRoot:           root,
		MaximumOpenConnections: activeInt(settingManager, "database.maximum_open_connections", 0),
		MaximumIdleConnections: activeInt(settingManager, "database.maximum_idle_connections", 0),
		MaximumResultRows:      activeInt(settingManager, "database.maximum_result_rows", database.DefaultMaximumResultRows),
		MaximumResultBytes:     activeInt(settingManager, "database.maximum_result_bytes", database.DefaultMaximumResultBytes),
	})
	defer databaseManager.Close()
	repositoryMu := &sync.RWMutex{}
	registry := core.NewRegistry(logger)
	lifecycleManager := lifecycle.New()
	lifecycleManager.ConfigureShutdown(gracefulShutdownSteps)
	serviceSet := services.New(settingManager, nil, loggingManager, lifecycleManager, nil, nil, nil, uuid, paths, startedAt, config.BuildID, &services.RuntimeServices{Failure: "runtime initialization is in progress"})
	serviceSet.Database = databaseManager
	if config.Register == nil {
		return errors.New("missing generated command registry")
	}
	if err := config.Register(registry, serviceSet); err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	commandServer := server.New(paths.Socket, registry)
	if err := commandServer.Start(); err != nil {
		return err
	}
	logger.Info("kernel command bus started", "instance_uuid", uuid, "pid", os.Getpid())
	runtimeContext, cancelRuntime := context.WithCancel(parent)
	initialize := config.initialize
	if initialize == nil {
		initialize = func(ctx context.Context, _ *settings.Manager, _ *database.Manager, _ *services.Services) (*services.RuntimeServices, runtimeCleanupFunc) {
			return initializeRuntime(ctx, root, uuid, paths, settingManager, databaseManager, registry, serviceSet, repositoryMu, logger.With("node_id", uuid))
		}
	}
	type runtimeResult struct {
		cleanup runtimeCleanupFunc
	}
	runtimeDone := make(chan runtimeResult, 1)
	go func() {
		if _, err := databaseManager.Check(runtimeContext); err != nil {
			status := databaseManager.Status()
			logger.Warn("system database unavailable", "backend", status.Backend, "location", status.Location, "error", err)
		}
		runtimeServices, cleanup := initialize(runtimeContext, settingManager, databaseManager, serviceSet)
		if cleanup == nil {
			cleanup = func(context.Context, shutdownProgressFunc) error { return nil }
		}
		serviceSet.PublishRuntime(runtimeServices)
		if runtimeServices.Failure == "" {
			logger.Info("kernel runtime ready", "mode", runtimeServices.Isolation.SelectedMode)
		} else if runtimeContext.Err() == nil {
			logger.Error("kernel runtime unavailable", "error", runtimeServices.Failure)
		}
		runtimeDone <- runtimeResult{cleanup: cleanup}
	}()
	signalContext, stopSignals := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	select {
	case <-signalContext.Done():
		lifecycleManager.Request()
	case <-lifecycleManager.Done():
	}
	if lifecycleManager.RestartRequested() {
		logger.Info("kernel restarting")
	} else {
		logger.Info("kernel shutting down")
	}
	commandServer.BeginShutdown("kernel.status", "kernel.shutdown", "kernel.restart")
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var shutdownError error
	cancelRuntime()
	progress := func(started bool, stepID, step, message string) {
		if started {
			lifecycleManager.StartStep(stepID, step, message)
		} else {
			lifecycleManager.CompleteStep(stepID, step, message)
		}
	}
	progress(true, "runtime_initialization", "runtime initialization", "canceling and joining runtime initialization")
	select {
	case initialized := <-runtimeDone:
		if err := initialized.cleanup(shutdownContext, progress); err != nil {
			shutdownError = errors.Join(shutdownError, err)
		}
		progress(false, "runtime_initialization", "runtime initialization", "runtime cleanup joined")
	case <-shutdownContext.Done():
		progress(false, "runtime_initialization", "runtime initialization", "runtime initialization exceeded the shutdown deadline")
		shutdownError = errors.Join(shutdownError, errors.New("runtime initialization did not stop before shutdown deadline"))
	}
	progress(true, "admin_socket", "administrative socket", "closing the administrative command socket")
	if err := commandServer.Shutdown(shutdownContext); err != nil {
		shutdownError = errors.Join(shutdownError, err)
	}
	progress(false, "admin_socket", "administrative socket", "administrative command socket closed")
	progress(true, "process_resources", "process resources", "closing logging and releasing the instance lock")
	if err := loggingManager.Close(); err != nil {
		shutdownError = errors.Join(shutdownError, err)
	}
	if err := lock.Release(); err != nil {
		shutdownError = errors.Join(shutdownError, err)
	}
	completionMessage := "graceful shutdown complete"
	if lifecycleManager.RestartRequested() {
		completionMessage = "graceful restart cleanup complete"
	}
	progress(false, "process_resources", "complete", completionMessage)
	if lifecycleManager.RestartRequested() {
		return errors.Join(ErrRestartRequested, shutdownError)
	}
	return shutdownError
}

type selectedDevelopmentRunsc struct {
	path     string
	rootless bool
}

func developmentRunscConfig(ctx context.Context, root string, paths instance.Paths, settingManager *settings.Manager) selectedDevelopmentRunsc {
	configured := activeString(settingManager, "sandbox.runtime.mode", string(runtimehost.ModeAuto))
	rootless := configured != string(runtimehost.ModeFull)
	if configured == string(runtimehost.ModeAuto) {
		if versions, err := runtimehost.LoadVersionsFile(paths.RuntimeVersionsFile); err == nil {
			socket := activeString(settingManager, "sandbox.containerd.socket", "/run/containerd/containerd.sock")
			rootless = !runtimehost.NewDoctor(runtimehost.DoctorConfig{
				Root: root, Versions: versions, ContainerdSocket: socket,
				ImageRecordPath: filepath.Join(paths.RuntimeFullImage, "image.json"),
				SmokeRecordPath: filepath.Join(paths.RuntimeFullImage, "smoke.json"),
			}).Inspect(ctx).Ready
		}
	}
	if rootless {
		return selectedDevelopmentRunsc{path: paths.Runsc, rootless: true}
	}
	path, err := exec.LookPath("runsc")
	if err != nil {
		path = "/usr/local/bin/runsc"
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return selectedDevelopmentRunsc{path: path, rootless: false}
}
