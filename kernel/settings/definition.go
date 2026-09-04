// Package settings owns definitions, precedence, persistence, and runtime application.
package settings

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	TypeString   = "string"
	TypeInteger  = "integer"
	TypeBoolean  = "boolean"
	TypeEnum     = "enum"
	TypeByteSize = "byte_size"

	environmentPrefix = "THE8020_"
)

// Storage identifies the persisted override store owned by a setting.
type Storage string

const (
	StorageNode   Storage = "node"
	StorageGlobal Storage = "global"
)

// ByteSize is a validated count of bytes.
type ByteSize int64

// Definition is generated from one modular setting TOML file.
type Definition struct {
	Key             string   `toml:"key"`
	Type            string   `toml:"type"`
	Storage         Storage  `toml:"storage"`
	Default         any      `toml:"default"`
	Environment     string   `toml:"environment"`
	Minimum         *int64   `toml:"minimum,omitempty"`
	Maximum         *int64   `toml:"maximum,omitempty"`
	Allowed         []string `toml:"allowed,omitempty"`
	Pattern         string   `toml:"pattern,omitempty"`
	RuntimeMutable  bool     `toml:"runtime_mutable"`
	RestartRequired bool     `toml:"restart_required"`
	Description     string   `toml:"description"`
}

// ValidateDefinition checks one generated definition and canonicalizes its default.
func ValidateDefinition(definition Definition) (Definition, error) {
	if !validKey(definition.Key) {
		return definition, fmt.Errorf("invalid setting key %q", definition.Key)
	}
	if !strings.HasPrefix(definition.Environment, environmentPrefix) || !validEnvironment(definition.Environment) {
		return definition, fmt.Errorf("invalid environment variable for %s", definition.Key)
	}
	if definition.Storage != StorageNode && definition.Storage != StorageGlobal {
		return definition, fmt.Errorf("setting %s storage must be node or global", definition.Key)
	}
	if strings.TrimSpace(definition.Description) == "" {
		return definition, fmt.Errorf("setting %s has no description", definition.Key)
	}
	if definition.RestartRequired && definition.RuntimeMutable {
		return definition, fmt.Errorf("setting %s cannot be runtime mutable and restart required", definition.Key)
	}
	if definition.Minimum != nil && definition.Maximum != nil && *definition.Minimum > *definition.Maximum {
		return definition, fmt.Errorf("setting %s minimum exceeds maximum", definition.Key)
	}
	if definition.Type == TypeEnum && len(definition.Allowed) == 0 {
		return definition, fmt.Errorf("enum setting %s has no allowed values", definition.Key)
	}
	if definition.Pattern != "" {
		if definition.Type != TypeString {
			return definition, fmt.Errorf("setting %s pattern requires string type", definition.Key)
		}
		if _, err := regexp.Compile(definition.Pattern); err != nil {
			return definition, fmt.Errorf("setting %s has invalid pattern: %w", definition.Key, err)
		}
	}
	defaultValue, err := normalizeValue(definition, definition.Default)
	if err != nil {
		return definition, fmt.Errorf("invalid default for %s: %w", definition.Key, err)
	}
	definition.Default = defaultValue
	return definition, nil
}

func validKey(key string) bool {
	parts := strings.Split(key, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for i, r := range part {
			if !((r >= 'a' && r <= 'z') || r == '_' || (i > 0 && r >= '0' && r <= '9')) {
				return false
			}
		}
	}
	return true
}

func validEnvironment(name string) bool {
	for i, r := range name {
		if !((r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func parse(definition Definition, raw string) (any, error) {
	switch definition.Type {
	case TypeString, TypeEnum:
		return normalizeValue(definition, raw)
	case TypeInteger:
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, errors.New("must be an integer")
		}
		return normalizeValue(definition, value)
	case TypeBoolean:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, errors.New("must be true or false")
		}
		return normalizeValue(definition, value)
	case TypeByteSize:
		value, err := parseByteSize(raw)
		if err != nil {
			return nil, err
		}
		return normalizeValue(definition, value)
	default:
		return nil, fmt.Errorf("unsupported setting type %q", definition.Type)
	}
}

func normalizeValue(definition Definition, raw any) (any, error) {
	switch definition.Type {
	case TypeString:
		value, ok := raw.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		if definition.Pattern != "" {
			matched, err := regexp.MatchString(definition.Pattern, value)
			if err != nil {
				return nil, fmt.Errorf("invalid configured pattern: %w", err)
			}
			if !matched {
				return nil, errors.New("does not match the required format")
			}
		}
		return value, nil
	case TypeInteger:
		value, ok := integerValue(raw)
		if !ok {
			return nil, errors.New("must be an integer")
		}
		if definition.Minimum != nil && value < *definition.Minimum {
			return nil, fmt.Errorf("must be at least %d", *definition.Minimum)
		}
		if definition.Maximum != nil && value > *definition.Maximum {
			return nil, fmt.Errorf("must be at most %d", *definition.Maximum)
		}
		return value, nil
	case TypeBoolean:
		value, ok := raw.(bool)
		if !ok {
			return nil, errors.New("must be a boolean")
		}
		return value, nil
	case TypeEnum:
		value, ok := raw.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		for _, allowed := range definition.Allowed {
			if value == allowed {
				return value, nil
			}
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(definition.Allowed, ", "))
	case TypeByteSize:
		var value ByteSize
		switch typed := raw.(type) {
		case ByteSize:
			value = typed
		case string:
			parsed, err := parseByteSize(typed)
			if err != nil {
				return nil, err
			}
			value = parsed
		default:
			integer, ok := integerValue(raw)
			if !ok {
				return nil, errors.New("must be a byte size")
			}
			value = ByteSize(integer)
		}
		if value < 0 || value == 0 && (definition.Minimum == nil || *definition.Minimum > 0) {
			return nil, errors.New("must be greater than zero")
		}
		if definition.Minimum != nil && int64(value) < *definition.Minimum {
			return nil, fmt.Errorf("must be at least %s", formatByteSize(ByteSize(*definition.Minimum)))
		}
		if definition.Maximum != nil && int64(value) > *definition.Maximum {
			return nil, fmt.Errorf("must be at most %s", formatByteSize(ByteSize(*definition.Maximum)))
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported setting type %q", definition.Type)
	}
}

func integerValue(raw any) (int64, bool) {
	switch value := raw.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case int32:
		return int64(value), true
	case uint64:
		if value > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
}

func parseByteSize(raw string) (ByteSize, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	units := []struct {
		name       string
		multiplier int64
	}{{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}, {"B", 1}}
	for _, unit := range units {
		if strings.HasSuffix(value, unit.name) {
			number := strings.TrimSpace(strings.TrimSuffix(value, unit.name))
			parsed, err := strconv.ParseInt(number, 10, 64)
			if err != nil || parsed < 0 {
				return 0, errors.New("must be a non-negative byte size such as 0B, 1KB, 10MB, or 1GB")
			}
			if parsed > (1<<63-1)/unit.multiplier {
				return 0, errors.New("byte size is too large")
			}
			return ByteSize(parsed * unit.multiplier), nil
		}
	}
	return 0, errors.New("must include B, KB, MB, or GB")
}

func formatByteSize(value ByteSize) string {
	bytes := int64(value)
	for _, unit := range []struct {
		name       string
		multiplier int64
	}{{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}} {
		if bytes%unit.multiplier == 0 {
			return fmt.Sprintf("%d%s", bytes/unit.multiplier, unit.name)
		}
	}
	return fmt.Sprintf("%dB", bytes)
}

func externalValue(value any) any {
	if bytes, ok := value.(ByteSize); ok {
		return formatByteSize(bytes)
	}
	return value
}

func equal(a, b any) bool {
	ab, aok := a.(ByteSize)
	bb, bok := b.(ByteSize)
	if aok || bok {
		return aok && bok && ab == bb
	}
	return fmt.Sprint(a) == fmt.Sprint(b) && fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b)
}
