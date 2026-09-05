package development

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"the8020/kernel/deployment"
	"the8020/kernel/execution"
	"the8020/kernel/sandbox/backend"
)

type Config struct {
	Root              string
	PackagesRoot      string
	UsersRoot         string
	RuntimeRoot       string
	ImageRoot         string
	ImageRecord       string
	MountProfile      []MountDefinition
	ActivationGateway ActivationGateway
	Driver            SandboxDriver
	RepositoryMu      *sync.RWMutex
	Logger            *slog.Logger
}

type Manager struct {
	config        Config
	driver        SandboxDriver
	userMu        sync.Map
	sandboxMu     sync.Map
	imageMu       sync.RWMutex
	repositoryMu  *sync.RWMutex
	deploymentMu  sync.RWMutex
	deployment    deployment.SchemaHook
	owned         sync.Map
	server        *http.Server
	listener      net.Listener
	endpoint      string
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}
}

func (m *Manager) SetSchemaDeployment(hook deployment.SchemaHook) {
	m.deploymentMu.Lock()
	m.deployment = hook
	m.deploymentMu.Unlock()
}

func (m *Manager) schemaDeployment() deployment.SchemaHook {
	m.deploymentMu.RLock()
	defer m.deploymentMu.RUnlock()
	return m.deployment
}

const sandboxSchema = 1

const authorizedKeysLimit = 64 << 10

func New(config Config) (*Manager, error) {
	for name, path := range map[string]string{"root": config.Root, "packages": config.PackagesRoot, "users": config.UsersRoot, "runtime": config.RuntimeRoot, "image": config.ImageRoot} {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("development %s path must be absolute", name)
		}
	}
	root, err := canonicalDirectory(config.Root)
	if err != nil {
		return nil, err
	}
	packages, err := canonicalDirectory(config.PackagesRoot)
	if err != nil {
		return nil, err
	}
	config.Root, config.PackagesRoot = root, packages
	for _, directory := range []string{config.UsersRoot, config.RuntimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, err
		}
	}
	for name, path := range map[string]*string{"users": &config.UsersRoot, "runtime": &config.RuntimeRoot} {
		canonical, canonicalErr := canonicalDirectory(*path)
		if canonicalErr != nil {
			return nil, fmt.Errorf("development %s path: %w", name, canonicalErr)
		}
		*path = canonical
	}
	config.ImageRoot = filepath.Clean(config.ImageRoot)
	if _, statErr := os.Stat(config.ImageRoot); statErr == nil {
		canonical, canonicalErr := canonicalDirectory(config.ImageRoot)
		if canonicalErr != nil {
			return nil, fmt.Errorf("development image path: %w", canonicalErr)
		}
		config.ImageRoot = canonical
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect development image path: %w", statErr)
	}
	profile, err := loadMountProfile(config)
	if err != nil {
		return nil, err
	}
	config.MountProfile = profile
	repositoryMu := config.RepositoryMu
	if repositoryMu == nil {
		repositoryMu = &sync.RWMutex{}
	}
	m := &Manager{config: config, driver: config.Driver, repositoryMu: repositoryMu, cleanupDone: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for development sandbox API: %w", err)
	}
	m.listener, m.endpoint = listener, "http://"+listener.Addr().String()
	m.server = &http.Server{Handler: http.HandlerFunc(m.serveSandbox), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = m.server.Serve(listener) }()
	cleanupContext, cleanupCancel := context.WithCancel(context.Background())
	m.cleanupCancel = cleanupCancel
	go func() {
		defer close(m.cleanupDone)
		m.destroyInheritedSandboxes(cleanupContext)
	}()
	return m, nil
}

func (m *Manager) destroyInheritedSandboxes(parent context.Context) {
	if m.driver == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	ids, err := m.driver.List(ctx)
	if err != nil {
		if m.config.Logger != nil {
			m.config.Logger.Warn("development inherited-sandbox inspection unavailable", "error", err)
		}
		return
	}
	for _, id := range ids {
		unlock := m.lockSandbox(id)
		if _, owned := m.owned.Load(id); owned {
			unlock()
			continue
		}
		if err := m.driver.Delete(ctx, id); err != nil {
			unlock()
			if m.config.Logger != nil {
				m.config.Logger.Error("development inherited sandbox cleanup failed", "sandbox_id", id, "error", err)
			}
			continue
		}
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, id)
		unlock()
	}
}

func (m *Manager) Close(ctx context.Context) error {
	if m.cleanupCancel != nil {
		m.cleanupCancel()
	}
	if m.cleanupDone != nil {
		select {
		case <-m.cleanupDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var joined error
	type activeSandbox struct{ sandboxID, userID string }
	active := []activeSandbox{}
	m.owned.Range(func(key, value any) bool {
		sandboxID, sandboxOK := key.(string)
		userID, userOK := value.(string)
		if sandboxOK && userOK {
			active = append(active, activeSandbox{sandboxID: sandboxID, userID: userID})
		}
		return true
	})
	for _, item := range active {
		unlock := m.lockUser(item.userID)
		sandbox, err := m.loadSandbox(item.userID)
		if err != nil || sandbox.SandboxID != item.sandboxID {
			joined = errors.Join(joined, err)
			unlock()
			continue
		}
		checkpointErr := m.checkpointOverlayLocked(ctx, sandbox)
		stopErr := m.driver.Stop(ctx, item.sandboxID)
		deleteErr := m.driver.Delete(ctx, item.sandboxID)
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, item.sandboxID)
		m.owned.Delete(item.sandboxID)
		joined = errors.Join(joined, checkpointErr, stopErr, deleteErr)
		sandbox.State = StateStopped
		sandbox.ActivationActive, sandbox.WritesPaused = false, false
		sandbox.UpdatedAt = time.Now().UTC()
		joined = errors.Join(joined, m.saveSandbox(sandbox))
		unlock()
	}
	if m.server != nil {
		joined = errors.Join(joined, m.server.Shutdown(ctx))
	}
	return joined
}

func (m *Manager) Create(ctx context.Context, userID string) (Sandbox, error) {
	if err := execution.ValidateUsername(userID); err != nil {
		return Sandbox{}, err
	}
	unlock := m.lockUser(userID)
	defer unlock()
	if existing, err := m.loadSandbox(userID); err == nil {
		return existing, fmt.Errorf("development sandbox already exists for user %s", userID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Sandbox{}, err
	}
	return m.createLocked(ctx, userID)
}

// EnsureSandbox returns the authenticated user's development sandbox, creating
// or starting it only when necessary.
func (m *Manager) EnsureSandbox(ctx context.Context, userID string) (string, error) {
	if err := execution.ValidateUsername(userID); err != nil {
		return "", err
	}
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if errors.Is(err, os.ErrNotExist) {
		sandbox, err = m.createLocked(ctx, userID)
	} else if err == nil {
		err = m.startLocked(ctx, &sandbox)
	}
	if err != nil {
		return "", err
	}
	return sandbox.SandboxID, nil
}

// AuthorizedKeys reads the existing sandbox's root authorized-keys file
// directly from durable storage without creating, starting, or mutating the
// sandbox. Every traversed component must remain beneath the recorded canonical
// system root and must not be a symlink.
func (m *Manager) AuthorizedKeys(userID string) ([]byte, error) {
	if err := execution.ValidateUsername(userID); err != nil {
		return nil, err
	}
	var sandbox Sandbox
	if err := readTOML(filepath.Join(m.sandboxRootForUser(userID), "sandbox.toml"), &sandbox); err != nil {
		return nil, err
	}
	expectedID, _ := sandboxIDForUser(userID)
	if sandbox.Schema != sandboxSchema || sandbox.UserID != userID || sandbox.SandboxID != expectedID || sandbox.DevelopmentImage == "" || sandbox.SystemPath == "" {
		return nil, errors.New("development sandbox storage is unavailable")
	}
	expectedRoot, err := m.systemRootPath(userID, sandbox.DevelopmentImage)
	if err != nil || filepath.Clean(sandbox.SystemPath) != expectedRoot {
		return nil, errors.New("development sandbox system root is invalid")
	}
	canonicalRoot, err := canonicalDirectory(expectedRoot)
	if err != nil || canonicalRoot != expectedRoot {
		return nil, errors.New("development sandbox system root is unsafe")
	}
	root, err := unix.Open(canonicalRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("development sandbox system root is unavailable")
	}
	defer unix.Close(root)
	fileDescriptor, err := unix.Openat2(root, "root/.ssh/authorized_keys", &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, errors.New("development sandbox authorized keys are unavailable")
	}
	file := os.NewFile(uintptr(fileDescriptor), "authorized_keys")
	if file == nil {
		_ = unix.Close(fileDescriptor)
		return nil, errors.New("development sandbox authorized keys are unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > authorizedKeysLimit {
		return nil, errors.New("development sandbox authorized keys are invalid")
	}
	var content bytes.Buffer
	if _, err := io.CopyN(&content, file, authorizedKeysLimit+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("read development sandbox authorized keys")
	}
	if content.Len() > authorizedKeysLimit {
		return nil, errors.New("development sandbox authorized keys are too large")
	}
	return content.Bytes(), nil
}

func (m *Manager) createLocked(ctx context.Context, userID string) (Sandbox, error) {
	if err := os.MkdirAll(filepath.Join(m.sandboxRootForUser(userID), "runtime"), 0o700); err != nil {
		return Sandbox{}, err
	}
	token, err := randomHex(32)
	if err != nil {
		return Sandbox{}, err
	}
	sandboxID, _ := sandboxIDForUser(userID)
	now := time.Now().UTC()
	sandbox := Sandbox{Schema: sandboxSchema, UserID: userID, SandboxID: sandboxID, State: StateCreating, CreatedAt: now, UpdatedAt: now, Token: token}
	if err := m.preparePersistentMounts(userID); err != nil {
		return Sandbox{}, err
	}
	if err := m.saveSandbox(sandbox); err != nil {
		return Sandbox{}, err
	}
	if err := m.startLocked(ctx, &sandbox); err != nil {
		sandbox.State = StateFailed
		sandbox.UpdatedAt = time.Now().UTC()
		_ = m.saveSandbox(sandbox)
		return sandbox, err
	}
	m.log("development sandbox created", sandbox, "result_state", sandbox.State)
	return sandbox, nil
}

func (m *Manager) List() ([]Sandbox, error) {
	return m.listLocked()
}

func (m *Manager) Inspect(userID string) (Sandbox, error) {
	return m.loadSandbox(userID)
}

func (m *Manager) listLocked() ([]Sandbox, error) {
	users, err := os.ReadDir(m.config.UsersRoot)
	if err != nil {
		return nil, err
	}
	result := []Sandbox{}
	for _, user := range users {
		if !user.IsDir() || !safeUserID(user.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(m.sandboxRootForUser(user.Name()), "sandbox.toml")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		item, err := m.loadSandbox(user.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UserID < result[j].UserID })
	return result, nil
}

func (m *Manager) Start(ctx context.Context, userID string) (Sandbox, error) {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return Sandbox{}, err
	}
	if err := m.startLocked(ctx, &sandbox); err != nil {
		return sandbox, err
	}
	return sandbox, nil
}

func (m *Manager) startLocked(ctx context.Context, sandbox *Sandbox) error {
	if m.driver == nil {
		return errors.New("development sandbox driver is unavailable")
	}
	if _, active := m.owned.Load(sandbox.SandboxID); active {
		running, err := m.driver.Running(ctx, sandbox.SandboxID)
		if err == nil && running {
			return nil
		}
		_ = m.driver.Delete(ctx, sandbox.SandboxID)
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
		m.owned.Delete(sandbox.SandboxID)
	}
	var image ImageStatus
	err := func() error {
		m.imageMu.RLock()
		defer m.imageMu.RUnlock()
		var statusErr error
		image, statusErr = m.imageStatusLocked()
		if statusErr != nil {
			return statusErr
		}
		if image.Digest == "" {
			return errors.New("development image is not built")
		}
		return m.prepareSandboxStorage(ctx, sandbox, image.Digest)
	}()
	if err != nil {
		return err
	}
	sandbox.State = StateStarting
	sandbox.UpdatedAt = time.Now().UTC()
	if err := m.saveSandbox(*sandbox); err != nil {
		return err
	}
	mounts, err := m.resolveMounts(*sandbox)
	if err != nil {
		return err
	}
	unlockSandbox := m.lockSandbox(sandbox.SandboxID)
	defer unlockSandbox()
	if err := m.driver.Delete(ctx, sandbox.SandboxID); err != nil {
		return fmt.Errorf("delete inherited development sandbox %s: %w", sandbox.SandboxID, err)
	}
	_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
	if err := m.driver.Start(ctx, SandboxStart{UserID: sandbox.UserID, SandboxID: sandbox.SandboxID, Packages: sandbox.SourcePath, RootFS: sandbox.SystemPath, Endpoint: m.endpoint, Token: sandbox.Token, Mounts: mounts}); err != nil {
		return err
	}
	m.owned.Store(sandbox.SandboxID, sandbox.UserID)
	if err := m.restoreOverlayLocked(ctx, sandbox); err != nil {
		_ = m.driver.Kill(context.Background(), sandbox.SandboxID)
		_ = m.driver.Delete(context.Background(), sandbox.SandboxID)
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
		m.owned.Delete(sandbox.SandboxID)
		return err
	}
	sandbox.State = StateReady
	sandbox.UpdatedAt = time.Now().UTC()
	sandbox.CanSafelyReset = canSafelyReset(sandbox)
	if err := m.saveSandbox(*sandbox); err != nil {
		return err
	}
	m.log("development sandbox started", *sandbox, "result_state", sandbox.State)
	return nil
}

func (m *Manager) prepareSandboxStorage(ctx context.Context, sandbox *Sandbox, imageDigest string) error {
	if err := m.preparePersistentMounts(sandbox.UserID); err != nil {
		return err
	}
	sandbox.SourcePath = m.config.PackagesRoot
	if sandbox.DevelopmentImage == "" && sandbox.SystemPath == "" {
		if sandbox.State != StateCreating {
			return errors.New("development sandbox system root is uninitialized; factory reset is required")
		}
		systemRoot, err := m.systemRootPath(sandbox.UserID, imageDigest)
		if err != nil {
			return err
		}
		if err := copySystemRoot(ctx, m.config.ImageRoot, systemRoot); err != nil {
			return fmt.Errorf("initialize durable development system: %w", err)
		}
		canonicalRoot, err := canonicalDirectory(systemRoot)
		if err != nil {
			return err
		}
		sandbox.DevelopmentImage, sandbox.SystemPath = imageDigest, canonicalRoot
		return nil
	}
	if sandbox.DevelopmentImage == "" || sandbox.SystemPath == "" {
		return errors.New("development sandbox system root record is incomplete; factory reset is required")
	}
	expectedRoot, err := m.systemRootPath(sandbox.UserID, sandbox.DevelopmentImage)
	if err != nil || filepath.Clean(sandbox.SystemPath) != expectedRoot {
		return errors.New("development sandbox system root record is invalid; factory reset is required")
	}
	canonicalRoot, err := canonicalDirectory(expectedRoot)
	if err != nil {
		return fmt.Errorf("development sandbox system root is unavailable; factory reset is required: %w", err)
	}
	if canonicalRoot != expectedRoot {
		return errors.New("development sandbox system root is unsafe; factory reset is required")
	}
	return nil
}

func packageDirectories(root string) []string {
	result := []string{}
	namespaces, _ := os.ReadDir(root)
	for _, namespace := range namespaces {
		if !namespace.IsDir() || !safePackageSegment(namespace.Name()) {
			continue
		}
		repositories, _ := os.ReadDir(filepath.Join(root, namespace.Name()))
		for _, repository := range repositories {
			if repository.IsDir() && safePackageSegment(repository.Name()) {
				result = append(result, namespace.Name()+"/"+repository.Name())
			}
		}
	}
	sort.Strings(result)
	return result
}

func (m *Manager) Stop(ctx context.Context, userID string) (Sandbox, error) {
	return m.stop(ctx, userID, false)
}

func (m *Manager) Kill(ctx context.Context, userID string) (Sandbox, error) {
	return m.stop(ctx, userID, true)
}

func (m *Manager) stop(ctx context.Context, userID string, kill bool) (Sandbox, error) {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return Sandbox{}, err
	}
	if _, active := m.owned.Load(sandbox.SandboxID); !active {
		return sandbox, nil
	}
	if err := m.checkpointOverlayLocked(ctx, sandbox); err != nil {
		return sandbox, fmt.Errorf("checkpoint development overlay before stop: %w", err)
	}
	sandbox.State, sandbox.UpdatedAt = StateStopping, time.Now().UTC()
	_ = m.saveSandbox(sandbox)
	if kill {
		err = m.driver.Kill(ctx, sandbox.SandboxID)
	} else {
		err = m.driver.Stop(ctx, sandbox.SandboxID)
	}
	if err != nil {
		return sandbox, err
	}
	if err := m.driver.Delete(ctx, sandbox.SandboxID); err != nil {
		return sandbox, err
	}
	_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
	m.owned.Delete(sandbox.SandboxID)
	sandbox.State, sandbox.UpdatedAt = StateStopped, time.Now().UTC()
	err = m.saveSandbox(sandbox)
	m.log("development sandbox stopped", sandbox, "result_state", sandbox.State)
	return sandbox, err
}

func (m *Manager) Restart(ctx context.Context, userID string) (Sandbox, error) {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return Sandbox{}, err
	}
	if _, active := m.owned.Load(sandbox.SandboxID); active {
		if err := m.checkpointOverlayLocked(ctx, sandbox); err != nil {
			return sandbox, fmt.Errorf("checkpoint development overlay before restart: %w", err)
		}
		_ = m.driver.Stop(ctx, sandbox.SandboxID)
		if err := m.driver.Delete(ctx, sandbox.SandboxID); err != nil {
			return sandbox, err
		}
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
		m.owned.Delete(sandbox.SandboxID)
	}
	if err := m.startLocked(ctx, &sandbox); err != nil {
		return sandbox, err
	}
	return sandbox, nil
}

func (m *Manager) Delete(ctx context.Context, userID string) error {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return err
	}
	if _, active := m.owned.Load(sandbox.SandboxID); active {
		_ = m.driver.Kill(ctx, sandbox.SandboxID)
		if err := m.driver.Delete(ctx, sandbox.SandboxID); err != nil {
			return err
		}
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
		m.owned.Delete(sandbox.SandboxID)
	}
	return os.RemoveAll(m.sandboxRoot(sandbox))
}

func (m *Manager) Shell(ctx context.Context, userID, command string) (ShellResult, error) {
	lock := m.userLock(userID)
	lock.Lock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		lock.Unlock()
		return ShellResult{}, err
	}
	if _, active := m.owned.Load(sandbox.SandboxID); !active {
		lock.Unlock()
		return ShellResult{}, errors.New("development sandbox is not running")
	}
	sandbox.State, sandbox.UpdatedAt = StateBusy, time.Now().UTC()
	if err := m.saveSandbox(sandbox); err != nil {
		lock.Unlock()
		return ShellResult{}, err
	}
	lock.Unlock()
	output, err := m.driver.Exec(ctx, sandbox.SandboxID, command)
	lock.Lock()
	current, loadErr := m.loadSandbox(userID)
	if loadErr == nil && current.State == StateBusy {
		current.State, current.UpdatedAt = StateReady, time.Now().UTC()
		_ = m.saveSandbox(current)
	}
	lock.Unlock()
	return ShellResult{UserID: userID, SandboxID: sandbox.SandboxID, Command: command, Output: string(output)}, err
}

func (m *Manager) OpenConsole(ctx context.Context, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	owned, ok := m.owned.Load(sandboxID)
	if !ok {
		return nil, errors.New("development sandbox is not ready")
	}
	userID, ok := owned.(string)
	if !ok {
		return nil, errors.New("development sandbox ownership is unavailable")
	}
	sandbox, err := m.loadSandbox(userID)
	if err != nil || sandbox.SandboxID != sandboxID || (sandbox.State != StateReady && sandbox.State != StateConflicted) {
		return nil, errors.New("development sandbox is not ready")
	}
	consoleDriver, ok := m.driver.(interface {
		OpenConsole(context.Context, string, backend.ConsoleOptions) (backend.Console, error)
	})
	if !ok {
		return nil, errors.New("development sandbox driver does not support interactive consoles")
	}
	return consoleDriver.OpenConsole(ctx, sandboxID, options)
}

func (m *Manager) ResetSource(ctx context.Context, userID string, confirmed bool) (Sandbox, error) {
	if !confirmed {
		return Sandbox{}, errors.New("source reset requires explicit confirmation")
	}
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return Sandbox{}, err
	}
	if _, active := m.owned.Load(sandbox.SandboxID); active {
		_ = m.driver.Kill(ctx, sandbox.SandboxID)
		if err := m.driver.Delete(ctx, sandbox.SandboxID); err != nil {
			return sandbox, err
		}
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
		m.owned.Delete(sandbox.SandboxID)
	}
	if err := os.RemoveAll(m.overlayRoot(sandbox)); err != nil {
		return sandbox, err
	}
	sandbox.SourcePath, sandbox.ConflictedPackages, sandbox.State = "", nil, StateResetting
	if err := m.startLocked(ctx, &sandbox); err != nil {
		return sandbox, err
	}
	return sandbox, nil
}

func (m *Manager) FactoryReset(ctx context.Context, userID string, confirmed bool) (Sandbox, error) {
	if !confirmed {
		return Sandbox{}, errors.New("factory reset requires explicit confirmation")
	}
	unlock := m.lockUser(userID)
	defer unlock()
	old, err := m.loadSandbox(userID)
	if err != nil {
		return Sandbox{}, err
	}
	if _, active := m.owned.Load(old.SandboxID); active {
		_ = m.driver.Kill(ctx, old.SandboxID)
		if err := m.driver.Delete(ctx, old.SandboxID); err != nil {
			return Sandbox{}, err
		}
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, old.SandboxID)
		m.owned.Delete(old.SandboxID)
	}
	if err := os.RemoveAll(m.sandboxRoot(old)); err != nil {
		return Sandbox{}, err
	}
	return m.createLocked(ctx, userID)
}

func (m *Manager) sandboxRootForUser(userID string) string {
	return filepath.Join(m.userRoot(userID), "dev-sandbox")
}

func (m *Manager) sandboxRoot(sandbox Sandbox) string {
	return m.sandboxRootForUser(sandbox.UserID)
}

func (m *Manager) userLock(userID string) *sync.Mutex {
	value, _ := m.userMu.LoadOrStore(userID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (m *Manager) lockUser(userID string) func() {
	lock := m.userLock(userID)
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) lockSandbox(id string) func() {
	value, _ := m.sandboxMu.LoadOrStore(id, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) systemRootPath(userID, imageDigest string) (string, error) {
	if !safeUserID(userID) || imageDigest == "" {
		return "", errors.New("development system storage requires a user and image digest")
	}
	digest := sha256.Sum256([]byte(imageDigest))
	return filepath.Join(m.sandboxRootForUser(userID), "system", hex.EncodeToString(digest[:]), "rootfs"), nil
}

func (m *Manager) loadSandbox(userID string) (Sandbox, error) {
	if !safeUserID(userID) {
		return Sandbox{}, errors.New("invalid development sandbox user")
	}
	var sandbox Sandbox
	if err := readTOML(filepath.Join(m.sandboxRootForUser(userID), "sandbox.toml"), &sandbox); err != nil {
		return Sandbox{}, err
	}
	expectedID, _ := sandboxIDForUser(userID)
	if sandbox.UserID != userID || sandbox.SandboxID != expectedID {
		return Sandbox{}, errors.New("development sandbox identity mismatch")
	}
	if sandbox.Schema != sandboxSchema {
		return Sandbox{}, fmt.Errorf("unsupported development sandbox schema %d", sandbox.Schema)
	}
	if _, owned := m.owned.Load(sandbox.SandboxID); !owned && sandbox.State != StateStopped && sandbox.State != StateFailed {
		sandbox.State = StateStopped
		sandbox.ActivationActive, sandbox.WritesPaused = false, false
		sandbox.UpdatedAt = time.Now().UTC()
		if err := m.saveSandbox(sandbox); err != nil {
			return Sandbox{}, err
		}
	}
	sandbox.CanSafelyReset = canSafelyReset(&sandbox)
	return sandbox, nil
}

func (m *Manager) saveSandbox(sandbox Sandbox) error {
	expectedID, err := sandboxIDForUser(sandbox.UserID)
	if err != nil || sandbox.SandboxID != expectedID {
		return errors.New("invalid development sandbox identity")
	}
	return writeTOML(filepath.Join(m.sandboxRoot(sandbox), "sandbox.toml"), sandbox, 0o600)
}

func canSafelyReset(sandbox *Sandbox) bool {
	if sandbox == nil || sandbox.ActivationActive || sandbox.WritesPaused {
		return false
	}
	switch sandbox.State {
	case StateReady, StateConflicted, StateStopped, StateFailed:
		return true
	default:
		return false
	}
}

func safeUserID(value string) bool {
	return execution.ValidateUsername(value) == nil
}

func sandboxIDForUser(userID string) (string, error) {
	if err := execution.ValidateUsername(userID); err != nil {
		return "", err
	}
	return "dev-" + userID, nil
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func beneath(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func safePackageSegment(value string) bool {
	if value == "" || strings.HasPrefix(value, ".") {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && !strings.ContainsRune("._-", character) {
			return false
		}
	}
	return true
}

func (m *Manager) log(message string, sandbox Sandbox, values ...any) {
	if m.config.Logger == nil {
		return
	}
	fields := []any{"developer_user_id", sandbox.UserID, "sandbox_id", sandbox.SandboxID}
	m.config.Logger.Info(message, append(fields, values...)...)
}

func (m *Manager) ImageStatus() (ImageStatus, error) {
	m.imageMu.RLock()
	defer m.imageMu.RUnlock()
	return m.imageStatusLocked()
}

func (m *Manager) imageStatusLocked() (ImageStatus, error) {
	var record struct {
		Digest      string    `json:"digest"`
		ImageDigest string    `json:"image_digest"`
		BuiltAt     time.Time `json:"built_at"`
		DenoVersion string    `json:"deno_version"`
	}
	if err := readJSON(m.config.ImageRecord, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ImageStatus{BuildStatus: "not-built"}, nil
		}
		return ImageStatus{}, err
	}
	digest := record.Digest
	if digest == "" {
		digest = record.ImageDigest
	}
	return ImageStatus{Digest: digest, BuiltAt: record.BuiltAt, DenoVersion: record.DenoVersion, BuildStatus: "ready"}, nil
}

func (m *Manager) serveSandbox(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "development" || parts[2] != "sandboxes" {
		http.NotFound(response, request)
		return
	}
	userID, operation := parts[3], parts[4]
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		http.Error(response, "sandbox not found", http.StatusNotFound)
		return
	}
	if request.Header.Get("Authorization") != "Bearer "+sandbox.Token {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	var options ActivationOptions
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 1<<20))
	if err := decoder.Decode(&options); err != nil {
		http.Error(response, "invalid request", http.StatusBadRequest)
		return
	}
	metadata := make(map[string]string, len(options.Metadata)+1)
	for key, value := range options.Metadata {
		metadata[key] = value
	}
	metadata["client"] = "sandbox-helper"
	options.Metadata = metadata
	if m.config.ActivationGateway == nil {
		http.Error(response, "development activation command bus is unavailable", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	switch operation {
	case "preview":
		result, err := m.config.ActivationGateway.Preview(request.Context(), userID, options)
		if err != nil {
			http.Error(response, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(response).Encode(result)
	case "activate":
		activationContext := context.WithValue(request.Context(), deferredOverlayResetKey{}, true)
		result, err := m.config.ActivationGateway.Activate(activationContext, userID, options)
		if err != nil || !result.Success {
			response.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(response).Encode(result)
		if result.Success && result.OverlayResetPending {
			if flusher, ok := response.(http.Flusher); ok {
				flusher.Flush()
			}
			go m.resetOverlayAfterHelper(userID)
		}
	default:
		http.NotFound(response, request)
	}
}
