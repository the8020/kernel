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

	"the8020/kernel/auth"
	"the8020/kernel/cbus/core"
	"the8020/kernel/cbus/server"
	platformconsole "the8020/kernel/console"
	"the8020/kernel/database"
	"the8020/kernel/development"
	"the8020/kernel/instance"
	"the8020/kernel/lifecycle"
	"the8020/kernel/logging"
	"the8020/kernel/network"
	"the8020/kernel/nodes"
	workspacepackages "the8020/kernel/packages"
	runtimehost "the8020/kernel/runtime"
	secretstore "the8020/kernel/secrets"
	"the8020/kernel/services"
	"the8020/kernel/settings"
	kernelssh "the8020/kernel/ssh"
)

// RegisterHandlers is generated static handler registration.
type RegisterHandlers func(*core.Registry, *services.Services) error

// Config contains generated catalogs and process-level startup inputs.
type Config struct {
	Root                string
	Startup             map[string]string
	InitOnly            bool
	SynchronizePackages bool
	Definitions         []settings.Definition
	Register            RegisterHandlers
	BuildID             string
	initialize          func(context.Context) (*services.RuntimeServices, runtimeCleanupFunc)
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
	synchronizePackages := flags.Bool("synchronize-packages", false, "synchronize the package index and exit")
	startup := repeatedSettings{}
	flags.Var(&startup, "set", "startup setting override (key=value, repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "error: unexpected arguments")
		return 2
	}
	if *synchronizePackages && !*initOnly {
		fmt.Fprintln(os.Stderr, "error: --synchronize-packages requires --init-only")
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
	err = Run(context.Background(), Config{Root: resolved, Startup: startup, InitOnly: *initOnly, SynchronizePackages: *synchronizePackages, Definitions: definitions, Register: register, BuildID: buildID})
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
	packages, err := readPath("Packages directory", filepath.Join(root, "packages"))
	if err != nil {
		return "", fmt.Errorf("read packages directory: %w", err)
	}
	config, err := readPath("System configuration directory", filepath.Join(root, "config"))
	if err != nil {
		return "", fmt.Errorf("read configuration directory: %w", err)
	}
	users, err := readPath("User data directory", filepath.Join(root, "users"))
	if err != nil {
		return "", fmt.Errorf("read user data directory: %w", err)
	}
	state, err := readPath("System state directory", filepath.Join(root, "state"))
	if err != nil {
		return "", fmt.Errorf("read state directory: %w", err)
	}
	if err := initializeLayout(root, instance.Layout{Packages: packages, Config: config, State: state, Users: users}); err != nil {
		return "", err
	}
	fmt.Fprintf(output, "Initialized 80|20 node in %s.\n", root)
	return root, nil
}

func initializeDefaultLayout(root string) error {
	return initializeLayout(root, instance.Layout{
		Packages: filepath.Join(root, "packages"), Config: filepath.Join(root, "config"),
		State: filepath.Join(root, "state"), Users: filepath.Join(root, "users"),
	})
}

func initializeLayout(root string, layout instance.Layout) error {
	if err := os.MkdirAll(filepath.Join(root, "node"), 0o700); err != nil {
		return fmt.Errorf("create node directory: %w", err)
	}
	paths, err := instance.PrepareLayout(root, layout)
	if err != nil {
		return err
	}
	for _, target := range []struct{ name, path string }{
		{name: "node", path: paths.Node},
		{name: "packages", path: paths.Packages},
		{name: "configuration", path: paths.Config},
		{name: "user data", path: paths.Users},
		{name: "state", path: paths.SharedState},
	} {
		if err := instance.CheckUnixPermissions(target.path); err != nil {
			return fmt.Errorf("%s directory does not support Unix permissions required by sandboxes: %w", target.name, err)
		}
	}
	_, err = instance.WriteLayout(root, layout)
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
	if config.SynchronizePackages {
		return synchronizePackagesBeforeStartup(parent, config, paths, uuid)
	}
	if config.InitOnly {
		return nil
	}
	lock, err := instance.Acquire(paths)
	if err != nil {
		return err
	}
	defer lock.Release()
	nodeManager, err := nodes.New(filepath.Join(paths.Config, "nodes.toml"), uuid)
	if err != nil {
		return fmt.Errorf("initialize node topology: %w", err)
	}
	defer nodeManager.Close()
	startedAt := time.Now().UTC()
	settingManager, err := settings.New(config.Definitions, settings.PersistencePaths{Node: paths.NodeSettingsFile, Global: paths.GlobalSettingsFile}, config.Startup, nil)
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
		Backend:      activeString(settingManager, "database.backend", database.BackendSQLite),
		Location:     activeString(settingManager, "database.location", database.InstanceRootPlaceholder+"/database/system.db"),
		Username:     activeString(settingManager, "database.username", ""),
		Password:     activeString(settingManager, "database.password", ""),
		InstanceRoot: root,
	})
	defer databaseManager.Close()
	databaseContext, cancelDatabaseCheck := context.WithTimeout(parent, 3*time.Second)
	databaseStatus, databaseErr := databaseManager.Check(databaseContext)
	cancelDatabaseCheck()
	if databaseErr != nil {
		logger.Warn("system database unavailable", "backend", databaseStatus.Backend, "location", databaseStatus.Location, "error", databaseErr)
	}
	authManager, err := auth.New(auth.Config{
		UsersFile:       filepath.Join(paths.ConfigAuth, "bootstrap-users.toml"),
		SessionsRoot:    paths.BootstrapSessions,
		SessionDuration: activeDuration(settingManager, "auth.bootstrap.session_duration", 12*time.Hour),
		CleanupInterval: activeDuration(settingManager, "auth.bootstrap.cleanup_interval", 15*time.Minute),
		Cookie: auth.CookieConfig{
			Name:     activeString(settingManager, "auth.bootstrap.cookie.name", "the8020_auth"),
			Secure:   activeBoolDefault(settingManager, "auth.bootstrap.cookie.secure", false),
			SameSite: activeString(settingManager, "auth.bootstrap.cookie.same_site", "lax"),
		},
		Argon2: auth.Argon2Parameters{
			Memory:       uint32(activeInt(settingManager, "auth.bootstrap.argon2.memory", 64*1024)),
			Iterations:   uint32(activeInt(settingManager, "auth.bootstrap.argon2.iterations", 3)),
			Parallelism:  uint8(activeInt(settingManager, "auth.bootstrap.argon2.parallelism", 1)),
			SaltLength:   16,
			OutputLength: 32,
		},
		LockTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("initialize bootstrap authentication: %w", err)
	}
	authContext, cancelAuth := context.WithCancel(parent)
	var authWait sync.WaitGroup
	authWait.Add(1)
	go func() {
		defer authWait.Done()
		authManager.RunCleanup(authContext, func(cleanupErr error) {
			logger.Error("bootstrap authentication-session cleanup failed", "error", cleanupErr)
		})
	}()
	var stopAuthOnce sync.Once
	stopAuth := func() {
		stopAuthOnce.Do(func() {
			cancelAuth()
			authWait.Wait()
		})
	}
	defer stopAuth()
	bootstrapUsers, err := authManager.ListUsers()
	if err != nil {
		return fmt.Errorf("list bootstrap administrators: %w", err)
	}
	if len(bootstrapUsers) == 0 {
		logger.Warn("no bootstrap administrators configured", "next_step", "auth bootstrap-admin add <username>")
	}
	secretManager, err := secretstore.New(secretstore.Config{Path: paths.SecretsFile, LockTimeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("initialize secret storage: %w", err)
	}
	repositoryMu := &sync.RWMutex{}
	packageStore, err := workspacepackages.New(workspacepackages.Config{
		WorkspaceRoot:    root,
		PackagesRoot:     paths.Packages,
		StateRoot:        paths.StateServices,
		IndexRoot:        paths.StatePackageIndex,
		RepositoryMu:     repositoryMu,
		Secrets:          secretManager,
		StateLockTimeout: activeDuration(settingManager, "services.state_lock_timeout", 5*time.Second),
		Defaults:         serviceFrameworkDefaults(settingManager),
		Logger:           logger.With("node_id", uuid),
	})
	if err != nil {
		return fmt.Errorf("initialize filesystem services: %w", err)
	}
	registry := core.NewRegistry(logger)
	developmentRunsc := developmentRunscConfig(parent, root, paths, settingManager)
	developmentManager, err := development.New(development.Config{
		Root: root, PackagesRoot: paths.Packages, ConfigRoot: paths.Config,
		UsersRoot:         paths.Users,
		RuntimeRoot:       paths.RuntimeDevelopment,
		ImageRoot:         filepath.Join(paths.DevelopmentImage, "rootfs"),
		ImageRecord:       filepath.Join(paths.DevelopmentImage, "image.json"),
		MountProfileFile:  filepath.Join(paths.Config, "development-mounts.toml"),
		ActivationGateway: development.NewCommandBusGateway(registry),
		RepositoryMu:      repositoryMu,
		Driver: development.NewRunscDriver(development.RunscConfig{
			RunscPath:   developmentRunsc.path,
			RuntimeRoot: filepath.Join(paths.RuntimeDevelopment, "runsc"),
			SandboxRoot: filepath.Join(paths.RuntimeDevelopment, "sandboxes"),
			LogRoot:     filepath.Join(paths.RuntimeDevelopment, "logs"),
			Rootless:    developmentRunsc.rootless,
		}),
		Logger: logger.With("node_id", uuid),
	})
	if err != nil {
		return fmt.Errorf("initialize development sandboxes: %w", err)
	}
	portValue, ok := settingManager.Active("network.main_port")
	if !ok {
		return errors.New("network.main_port is not registered")
	}
	networkManager, err := network.New(
		int(portValue.(int64)),
		activeString(settingManager, "network.root_alias", "the8020/uui/shell/"),
	)
	if err != nil {
		return err
	}
	defer networkManager.Close()
	if err := settingManager.RegisterApplier([]string{"network.main_port"}, networkManager); err != nil {
		return err
	}
	consoleManager, err := platformconsole.New(platformconsole.Config{
		Authentication: authManager,
		Development:    developmentManager,
	})
	if err != nil {
		return fmt.Errorf("initialize sandbox console broker: %w", err)
	}
	defer consoleManager.Close()
	if err := networkManager.RegisterRoute(platformconsole.Route, consoleManager); err != nil {
		return fmt.Errorf("register sandbox console route: %w", err)
	}
	sshPortValue, ok := settingManager.Active("network.ssh_port")
	if !ok {
		return errors.New("network.ssh_port is not registered")
	}
	sshManager, err := kernelssh.New(kernelssh.Config{
		Port:           int(sshPortValue.(int64)),
		HostKeyPath:    paths.SSHHostKey,
		Authentication: authManager,
		Development:    developmentManager,
		Consoles:       consoleManager,
		Logger:         logger.With("node_id", uuid),
	})
	if err != nil {
		return fmt.Errorf("initialize SSH server: %w", err)
	}
	defer sshManager.Close(context.Background())
	if err := settingManager.RegisterApplier([]string{"network.ssh_port"}, sshManager); err != nil {
		return err
	}
	if err := settingManager.RegisterApplier([]string{"logging.enabled", "logging.split_period", "logging.max_file_size", "logging.max_total_size"}, loggingManager); err != nil {
		return err
	}
	lifecycleManager := lifecycle.New()
	lifecycleManager.ConfigureShutdown(gracefulShutdownSteps)
	serviceSet := services.New(settingManager, networkManager, loggingManager, lifecycleManager, authManager, packageStore, developmentManager, uuid, paths, startedAt, config.BuildID, &services.RuntimeServices{Failure: "runtime initialization is in progress"})
	serviceSet.Secrets = secretManager
	serviceSet.Database = databaseManager
	serviceSet.Layout = instance.NewLayoutManager(root)
	serviceSet.Nodes = nodeManager
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
	logger.Info("kernel started", "instance_uuid", uuid, "pid", os.Getpid(), "main_port", networkManager.Port(), "ssh_port", sshManager.Port())
	runtimeContext, cancelRuntime := context.WithCancel(parent)
	initialize := config.initialize
	if initialize == nil {
		initialize = func(ctx context.Context) (*services.RuntimeServices, runtimeCleanupFunc) {
			return initializeRuntime(ctx, root, uuid, paths, settingManager, packageStore, networkManager, authManager, databaseManager, registry, consoleManager, nodeManager, logger.With("node_id", uuid))
		}
	}
	type runtimeResult struct {
		cleanup runtimeCleanupFunc
	}
	runtimeDone := make(chan runtimeResult, 1)
	go func() {
		runtimeServices, cleanup := initialize(runtimeContext)
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
	commandServer.BeginShutdown("system.status", "system.shutdown", "system.restart")
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var shutdownError error
	if err := sshManager.Close(shutdownContext); err != nil {
		shutdownError = errors.Join(shutdownError, err)
	}
	_ = consoleManager.Close()
	cancelRuntime()
	progress := func(started bool, stepID, step, message string) {
		if started {
			lifecycleManager.StartStep(stepID, step, message)
		} else {
			lifecycleManager.CompleteStep(stepID, step, message)
		}
	}
	var parallelWait sync.WaitGroup
	parallelWait.Add(2)
	go func() {
		defer parallelWait.Done()
		progress(true, "public_http", "public HTTP", "draining the public HTTP listener")
		networkManager.Close()
		progress(false, "public_http", "public HTTP", "public HTTP listener closed")
	}()
	go func() {
		defer parallelWait.Done()
		progress(true, "authentication", "authentication maintenance", "stopping authentication-session cleanup")
		stopAuth()
		progress(false, "authentication", "authentication maintenance", "authentication maintenance stopped")
	}()
	progress(true, "runtime_initialization", "runtime initialization", "canceling and joining runtime initialization")
	select {
	case initialized := <-runtimeDone:
		if err := initialized.cleanup(shutdownContext, progress); err != nil {
			shutdownError = errors.Join(shutdownError, err)
		}
		if err := developmentManager.Close(shutdownContext); err != nil {
			shutdownError = errors.Join(shutdownError, err)
		}
		progress(false, "runtime_initialization", "runtime initialization", "runtime and development sandbox cleanup joined")
	case <-shutdownContext.Done():
		progress(false, "runtime_initialization", "runtime initialization", "runtime initialization exceeded the shutdown deadline")
		shutdownError = errors.Join(shutdownError, errors.New("runtime initialization did not stop before shutdown deadline"))
		shutdownError = errors.Join(shutdownError, developmentManager.Close(context.Background()))
	}
	parallelWait.Wait()
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

func synchronizePackagesBeforeStartup(parent context.Context, config Config, paths instance.Paths, uuid string) error {
	lock, err := instance.Acquire(paths)
	if err != nil {
		return fmt.Errorf("acquire node for package synchronization: %w", err)
	}
	defer lock.Release()
	secretManager, err := secretstore.New(secretstore.Config{Path: paths.SecretsFile, LockTimeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("initialize secret storage: %w", err)
	}
	store, err := workspacepackages.New(workspacepackages.Config{
		WorkspaceRoot: rootForPaths(paths), PackagesRoot: paths.Packages,
		StateRoot: paths.StateServices, IndexRoot: paths.StatePackageIndex,
		Secrets: secretManager,
	})
	if err != nil {
		return fmt.Errorf("initialize package synchronization: %w", err)
	}
	if config.Register == nil {
		return errors.New("missing generated command registry")
	}
	registry := core.NewRegistry(nil)
	serviceSet := &services.Services{
		Packages: store, PackageManagement: store, Secrets: secretManager,
		Instance: services.InstanceInfo{UUID: uuid, PID: os.Getpid(), Paths: paths, BuildID: config.BuildID},
	}
	if err := config.Register(registry, serviceSet); err != nil {
		return fmt.Errorf("register package synchronization command: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	response := registry.Execute(ctx, core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: "package.synchronize", Arguments: map[string]any{}})
	if !response.Success {
		if response.Error == nil {
			return errors.New("package synchronization failed")
		}
		return fmt.Errorf("package synchronization failed: %s", response.Error.Message)
	}
	return nil
}

func rootForPaths(paths instance.Paths) string {
	if paths.Root != "" {
		return paths.Root
	}
	return filepath.Dir(paths.Node)
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
