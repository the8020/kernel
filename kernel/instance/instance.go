// Package instance owns instance-root resolution and the on-disk kernel instance.
package instance

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

// ErrAlreadyRunning reports that another process holds the instance lock.
var ErrAlreadyRunning = errors.New("kernel instance is already running")

// ErrNotInitialized reports that a directory has no committed node layout.
var ErrNotInitialized = errors.New("80|20 node is not initialized")

// Paths contains every path owned by one kernel instance.
type Paths struct {
	Root                  string
	Node                  string
	Kernel                string
	Bin                   string
	Runsc                 string
	Users                 string
	Packages              string
	Config                string
	ConfigAuth            string
	RuntimeRootlessImage  string
	RuntimeFullImage      string
	DevelopmentImage      string
	RuntimeVersionsFile   string
	SharedState           string
	StateAuth             string
	BootstrapSessions     string
	StateServices         string
	StatePackageIndex     string
	StatePackageData      string
	InstanceFile          string
	NodeSettingsFile      string
	GlobalSettingsFile    string
	Run                   string
	Logs                  string
	Runtime               string
	RuntimeGroups         string
	RuntimeSandboxHistory string
	RuntimePorts          string
	RuntimeServices       string
	RuntimeServicePools   string
	RuntimeAttachments    string
	RuntimeTemporary      string
	RuntimeDevelopment    string
	SSH                   string
	SSHHostKey            string
	LockFile              string
	PIDFile               string
	Socket                string
}

// Layout is the small, durable mapping between one node and the externally
// synchronized system directories it uses. All values are canonical absolute
// paths so kernel behavior never depends on its process working directory.
type Layout struct {
	Version  int    `toml:"version" json:"version"`
	Packages string `toml:"packages" json:"packages"`
	Config   string `toml:"config" json:"config"`
	State    string `toml:"state" json:"state"`
	Users    string `toml:"users" json:"users"`
}

// LayoutManager exposes the bootstrap layout to the typed administrative
// surface. Changes are durable immediately and take effect after restart.
type LayoutManager struct{ root string }

func NewLayoutManager(root string) *LayoutManager { return &LayoutManager{root: root} }

func (m *LayoutManager) Current() (Layout, error) {
	paths, err := LoadPaths(m.root)
	if err != nil {
		return Layout{}, err
	}
	return Layout{Version: layoutVersion, Packages: paths.Packages, Config: paths.Config, State: paths.SharedState, Users: paths.Users}, nil
}

func (m *LayoutManager) Set(layout Layout) (Layout, error) {
	for name, path := range map[string]string{"packages": layout.Packages, "config": layout.Config, "state": layout.State, "users": layout.Users} {
		if !filepath.IsAbs(path) {
			return Layout{}, fmt.Errorf("%s directory must be absolute", name)
		}
	}
	paths, err := PrepareLayout(m.root, layout)
	if err != nil {
		return Layout{}, err
	}
	for name, path := range map[string]string{"node": paths.Node, "packages": paths.Packages, "config": paths.Config, "state": paths.SharedState, "users": paths.Users} {
		if err := CheckUnixPermissions(path); err != nil {
			return Layout{}, fmt.Errorf("%s directory does not support required Unix permissions: %w", name, err)
		}
	}
	if _, err := WriteLayout(m.root, layout); err != nil {
		return Layout{}, err
	}
	return m.Current()
}

const layoutVersion = 1

// ResolveRoot returns the canonical explicit root, or the exact canonical
// current working directory when root is empty. An explicit root may name a
// directory that initialization has not created yet.
func ResolveRoot(root string) (string, error) {
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get current working directory: %w", err)
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve instance root: %w", err)
	}
	canonical, err := canonicalRoot(abs)
	if err != nil {
		return "", fmt.Errorf("resolve instance root: %w", err)
	}
	return canonical, nil
}

func canonicalRoot(path string) (string, error) {
	current := path
	missing := make([]string, 0, 2)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("instance root ancestor is not a directory: %s", current)
			}
			canonical, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				canonical = filepath.Join(canonical, missing[index])
			}
			return canonical, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, linkErr := os.Lstat(current); linkErr == nil {
			return "", fmt.Errorf("instance root contains a dangling symbolic link: %s", current)
		} else if !errors.Is(linkErr, os.ErrNotExist) {
			return "", linkErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// NewPaths constructs the default mapped and node-local paths for root.
func NewPaths(root string) Paths {
	return NewPathsForLayout(root, Layout{
		Version: layoutVersion, Packages: filepath.Join(root, "packages"),
		Config: filepath.Join(root, "config"), State: filepath.Join(root, "state"),
		Users: filepath.Join(root, "users"),
	})
}

// NewPathsForLayout constructs paths from one initialized node layout.
func NewPathsForLayout(root string, layout Layout) Paths {
	node := filepath.Join(root, "node")
	kernel := filepath.Join(node, "kernel")
	run := filepath.Join(kernel, "run")
	runtimeState := filepath.Join(kernel, "runtime")
	runtimeImages := filepath.Join(runtimeState, "images")
	return Paths{
		Root: root, Node: node, Kernel: kernel, Bin: filepath.Join(kernel, "bin"), Runsc: filepath.Join(kernel, "bin", "runsc"), Users: layout.Users, Packages: layout.Packages,
		Config: layout.Config, ConfigAuth: filepath.Join(layout.Config, "auth"),
		RuntimeRootlessImage: filepath.Join(runtimeImages, "rootless"), RuntimeFullImage: filepath.Join(runtimeImages, "full"),
		DevelopmentImage: filepath.Join(runtimeImages, "development"), RuntimeVersionsFile: filepath.Join(layout.Config, "runtime", "versions.toml"),
		SharedState: layout.State, StateAuth: filepath.Join(layout.State, "auth"), BootstrapSessions: filepath.Join(layout.State, "auth", "bootstrap-sessions"), StateServices: filepath.Join(layout.State, "services"), StatePackageIndex: filepath.Join(layout.State, "package-index"), StatePackageData: filepath.Join(layout.State, "package-data"),
		InstanceFile:     filepath.Join(kernel, "instance.toml"),
		NodeSettingsFile: filepath.Join(kernel, "settings.toml"), GlobalSettingsFile: filepath.Join(layout.Config, "settings.toml"), Run: run,
		Logs: filepath.Join(kernel, "logs"), Runtime: runtimeState,
		RuntimeGroups: filepath.Join(runtimeState, "groups"), RuntimeSandboxHistory: filepath.Join(runtimeState, "sandbox-history"), RuntimePorts: filepath.Join(runtimeState, "ports"), RuntimeServices: filepath.Join(runtimeState, "services"), RuntimeServicePools: filepath.Join(runtimeState, "service-pools"),
		RuntimeAttachments: filepath.Join(runtimeState, "attachments"), RuntimeTemporary: filepath.Join(runtimeState, "tmp"),
		RuntimeDevelopment: filepath.Join(runtimeState, "development"),
		SSH:                filepath.Join(kernel, "ssh"),
		SSHHostKey:         filepath.Join(kernel, "ssh", "host_ed25519"),
		LockFile:           filepath.Join(run, "kernel.lock"),
		PIDFile:            filepath.Join(run, "kernel.pid"), Socket: filepath.Join(run, "admin.sock"),
	}
}

// LayoutFile is readable before the remainder of an instance is initialized.
func LayoutFile(root string) string { return filepath.Join(root, "node", "kernel", "paths.toml") }

// LoadPaths reads the initialized layout below root.
func LoadPaths(root string) (Paths, error) {
	data, err := os.ReadFile(LayoutFile(root))
	if errors.Is(err, os.ErrNotExist) {
		return Paths{}, ErrNotInitialized
	}
	if err != nil {
		return Paths{}, err
	}
	var layout Layout
	if err := toml.Unmarshal(data, &layout); err != nil {
		return Paths{}, fmt.Errorf("decode node layout: %w", err)
	}
	if layout.Version != layoutVersion {
		return Paths{}, fmt.Errorf("unsupported node layout version %d", layout.Version)
	}
	for name, value := range map[string]string{"packages": layout.Packages, "config": layout.Config, "state": layout.State, "users": layout.Users} {
		if !filepath.IsAbs(value) {
			return Paths{}, fmt.Errorf("%s path is not absolute", name)
		}
	}
	for name, value := range map[string]*string{"packages": &layout.Packages, "config": &layout.Config, "state": &layout.State, "users": &layout.Users} {
		canonical, canonicalErr := canonicalExistingDirectory(*value)
		if canonicalErr != nil {
			return Paths{}, fmt.Errorf("resolve %s directory: %w", name, canonicalErr)
		}
		*value = canonical
	}
	paths := NewPathsForLayout(root, layout)
	if err := validateLayoutPaths(paths); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

// WriteLayout records the selected shared roots without interpreting how an
// operator synchronizes them.
func WriteLayout(root string, layout Layout) (Paths, error) {
	paths, err := PrepareLayout(root, layout)
	if err != nil {
		return Paths{}, err
	}
	layout = Layout{Version: layoutVersion, Packages: paths.Packages, Config: paths.Config, State: paths.SharedState, Users: paths.Users}
	data, err := toml.Marshal(layout)
	if err != nil {
		return Paths{}, fmt.Errorf("encode node layout: %w", err)
	}
	if err := writeLayoutFile(LayoutFile(root), data); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

// PrepareLayout creates and canonicalizes the selected roots without committing
// paths.toml. Callers can therefore prove filesystem behavior before a layout
// becomes active.
func PrepareLayout(root string, layout Layout) (Paths, error) {
	layout.Version = layoutVersion
	for name, value := range map[string]*string{"packages": &layout.Packages, "config": &layout.Config, "state": &layout.State, "users": &layout.Users} {
		if strings.TrimSpace(*value) == "" {
			return Paths{}, fmt.Errorf("%s directory is required", name)
		}
		canonical, err := canonicalDirectory(*value)
		if err != nil {
			return Paths{}, fmt.Errorf("prepare %s directory: %w", name, err)
		}
		*value = canonical
	}
	paths := NewPathsForLayout(root, layout)
	if err := validateLayoutPaths(paths); err != nil {
		return Paths{}, err
	}
	if err := os.MkdirAll(paths.Kernel, 0o700); err != nil {
		return Paths{}, fmt.Errorf("create node kernel directory: %w", err)
	}
	return paths, nil
}

func validateLayoutPaths(paths Paths) error {
	shared := map[string]string{"packages": paths.Packages, "config": paths.Config, "state": paths.SharedState, "users": paths.Users}
	for firstName, first := range shared {
		if overlaps(first, paths.Node) {
			return fmt.Errorf("%s directory overlaps node directory", firstName)
		}
		for secondName, second := range shared {
			if firstName < secondName && overlaps(first, second) {
				return fmt.Errorf("%s and %s directories overlap", firstName, secondName)
			}
		}
	}
	return nil
}

func overlaps(first, second string) bool {
	if first == second {
		return true
	}
	firstToSecond, err := filepath.Rel(first, second)
	if err == nil && firstToSecond != ".." && !strings.HasPrefix(firstToSecond, ".."+string(filepath.Separator)) {
		return true
	}
	secondToFirst, err := filepath.Rel(second, first)
	return err == nil && secondToFirst != ".." && !strings.HasPrefix(secondToFirst, ".."+string(filepath.Separator))
}

func writeLayoutFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".paths-*.toml")
	if err != nil {
		return fmt.Errorf("create node layout temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("write node layout: %w", err)
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func canonicalExistingDirectory(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", canonical)
	}
	return canonical, nil
}

// CheckUnixPermissions proves the ownership and mode operations required by
// package managers on a disposable file in dir. It leaves no artifact behind.
func CheckUnixPermissions(dir string) error {
	file, err := os.CreateTemp(dir, ".the8020-permissions-")
	if err != nil {
		return fmt.Errorf("create permission probe: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Close(); err != nil {
		return fmt.Errorf("close permission probe: %w", err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	probeUID, probeGID := uid, gid
	if os.Geteuid() == 0 {
		probeUID, probeGID = 1, 1
	}
	if err := os.Chown(name, probeUID, probeGID); err != nil {
		return fmt.Errorf("change file ownership: %w", err)
	}
	if err := verifyProbeMetadata(name, probeUID, probeGID, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o000); err != nil {
		return fmt.Errorf("change file mode: %w", err)
	}
	if err := verifyProbeMetadata(name, probeUID, probeGID, 0o000); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("restore file mode: %w", err)
	}
	if err := verifyProbeMetadata(name, probeUID, probeGID, 0o600); err != nil {
		return err
	}
	if err := os.Chown(name, uid, gid); err != nil {
		return fmt.Errorf("restore file ownership: %w", err)
	}
	if err := verifyProbeMetadata(name, uid, gid, 0o600); err != nil {
		return err
	}
	return nil
}

func verifyProbeMetadata(path string, uid, gid int, mode fs.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return fmt.Errorf("inspect permission probe: %w", err)
	}
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		return fmt.Errorf("filesystem did not preserve ownership change: got %d:%d, want %d:%d", stat.Uid, stat.Gid, uid, gid)
	}
	if actual := fs.FileMode(stat.Mode).Perm(); actual != mode.Perm() {
		return fmt.Errorf("filesystem did not preserve mode change: got %04o, want %04o", actual, mode.Perm())
	}
	return nil
}

// Initialize creates missing runtime directories, settings, and stable identity.
func Initialize(paths Paths) (string, error) {
	for _, dir := range []string{paths.Packages, paths.Config, paths.ConfigAuth, paths.SharedState, paths.StateAuth, paths.StateServices, paths.StatePackageIndex, paths.StatePackageData, paths.Users} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create workspace directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return "", fmt.Errorf("set workspace directory permissions %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(paths.BootstrapSessions, 0o700); err != nil {
		return "", fmt.Errorf("create bootstrap authentication-session directory %s: %w", paths.BootstrapSessions, err)
	}
	if err := os.Chmod(paths.BootstrapSessions, 0o700); err != nil {
		return "", fmt.Errorf("restrict bootstrap authentication-session directory %s: %w", paths.BootstrapSessions, err)
	}
	for _, dir := range []string{paths.Kernel, paths.Bin, paths.RuntimeRootlessImage, paths.RuntimeFullImage, paths.DevelopmentImage, paths.Run, paths.Logs, paths.Runtime, paths.RuntimeGroups, paths.RuntimeSandboxHistory, paths.RuntimePorts, paths.RuntimeServices, paths.RuntimeServicePools, paths.RuntimeAttachments, paths.RuntimeTemporary, paths.RuntimeDevelopment, paths.SSH} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create runtime directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("restrict runtime directory %s: %w", dir, err)
		}
	}
	// This root is mounted directly as read-only /artifacts for the non-root
	// runtime user. Its private node/kernel ancestors remain mode 0700.
	if err := os.Chmod(paths.RuntimeAttachments, 0o755); err != nil {
		return "", fmt.Errorf("make runtime attachments sandbox-readable: %w", err)
	}
	if err := ensureEmptyFile(paths.NodeSettingsFile); err != nil {
		return "", err
	}
	if err := ensureEmptyFile(paths.GlobalSettingsFile); err != nil {
		return "", err
	}
	return ensureIdentity(paths.InstanceFile)
}

func ensureEmptyFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return os.Chmod(path, 0o600)
	}
	if err != nil {
		return fmt.Errorf("initialize %s: %w", path, err)
	}
	return file.Close()
}

func ensureIdentity(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return parseIdentity(string(data))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read instance identity: %w", err)
	}
	uuid, err := newUUID()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", fmt.Errorf("read concurrently initialized identity: %w", readErr)
		}
		return parseIdentity(string(data))
	}
	if err != nil {
		return "", fmt.Errorf("create instance identity: %w", err)
	}
	if _, err = fmt.Fprintf(file, "uuid = %q\n", uuid); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write instance identity: %w", err)
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("sync instance identity: %w", err)
	}
	if err = file.Close(); err != nil {
		return "", fmt.Errorf("close instance identity: %w", err)
	}
	return uuid, nil
}

func parseIdentity(data string) (string, error) {
	line := strings.TrimSpace(data)
	prefix := "uuid = \""
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "\"") {
		return "", errors.New("invalid node/kernel/instance.toml")
	}
	uuid := strings.TrimSuffix(strings.TrimPrefix(line, prefix), "\"")
	if len(uuid) != 36 {
		return "", errors.New("invalid UUID in node/kernel/instance.toml")
	}
	return uuid, nil
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate instance UUID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

// Lock is the authoritative process-lifetime lock for one instance.
type Lock struct {
	file  *os.File
	paths Paths
}

// Acquire takes the instance lock, removes stale runtime endpoints, and writes
// the informational PID file.
func Acquire(paths Paths) (*Lock, error) {
	file, err := os.OpenFile(paths.LockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("acquire instance lock: %w", err)
	}
	lock := &Lock{file: file, paths: paths}
	for _, stale := range []string{paths.PIDFile, paths.Socket} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = lock.Release()
			return nil, fmt.Errorf("remove stale runtime file %s: %w", stale, err)
		}
	}
	if err := os.WriteFile(paths.PIDFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		_ = lock.Release()
		return nil, fmt.Errorf("write PID file: %w", err)
	}
	return lock, nil
}

// Release removes ephemeral runtime files and releases the process lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	var joined error
	for _, path := range []string{l.paths.Socket, l.paths.PIDFile} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			joined = errors.Join(joined, err)
		}
	}
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := l.file.Close(); err != nil {
		joined = errors.Join(joined, err)
	}
	l.file = nil
	return joined
}
