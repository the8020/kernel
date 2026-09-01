package development

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/sandbox/backend"
)

type Config struct {
	Root              string
	PackagesRoot      string
	ConfigRoot        string
	UsersRoot         string
	RuntimeRoot       string
	ImageRoot         string
	ImageRecord       string
	MountProfileFile  string
	MountProfile      []MountDefinition
	ActivationGateway ActivationGateway
	Driver            SandboxDriver
	RepositoryMu      *sync.RWMutex
	Logger            *slog.Logger
}

type Manager struct {
	config         Config
	driver         SandboxDriver
	workspaceMu    sync.Map
	workspaceOwner sync.Map
	sandboxMu      sync.Map
	imageMu        sync.RWMutex
	repositoryMu   *sync.RWMutex
	owned          sync.Map
	server         *http.Server
	listener       net.Listener
	endpoint       string
	cleanupCancel  context.CancelFunc
	cleanupDone    chan struct{}
}

const workspaceSchema = 3

func New(config Config) (*Manager, error) {
	for name, path := range map[string]string{"root": config.Root, "packages": config.PackagesRoot, "config": config.ConfigRoot, "users": config.UsersRoot, "runtime": config.RuntimeRoot, "image": config.ImageRoot} {
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
	for _, directory := range []string{config.ConfigRoot, config.UsersRoot, config.RuntimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, err
		}
	}
	for name, path := range map[string]*string{"config": &config.ConfigRoot, "users": &config.UsersRoot, "runtime": &config.RuntimeRoot} {
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
		return nil, fmt.Errorf("listen for workspace-scoped development API: %w", err)
	}
	m.listener, m.endpoint = listener, "http://"+listener.Addr().String()
	m.server = &http.Server{Handler: http.HandlerFunc(m.serveWorkspace), ReadHeaderTimeout: 5 * time.Second}
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
	type activeWorkspace struct{ sandboxID, workspaceID string }
	active := []activeWorkspace{}
	m.owned.Range(func(key, value any) bool {
		sandboxID, sandboxOK := key.(string)
		workspaceID, workspaceOK := value.(string)
		if sandboxOK && workspaceOK {
			active = append(active, activeWorkspace{sandboxID: sandboxID, workspaceID: workspaceID})
		}
		return true
	})
	for _, item := range active {
		unlock := m.lockWorkspace(item.workspaceID)
		workspace, err := m.loadWorkspace(item.workspaceID)
		if err != nil || workspace.ActiveSandboxID != item.sandboxID {
			joined = errors.Join(joined, err)
			unlock()
			continue
		}
		stopErr := m.driver.Stop(ctx, item.sandboxID)
		deleteErr := m.driver.Delete(ctx, item.sandboxID)
		m.owned.Delete(item.sandboxID)
		joined = errors.Join(joined, stopErr, deleteErr)
		workspace.State, workspace.ActiveSandboxID = StateStopped, ""
		workspace.ActivationActive, workspace.WritesPaused = false, false
		workspace.UpdatedAt = time.Now().UTC()
		joined = errors.Join(joined, m.saveWorkspace(workspace))
		unlock()
	}
	if m.server != nil {
		joined = errors.Join(joined, m.server.Shutdown(ctx))
	}
	return joined
}

func (m *Manager) Create(ctx context.Context, userID string) (Workspace, error) {
	if _, err := sandboxIDForUser(userID); err != nil {
		return Workspace{}, err
	}
	id := workspaceID(userID)
	unlock := m.lockWorkspace(id)
	defer unlock()
	if existing, err := m.loadWorkspaceForUser(id, userID); err == nil {
		return existing, fmt.Errorf("default development workspace already exists for user %s", userID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, err
	}
	return m.createLocked(ctx, id, userID)
}

// EnsureDefaultSandbox returns the authenticated user's running default
// development sandbox, creating or starting it only when necessary. Its
// deterministic workspace lookup never enumerates other users' workspaces.
func (m *Manager) EnsureDefaultSandbox(ctx context.Context, userID string) (string, error) {
	if _, err := sandboxIDForUser(userID); err != nil {
		return "", err
	}
	id := workspaceID(userID)
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspaceForUser(id, userID)
	if errors.Is(err, os.ErrNotExist) {
		workspace, err = m.createLocked(ctx, id, userID)
	} else if err == nil {
		err = m.startLocked(ctx, &workspace)
	}
	if err != nil {
		return "", err
	}
	return workspace.ActiveSandboxID, nil
}

func (m *Manager) createLocked(ctx context.Context, id, userID string) (Workspace, error) {
	if err := os.MkdirAll(filepath.Join(m.workspaceRootFor(userID, id), "runtime"), 0o700); err != nil {
		return Workspace{}, err
	}
	now := time.Now().UTC()
	token, err := randomHex(32)
	if err != nil {
		return Workspace{}, err
	}
	workspace := Workspace{Schema: workspaceSchema, WorkspaceID: id, OwnerUserID: userID, State: StateCreating, CreatedAt: now, UpdatedAt: now, Token: token, MountProfile: cloneMountProfile(m.config.MountProfile)}
	if err := m.preparePersistentMounts(&workspace); err != nil {
		return Workspace{}, err
	}
	if err := m.saveWorkspace(workspace); err != nil {
		return Workspace{}, err
	}
	if err := m.startLocked(ctx, &workspace); err != nil {
		workspace.State, workspace.ActiveSandboxID = StateFailed, ""
		workspace.UpdatedAt = time.Now().UTC()
		_ = m.saveWorkspace(workspace)
		return workspace, err
	}
	m.log("development workspace created", workspace, "result_state", workspace.State)
	return workspace, nil
}

func (m *Manager) List() ([]Workspace, error) {
	return m.listLocked()
}

func (m *Manager) Inspect(id string) (Workspace, error) {
	return m.loadWorkspace(id)
}

func (m *Manager) listLocked() ([]Workspace, error) {
	users, err := os.ReadDir(m.config.UsersRoot)
	if err != nil {
		return nil, err
	}
	result := []Workspace{}
	for _, user := range users {
		if !user.IsDir() || !safeUserID(user.Name()) {
			continue
		}
		entries, readErr := os.ReadDir(filepath.Join(m.userRoot(user.Name()), "workspaces"))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if !entry.IsDir() || !safeRuntimeID(entry.Name()) {
				continue
			}
			item, loadErr := m.loadWorkspaceForUser(entry.Name(), user.Name())
			if loadErr != nil {
				return nil, loadErr
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WorkspaceID < result[j].WorkspaceID })
	return result, nil
}

func (m *Manager) Start(ctx context.Context, id string) (Workspace, error) {
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		return Workspace{}, err
	}
	if err := m.startLocked(ctx, &workspace); err != nil {
		return workspace, err
	}
	return workspace, nil
}

func (m *Manager) startLocked(ctx context.Context, workspace *Workspace) error {
	if m.driver == nil {
		return errors.New("development sandbox driver is unavailable")
	}
	if workspace.ActiveSandboxID != "" {
		running, err := m.driver.Running(ctx, workspace.ActiveSandboxID)
		if err == nil && running {
			return nil
		}
		_ = m.driver.Delete(ctx, workspace.ActiveSandboxID)
		m.owned.Delete(workspace.ActiveSandboxID)
		workspace.ActiveSandboxID = ""
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
		return m.prepareWorkspaceStorage(ctx, workspace, image.Digest)
	}()
	if err != nil {
		return err
	}
	sandboxID, err := sandboxIDForUser(workspace.OwnerUserID)
	if err != nil {
		return err
	}
	workspace.State, workspace.ActiveSandboxID, workspace.DevelopmentImage = StateStarting, sandboxID, image.Digest
	workspace.UpdatedAt = time.Now().UTC()
	if err := m.saveWorkspace(*workspace); err != nil {
		return err
	}
	mounts, err := m.resolveMounts(*workspace)
	if err != nil {
		return err
	}
	unlockSandbox := m.lockSandbox(sandboxID)
	defer unlockSandbox()
	if err := m.driver.Delete(ctx, sandboxID); err != nil {
		workspace.ActiveSandboxID = ""
		return fmt.Errorf("delete inherited development sandbox %s: %w", sandboxID, err)
	}
	if err := m.driver.Start(ctx, SandboxStart{WorkspaceID: workspace.WorkspaceID, SandboxID: sandboxID, Packages: workspace.SourcePath, RootFS: workspace.SystemPath, Endpoint: m.endpoint, Token: workspace.Token, Mounts: mounts}); err != nil {
		workspace.ActiveSandboxID = ""
		return err
	}
	m.owned.Store(sandboxID, workspace.WorkspaceID)
	workspace.State = StateReady
	workspace.UpdatedAt = time.Now().UTC()
	workspace.CanSafelyReset = canSafelyReset(workspace)
	if err := m.saveWorkspace(*workspace); err != nil {
		return err
	}
	m.log("development sandbox started", *workspace, "sandbox_id", sandboxID, "result_state", workspace.State)
	return nil
}

func (m *Manager) prepareWorkspaceStorage(ctx context.Context, workspace *Workspace, imageDigest string) error {
	if err := m.preparePersistentMounts(workspace); err != nil {
		return err
	}
	source := filepath.Join(m.workspaceDataRoot(*workspace), "source", "packages")
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		if err := copyDirectory(ctx, m.config.PackagesRoot, source); err != nil {
			return fmt.Errorf("initialize durable development source: %w", err)
		}
		if err := m.initializeBases(ctx, *workspace, source); err != nil {
			_ = os.RemoveAll(m.workspaceDataRoot(*workspace))
			return err
		}
	} else if err != nil {
		return err
	}
	canonicalSource, err := canonicalDirectory(source)
	if err != nil {
		return err
	}
	workspace.SourcePath = canonicalSource
	systemRoot, err := m.systemRootPath(workspace.OwnerUserID, imageDigest)
	if err != nil {
		return err
	}
	if _, err := os.Stat(systemRoot); errors.Is(err, os.ErrNotExist) {
		if err := copySystemRoot(ctx, m.config.ImageRoot, systemRoot); err != nil {
			return fmt.Errorf("initialize durable development system: %w", err)
		}
	} else if err != nil {
		return err
	}
	workspace.SystemPath, err = canonicalDirectory(systemRoot)
	return err
}

func (m *Manager) initializeBases(ctx context.Context, workspace Workspace, source string) error {
	document := basesDocument{Schema: 1, Packages: map[string]baseDocument{}}
	for _, id := range packageDirectories(source) {
		root := filepath.Join(source, filepath.FromSlash(id))
		if info, err := os.Stat(filepath.Join(root, ".git")); err != nil || !info.IsDir() {
			continue
		}
		head, err := gitOutputContext(ctx, root, nil, "rev-parse", "HEAD")
		if err != nil || head == "" {
			continue
		}
		document.Packages[id] = baseDocument{BaseCommit: head}
	}
	return writeBases(filepath.Join(m.workspaceRoot(workspace), "bases.toml"), document)
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

func (m *Manager) Stop(ctx context.Context, id string) (Workspace, error) {
	return m.stop(ctx, id, false)
}

func (m *Manager) Kill(ctx context.Context, id string) (Workspace, error) {
	return m.stop(ctx, id, true)
}

func (m *Manager) stop(ctx context.Context, id string, kill bool) (Workspace, error) {
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		return Workspace{}, err
	}
	if workspace.ActiveSandboxID == "" {
		return workspace, nil
	}
	workspace.State = StateStopping
	workspace.UpdatedAt = time.Now().UTC()
	_ = m.saveWorkspace(workspace)
	if kill {
		err = m.driver.Kill(ctx, workspace.ActiveSandboxID)
	} else {
		err = m.driver.Stop(ctx, workspace.ActiveSandboxID)
	}
	if err != nil {
		return workspace, err
	}
	if err := m.driver.Delete(ctx, workspace.ActiveSandboxID); err != nil {
		return workspace, err
	}
	oldSandbox := workspace.ActiveSandboxID
	m.owned.Delete(oldSandbox)
	workspace.ActiveSandboxID, workspace.State = "", StateStopped
	workspace.UpdatedAt = time.Now().UTC()
	err = m.saveWorkspace(workspace)
	m.log("development sandbox stopped", workspace, "sandbox_id", oldSandbox, "result_state", workspace.State)
	return workspace, err
}

func (m *Manager) Restart(ctx context.Context, id string) (Workspace, error) {
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		return Workspace{}, err
	}
	if workspace.ActiveSandboxID != "" {
		_ = m.driver.Stop(ctx, workspace.ActiveSandboxID)
		if err := m.driver.Delete(ctx, workspace.ActiveSandboxID); err != nil {
			return workspace, err
		}
		m.owned.Delete(workspace.ActiveSandboxID)
		workspace.ActiveSandboxID = ""
	}
	if err := m.startLocked(ctx, &workspace); err != nil {
		return workspace, err
	}
	return workspace, nil
}

func (m *Manager) Delete(ctx context.Context, id string, deleteUserData bool) error {
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		return err
	}
	if workspace.ActiveSandboxID != "" {
		_ = m.driver.Kill(ctx, workspace.ActiveSandboxID)
		if err := m.driver.Delete(ctx, workspace.ActiveSandboxID); err != nil {
			return err
		}
		m.owned.Delete(workspace.ActiveSandboxID)
	}
	if err := os.RemoveAll(m.workspaceRoot(workspace)); err != nil {
		return err
	}
	if deleteUserData {
		if err := os.RemoveAll(m.userRoot(workspace.OwnerUserID)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Shell(ctx context.Context, id, command string) (ShellResult, error) {
	lock := m.workspaceLock(id)
	lock.Lock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		lock.Unlock()
		return ShellResult{}, err
	}
	if workspace.ActiveSandboxID == "" {
		lock.Unlock()
		return ShellResult{}, errors.New("development sandbox is not running")
	}
	sandboxID := workspace.ActiveSandboxID
	workspace.State, workspace.UpdatedAt = StateBusy, time.Now().UTC()
	if err := m.saveWorkspace(workspace); err != nil {
		lock.Unlock()
		return ShellResult{}, err
	}
	lock.Unlock()
	output, err := m.driver.Exec(ctx, sandboxID, command)
	lock.Lock()
	current, loadErr := m.loadWorkspace(id)
	if loadErr == nil && current.ActiveSandboxID == sandboxID && current.State == StateBusy {
		current.State, current.UpdatedAt = StateReady, time.Now().UTC()
		_ = m.saveWorkspace(current)
	}
	lock.Unlock()
	return ShellResult{WorkspaceID: id, SandboxID: sandboxID, Command: command, Output: string(output)}, err
}

func (m *Manager) OpenConsole(ctx context.Context, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	owned, ok := m.owned.Load(sandboxID)
	if !ok {
		return nil, errors.New("development sandbox is not ready")
	}
	workspaceID, ok := owned.(string)
	if !ok {
		return nil, errors.New("development sandbox ownership is unavailable")
	}
	workspace, err := m.loadWorkspace(workspaceID)
	if err != nil || workspace.ActiveSandboxID != sandboxID || (workspace.State != StateReady && workspace.State != StateConflicted) {
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

func (m *Manager) ResetSource(ctx context.Context, id string, confirmed bool) (Workspace, error) {
	if !confirmed {
		return Workspace{}, errors.New("source reset requires explicit confirmation")
	}
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		return Workspace{}, err
	}
	if workspace.ActiveSandboxID != "" {
		_ = m.driver.Kill(ctx, workspace.ActiveSandboxID)
		if err := m.driver.Delete(ctx, workspace.ActiveSandboxID); err != nil {
			return workspace, err
		}
		m.owned.Delete(workspace.ActiveSandboxID)
		workspace.ActiveSandboxID = ""
	}
	if err := os.RemoveAll(filepath.Join(m.workspaceDataRoot(workspace), "source")); err != nil {
		return workspace, err
	}
	_ = os.Remove(filepath.Join(m.workspaceRoot(workspace), "bases.toml"))
	workspace.SourcePath, workspace.ConflictedPackages, workspace.State = "", nil, StateResetting
	if err := m.startLocked(ctx, &workspace); err != nil {
		return workspace, err
	}
	return workspace, nil
}

func (m *Manager) FactoryReset(ctx context.Context, id string, confirmed bool) (Workspace, error) {
	if !confirmed {
		return Workspace{}, errors.New("factory reset requires explicit confirmation")
	}
	unlock := m.lockWorkspace(id)
	defer unlock()
	old, err := m.loadWorkspace(id)
	if err != nil {
		return Workspace{}, err
	}
	if old.ActiveSandboxID != "" {
		_ = m.driver.Kill(ctx, old.ActiveSandboxID)
		if err := m.driver.Delete(ctx, old.ActiveSandboxID); err != nil {
			return Workspace{}, err
		}
		m.owned.Delete(old.ActiveSandboxID)
	}
	for _, path := range []string{
		m.workspaceRoot(old),
		filepath.Join(m.userRoot(old.OwnerUserID), "system"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return Workspace{}, err
		}
	}
	if err := os.MkdirAll(filepath.Join(m.workspaceRootFor(old.OwnerUserID, id), "runtime"), 0o700); err != nil {
		return Workspace{}, err
	}
	token, err := randomHex(32)
	if err != nil {
		return Workspace{}, err
	}
	now := time.Now().UTC()
	workspace := Workspace{Schema: workspaceSchema, WorkspaceID: id, OwnerUserID: old.OwnerUserID, State: StateResetting, CreatedAt: now, UpdatedAt: now, Token: token, MountProfile: cloneMountProfile(m.config.MountProfile)}
	if err := m.preparePersistentMounts(&workspace); err != nil {
		return Workspace{}, err
	}
	if err := m.saveWorkspace(workspace); err != nil {
		return Workspace{}, err
	}
	if err := m.startLocked(ctx, &workspace); err != nil {
		return workspace, err
	}
	return workspace, nil
}

func (m *Manager) workspaceRootFor(userID, id string) string {
	return filepath.Join(m.userRoot(userID), "workspaces", id)
}

func (m *Manager) workspaceRoot(workspace Workspace) string {
	return m.workspaceRootFor(workspace.OwnerUserID, workspace.WorkspaceID)
}

func (m *Manager) workspaceDataRoot(workspace Workspace) string {
	return m.workspaceRoot(workspace)
}

func (m *Manager) workspaceLock(id string) *sync.Mutex {
	value, _ := m.workspaceMu.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (m *Manager) lockWorkspace(id string) func() {
	lock := m.workspaceLock(id)
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
	return filepath.Join(m.userRoot(userID), "system", hex.EncodeToString(digest[:]), "rootfs"), nil
}

func (m *Manager) loadWorkspace(id string) (Workspace, error) {
	if !safeRuntimeID(id) {
		return Workspace{}, errors.New("invalid development workspace ID")
	}
	if owner, ok := m.workspaceOwner.Load(id); ok {
		if userID, valid := owner.(string); valid {
			workspace, err := m.loadWorkspaceForUser(id, userID)
			if err == nil || !errors.Is(err, os.ErrNotExist) {
				return workspace, err
			}
			m.workspaceOwner.Delete(id)
		}
	}
	users, err := os.ReadDir(m.config.UsersRoot)
	if err != nil {
		return Workspace{}, err
	}
	for _, user := range users {
		if !user.IsDir() || !safeUserID(user.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(m.workspaceRootFor(user.Name(), id), "workspace.toml")); err == nil {
			return m.loadWorkspaceForUser(id, user.Name())
		} else if !errors.Is(err, os.ErrNotExist) {
			return Workspace{}, err
		}
	}
	return Workspace{}, os.ErrNotExist
}

func (m *Manager) loadWorkspaceForUser(id, userID string) (Workspace, error) {
	if !safeRuntimeID(id) || !safeUserID(userID) {
		return Workspace{}, errors.New("invalid development workspace identity")
	}
	var workspace Workspace
	if err := readTOML(filepath.Join(m.workspaceRootFor(userID, id), "workspace.toml"), &workspace); err != nil {
		return Workspace{}, err
	}
	if workspace.WorkspaceID != id || workspace.OwnerUserID != userID {
		return Workspace{}, errors.New("development workspace identity mismatch")
	}
	if workspace.Schema != workspaceSchema {
		return Workspace{}, fmt.Errorf("unsupported development workspace schema %d", workspace.Schema)
	}
	m.workspaceOwner.Store(id, userID)
	if workspace.ActiveSandboxID != "" {
		if _, owned := m.owned.Load(workspace.ActiveSandboxID); !owned {
			workspace.ActiveSandboxID = ""
			workspace.State = StateStopped
			workspace.ActivationActive, workspace.WritesPaused = false, false
			workspace.UpdatedAt = time.Now().UTC()
			if err := m.saveWorkspace(workspace); err != nil {
				return Workspace{}, err
			}
		}
	}
	if err := validateMountProfile(m.config, workspace.MountProfile); err != nil {
		return Workspace{}, fmt.Errorf("validate persisted development mount profile: %w", err)
	}
	workspace.CanSafelyReset = canSafelyReset(&workspace)
	return workspace, nil
}

func (m *Manager) saveWorkspace(workspace Workspace) error {
	if !safeRuntimeID(workspace.WorkspaceID) || !safeUserID(workspace.OwnerUserID) {
		return errors.New("invalid development workspace identity")
	}
	m.workspaceOwner.Store(workspace.WorkspaceID, workspace.OwnerUserID)
	return writeTOML(filepath.Join(m.workspaceRoot(workspace), "workspace.toml"), workspace, 0o600)
}

func canSafelyReset(workspace *Workspace) bool {
	if workspace == nil || workspace.ActivationActive || workspace.WritesPaused {
		return false
	}
	switch workspace.State {
	case StateReady, StateConflicted, StateStopped, StateFailed:
		return true
	default:
		return false
	}
}

func workspaceID(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return "default-" + hex.EncodeToString(sum[:8])
}

func safeUserID(value string) bool {
	return auth.ValidateUsername(value) == nil
}

func sandboxIDForUser(userID string) (string, error) {
	if err := auth.ValidateUsername(userID); err != nil {
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

func (m *Manager) log(message string, workspace Workspace, values ...any) {
	if m.config.Logger == nil {
		return
	}
	fields := []any{"developer_user_id", workspace.OwnerUserID, "workspace_id", workspace.WorkspaceID}
	m.config.Logger.Info(message, append(fields, values...)...)
}

func (m *Manager) ImageStatus() (ImageStatus, error) {
	m.imageMu.RLock()
	defer m.imageMu.RUnlock()
	return m.imageStatusLocked()
}

func (m *Manager) imageStatusLocked() (ImageStatus, error) {
	var record struct {
		Digest       string    `json:"digest"`
		ImageDigest  string    `json:"image_digest"`
		BuiltAt      time.Time `json:"built_at"`
		CodexVersion string    `json:"codex_version"`
		DenoVersion  string    `json:"deno_version"`
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
	return ImageStatus{Digest: digest, BuiltAt: record.BuiltAt, CodexVersion: record.CodexVersion, DenoVersion: record.DenoVersion, BuildStatus: "ready"}, nil
}

func (m *Manager) serveWorkspace(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "development" || parts[2] != "workspaces" {
		http.NotFound(response, request)
		return
	}
	id, operation := parts[3], parts[4]
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		http.Error(response, "workspace not found", http.StatusNotFound)
		return
	}
	if request.Header.Get("Authorization") != "Bearer "+workspace.Token {
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
		result, err := m.config.ActivationGateway.Preview(request.Context(), id, options)
		if err != nil {
			http.Error(response, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(response).Encode(result)
	case "activate":
		result, err := m.config.ActivationGateway.Activate(request.Context(), id, options)
		if err != nil || !result.Success {
			response.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(response).Encode(result)
	default:
		http.NotFound(response, request)
	}
}
