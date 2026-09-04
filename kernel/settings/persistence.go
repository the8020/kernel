package settings

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func persist(path string, definitions map[string]Definition, sources map[string]sourceValues, storage Storage) error {
	values := make(map[string]any)
	for key, source := range sources {
		if source.hasPersisted && definitions[key].Storage == storage {
			values[key] = source.persisted
		}
	}
	return persistValues(path, definitions, values, storage)
}

func persistValues(path string, definitions map[string]Definition, values map[string]any, storage Storage) error {
	sections := map[string][]string{}
	for key := range values {
		if definitions[key].Storage != storage {
			continue
		}
		parts := strings.Split(key, ".")
		section := strings.Join(parts[:len(parts)-1], ".")
		sections[section] = append(sections[section], key)
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".settings-*.toml")
	if err != nil {
		return fmt.Errorf("create temporary settings file: %w", err)
	}
	temporaryName := temporary.Name()
	cleanup := func() { _ = temporary.Close(); _ = os.Remove(temporaryName) }
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("restrict temporary settings file: %w", err)
	}
	writer := bufio.NewWriter(temporary)
	sectionNames := make([]string, 0, len(sections))
	for section := range sections {
		sectionNames = append(sectionNames, section)
	}
	sort.Strings(sectionNames)
	for index, section := range sectionNames {
		if index > 0 {
			_, _ = writer.WriteString("\n")
		}
		if _, err := fmt.Fprintf(writer, "[%s]\n", section); err != nil {
			cleanup()
			return fmt.Errorf("write settings: %w", err)
		}
		keys := sections[section]
		sort.Strings(keys)
		for _, key := range keys {
			name := key[strings.LastIndex(key, ".")+1:]
			if _, err := fmt.Fprintf(writer, "%s = %s\n", name, tomlValue(definitions[key], values[key])); err != nil {
				cleanup()
				return fmt.Errorf("write settings: %w", err)
			}
		}
	}
	if err := writer.Flush(); err != nil {
		cleanup()
		return fmt.Errorf("flush settings: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync settings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("close settings: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("replace settings atomically: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict settings file: %w", err)
	}
	dir, err := os.Open(directory)
	if err == nil {
		syncErr := dir.Sync()
		closeErr := dir.Close()
		if syncErr != nil {
			return fmt.Errorf("sync settings directory: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close settings directory: %w", closeErr)
		}
	}
	return nil
}

func tomlValue(definition Definition, value any) string {
	switch definition.Type {
	case TypeBoolean:
		return strconv.FormatBool(value.(bool))
	case TypeInteger:
		return strconv.FormatInt(value.(int64), 10)
	case TypeByteSize:
		return strconv.Quote(formatByteSize(value.(ByteSize)))
	default:
		return strconv.Quote(value.(string))
	}
}
