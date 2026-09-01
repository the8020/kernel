package development

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	recordLimit        = 16 << 20
	commandOutputLimit = 1 << 20
)

func readTOML(path string, output any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > recordLimit {
		return fmt.Errorf("development record %s exceeds size limit", path)
	}
	if err := toml.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func writeTOML(path string, value any, mode fs.FileMode) error {
	data, err := toml.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, data, mode)
}

func readJSON(path string, output any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > recordLimit {
		return fmt.Errorf("development record %s exceeds size limit", path)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".development-record-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func validRelative(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

type boundedBuffer struct {
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := b.limit - b.data.Len()
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		_, _ = b.data.Write(value[:remaining])
	}
	if remaining < len(value) {
		b.truncated = true
	}
	return written, nil
}

func (b *boundedBuffer) String() string {
	return strings.TrimSpace(b.RawString())
}

func (b *boundedBuffer) RawString() string {
	value := b.data.String()
	if b.truncated {
		value += "\n[output truncated]"
	}
	return value
}

func commandOutput(ctx context.Context, name string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	output := &boundedBuffer{limit: commandOutputLimit}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	return output.String(), err
}

func copyDirectory(ctx context.Context, source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("copy destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".workspace-copy-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	output, err := commandOutput(ctx, "cp", "-a", "--reflink=auto", filepath.Clean(source)+string(filepath.Separator)+".", stage)
	if err != nil {
		return fmt.Errorf("copy durable development storage: %w: %s", err, output)
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return nil
}

// copySystemRoot initializes an OCI root without preserving the immutable
// template modes on proc and sys. Those empty directories are mount points at
// runtime, and some shared filesystems reject changing them to 0555 during an
// archive copy.
func copySystemRoot(ctx context.Context, source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("copy destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".system-copy-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "proc" || entry.Name() == "sys" {
			if !entry.IsDir() {
				return fmt.Errorf("development image %s mount point is not a directory", entry.Name())
			}
			children, readErr := os.ReadDir(filepath.Join(source, entry.Name()))
			if readErr != nil {
				return readErr
			}
			if len(children) != 0 {
				return fmt.Errorf("development image %s mount point is not empty", entry.Name())
			}
			if err := os.Mkdir(filepath.Join(stage, entry.Name()), 0o755); err != nil {
				return err
			}
			continue
		}
		output, copyErr := commandOutput(ctx, "cp", "-a", "--reflink=auto", filepath.Join(source, entry.Name()), stage)
		if copyErr != nil {
			return fmt.Errorf("copy durable development system entry %s: %w: %s", entry.Name(), copyErr, output)
		}
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return nil
}

// replaceWorktree copies ordinary working-tree entries while retaining the
// destination repository's private .git directory.
func replaceWorktree(ctx context.Context, source, destination string) error {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		output, copyErr := commandOutput(ctx, "cp", "-a", "--reflink=auto", filepath.Join(source, entry.Name()), destination)
		if copyErr != nil {
			return fmt.Errorf("copy development package entry %s: %w: %s", entry.Name(), copyErr, output)
		}
	}
	return nil
}

type basesDocument struct {
	Schema   int                     `toml:"schema"`
	Packages map[string]baseDocument `toml:"packages"`
}

type baseDocument struct {
	BaseCommit string `toml:"base_commit"`
	Conflicted bool   `toml:"conflicted,omitempty"`
}

func readBases(path string) (basesDocument, error) {
	document := basesDocument{}
	if err := readTOML(path, &document); err != nil {
		return basesDocument{}, err
	}
	if document.Schema != 1 || document.Packages == nil {
		return basesDocument{}, errors.New("invalid development source base record")
	}
	return document, nil
}

func writeBases(path string, document basesDocument) error {
	if document.Packages == nil {
		document.Packages = map[string]baseDocument{}
	}
	document.Schema = 1
	return writeTOML(path, document, 0o600)
}

func sortedBaseIDs(document basesDocument) []string {
	ids := make([]string, 0, len(document.Packages))
	for id := range document.Packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
