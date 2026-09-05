package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"the8020/kernel/deployment"
)

// HandlerDefinition selects an ordinary program by its full package identity.
type HandlerDefinition struct {
	Description string `toml:"description"`
	ProgramID   string `toml:"program"`
}

// HookDefinition keeps declaration order and its resolved ordinary program.
type HookDefinition struct {
	HandlerDefinition
	ID      string
	Order   int
	Program ProgramDefinition
}

func readHandler(root, path, kind string) (HandlerDefinition, string, int, error) {
	var handler HandlerDefinition
	if err := requireRealPath(root, path, false); err != nil {
		return handler, "", 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return handler, "", 0, err
	}
	if info.Size() > manifestLimit {
		return handler, "", 0, errors.New("handler declaration exceeds manifest size limit")
	}
	var trigger string
	var order int
	if kind == "events" {
		var declaration struct {
			HandlerDefinition
			Event string `toml:"event"`
		}
		if err := decodeTOMLFile(path, &declaration); err != nil {
			return handler, "", 0, err
		}
		handler, trigger = declaration.HandlerDefinition, declaration.Event
		if len(trigger) > 128 || ValidateName(trigger) != nil {
			return handler, "", 0, errors.New("handler event must be a valid event name")
		}
	} else {
		var declaration struct {
			HandlerDefinition
			Hook  string `toml:"hook"`
			Order int    `toml:"order"`
		}
		if err := decodeTOMLFile(path, &declaration); err != nil {
			return handler, "", 0, err
		}
		handler, trigger = declaration.HandlerDefinition, declaration.Hook
		order = declaration.Order
		if trigger != "pre-activate" && trigger != "post-activate" && trigger != "index-services" {
			return handler, "", 0, errors.New("handler hook must be pre-activate, post-activate, or index-services")
		}
	}
	if strings.TrimSpace(handler.Description) == "" {
		return handler, "", 0, errors.New("handler description is required")
	}
	if _, _, err := ParseProgramID(handler.ProgramID); err != nil {
		return handler, "", 0, fmt.Errorf("handler program: %w", err)
	}
	return handler, trigger, order, nil
}

// DeclarationFiles lists an optional flat TOML directory beneath a package root.
// Filenames are opaque; symlinks, nested directories, and other files are invalid.
func DeclarationFiles(root, kind string) ([]string, error) {
	folder := filepath.Join(root, kind)
	if _, err := os.Lstat(folder); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := requireRealPath(root, folder, true); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".toml") {
			return nil, fmt.Errorf("%s must contain only flat TOML declarations: %s", kind, file.Name())
		}
		path := filepath.Join(folder, file.Name())
		if err := requireRealPath(root, path, false); err != nil {
			return nil, err
		}
		paths = append(paths, path)
		if len(paths) > 2048 {
			return nil, fmt.Errorf("%s exceeds 2048 declarations", kind)
		}
	}
	return paths, nil
}

// HookHandlers indexes each declaration by its hook field, never its filename.
func HookHandlers(root string) (map[string][]HookDefinition, error) {
	files, err := DeclarationFiles(root, "hooks")
	if err != nil {
		return nil, err
	}
	result := map[string][]HookDefinition{}
	for _, path := range files {
		handler, hook, order, err := readHandler(root, path, "hooks")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		result[hook] = append(result[hook], HookDefinition{
			HandlerDefinition: handler, ID: "hooks/" + filepath.Base(path), Order: order,
		})
	}
	for _, handlers := range result {
		sortHooks(handlers)
	}
	return result, nil
}

func sortHooks(handlers []HookDefinition) {
	slices.SortFunc(handlers, func(a, b HookDefinition) int {
		if a.Order < b.Order {
			return -1
		}
		if a.Order > b.Order {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// ValidateHandlers checks candidate declarations and resolves their programs
// against the complete candidate set, then the ready installed package set.
func (s *Store) ValidateHandlers(ctx context.Context, candidates []deployment.Candidate) error {
	_, err := s.indexCandidateHandlers(ctx, candidates)
	return err
}
