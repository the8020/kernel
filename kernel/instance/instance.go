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
	Root                   string
	Node                   string
	Kernel                 string
	Bin                    string
	Runsc                  string
	Users                  string
	Packages               string
	RuntimeDefinitions     string
	RuntimeRootlessImage   string
	RuntimeFullImage       string
	DevelopmentImage       string
	RuntimeVersionsFile    string
	Database               string
	NodeSettingsFile       string
	Run                    string
	Logs                   string
	Runtime                string
	RuntimeGroups          string
	RuntimeSandboxHistory  string
	RuntimePorts           string
	RuntimeServices        string
	RuntimeServicePools    string
	RuntimeAttachments     string
	RuntimeTemporary       string
	RuntimeDevelopment     string
	RuntimeKernelSocketDir string
	RuntimeKernelSocket    string
	SSH                    string
	SSHHostKey             string
	LockFile               string
	PIDFile                string
	Socket                 string
}

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
	node := filepath.Join(root, "node")
	kernel := filepath.Join(node, "kernel")
	run := filepath.Join(kernel, "run")
	runtimeState := filepath.Join(kernel, "runtime")
	runtimeImages := filepath.Join(runtimeState, "images")
	runtimeDefinitions := filepath.Join(runtimeState, "definitions")
	return Paths{
		Root: root, Node: node, Kernel: kernel, Bin: filepath.Join(kernel, "bin"), Runsc: filepath.Join(kernel, "bin", "runsc"),
		Users: filepath.Join(root, "users"), Packages: filepath.Join(root, "packages"), RuntimeDefinitions: runtimeDefinitions,
		RuntimeRootlessImage: filepath.Join(runtimeImages, "rootless"), RuntimeFullImage: filepath.Join(runtimeImages, "full"),
		DevelopmentImage: filepath.Join(runtimeImages, "development"), RuntimeVersionsFile: filepath.Join(runtimeDefinitions, "versions.toml"),
		Database: filepath.Join(root, "database"), NodeSettingsFile: filepath.Join(root, "kernel.toml"), Run: run,
		Logs: filepath.Join(kernel, "logs"), Runtime: runtimeState,
		RuntimeGroups: filepath.Join(runtimeState, "groups"), RuntimeSandboxHistory: filepath.Join(runtimeState, "sandbox-history"), RuntimePorts: filepath.Join(runtimeState, "ports"), RuntimeServices: filepath.Join(runtimeState, "services"), RuntimeServicePools: filepath.Join(runtimeState, "service-pools"),
		RuntimeAttachments: filepath.Join(runtimeState, "attachments"), RuntimeTemporary: filepath.Join(runtimeState, "tmp"),
		RuntimeDevelopment:     filepath.Join(runtimeState, "development"),
		RuntimeKernelSocketDir: filepath.Join(runtimeState, "kernel-api"),
		RuntimeKernelSocket:    filepath.Join(runtimeState, "kernel-api", "kernel.sock"),
		SSH:                    filepath.Join(kernel, "ssh"),
		SSHHostKey:             filepath.Join(kernel, "ssh", "host_ed25519"),
		LockFile:               filepath.Join(run, "kernel.lock"),
		PIDFile:                filepath.Join(run, "kernel.pid"), Socket: filepath.Join(run, "admin.sock"),
	}
}

// LoadPaths resolves the fixed layout after the canonical kernel configuration
// file has committed node initialization.
func LoadPaths(root string) (Paths, error) {
	paths := NewPaths(root)
	info, err := os.Stat(paths.NodeSettingsFile)
	if errors.Is(err, os.ErrNotExist) {
		return Paths{}, ErrNotInitialized
	}
	if err != nil {
		return Paths{}, err
	}
	if !info.Mode().IsRegular() {
		return Paths{}, errors.New("kernel.toml must be a regular file")
	}
	return paths, nil
}

// Prepare creates the fixed node-local and package/user roots and commits an
// empty kernel.toml marker. Initialize fills the stable node identity.
func Prepare(root string) (Paths, error) {
	paths := NewPaths(root)
	for _, directory := range []string{root, paths.Packages, paths.Users} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return Paths{}, err
		}
	}
	if err := os.MkdirAll(paths.Kernel, 0o700); err != nil {
		return Paths{}, err
	}
	if err := ensureEmptyFile(paths.NodeSettingsFile); err != nil {
		return Paths{}, err
	}
	return paths, nil
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

// Initialize creates fixed runtime directories and a stable identity in the
// canonical node-local kernel.toml.
func Initialize(paths Paths) (string, error) {
	for _, dir := range []string{paths.Packages, paths.Users} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create shared directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return "", fmt.Errorf("set workspace directory permissions %s: %w", dir, err)
		}
	}
	for _, dir := range []string{paths.Kernel, paths.Bin, paths.Database, paths.RuntimeDefinitions, paths.RuntimeRootlessImage, paths.RuntimeFullImage, paths.DevelopmentImage, paths.Run, paths.Logs, paths.Runtime, paths.RuntimeGroups, paths.RuntimeSandboxHistory, paths.RuntimePorts, paths.RuntimeServices, paths.RuntimeServicePools, paths.RuntimeAttachments, paths.RuntimeTemporary, paths.RuntimeDevelopment, paths.RuntimeKernelSocketDir, paths.SSH} {
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
	// This directory is bind-mounted into sandboxes. Its parent remains private;
	// the socket itself is authenticated with each sandbox's internal token.
	if err := os.Chmod(paths.RuntimeKernelSocketDir, 0o755); err != nil {
		return "", fmt.Errorf("make runtime kernel socket directory sandbox-accessible: %w", err)
	}
	if err := ensureEmptyFile(paths.NodeSettingsFile); err != nil {
		return "", err
	}
	return ensureIdentity(paths.NodeSettingsFile)
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read instance identity: %w", err)
	}
	var document map[string]any
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := toml.Unmarshal(data, &document); err != nil {
			return "", fmt.Errorf("decode kernel.toml: %w", err)
		}
	}
	if document == nil {
		document = map[string]any{}
	}
	if node, ok := document["node"].(map[string]any); ok {
		if id, ok := node["id"].(string); ok && id != "" {
			return parseIdentity(id)
		}
	}
	uuid, err := newUUID()
	if err != nil {
		return "", err
	}
	node, _ := document["node"].(map[string]any)
	if node == nil {
		node = map[string]any{}
	}
	node["id"] = uuid
	document["node"] = node
	encoded, err := toml.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode kernel.toml identity: %w", err)
	}
	if err := writePrivateFile(path, encoded); err != nil {
		return "", err
	}
	return uuid, nil
}

func parseIdentity(uuid string) (string, error) {
	if len(uuid) != 36 {
		return "", errors.New("invalid node.id in kernel.toml")
	}
	for index, character := range uuid {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return "", errors.New("invalid node.id in kernel.toml")
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return "", errors.New("invalid node.id in kernel.toml")
		}
	}
	return uuid, nil
}

func writePrivateFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".kernel-*.toml")
	if err != nil {
		return err
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
		return fmt.Errorf("write kernel.toml: %w", err)
	}
	return nil
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
