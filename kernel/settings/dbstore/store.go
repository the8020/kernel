package dbstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/settings"
)

const (
	settingsTable  = `"the8020__system__settings"`
	revisionsTable = `"the8020__system__revisions"`
	settingsDomain = "settings"
)

type Store struct{ database database.Store }

func New(store database.Store) (*Store, error) {
	if store == nil {
		return nil, errors.New("database is required")
	}
	return &Store{database: store}, nil
}

func (s *Store) Revision(ctx context.Context) (uint64, error) {
	var revision int64
	if err := s.database.QueryRowContext(ctx, `SELECT "revision" FROM `+revisionsTable+` WHERE "domain" = $1`, settingsDomain).Scan(&revision); err != nil {
		return 0, err
	}
	if revision < 0 {
		return 0, errors.New("settings revision cannot be negative")
	}
	return uint64(revision), nil
}

func (s *Store) Load(ctx context.Context, definitions []settings.Definition) (map[string]any, uint64, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	now := database.EncodeTime(s.database, time.Now())
	statement := `INSERT INTO ` + settingsTable + ` ("key", "value", "definitionHash", "updatedAt") VALUES ($1, $2, $3, $4)
		ON CONFLICT ("key") DO UPDATE SET "definitionHash" = excluded."definitionHash"`
	byKey := make(map[string]settings.Definition, len(definitions))
	for _, definition := range definitions {
		encoded, err := database.EncodeJSON(s.database.Backend(), definition.Default)
		if err != nil {
			return nil, 0, fmt.Errorf("encode default for %s: %w", definition.Key, err)
		}
		if _, err := tx.ExecContext(ctx, statement, definition.Key, encoded, definitionHash(definition), now); err != nil {
			return nil, 0, fmt.Errorf("initialize global setting %s: %w", definition.Key, err)
		}
		byKey[definition.Key] = definition
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+revisionsTable+` ("domain", "revision", "updatedAt") VALUES ($1, 0, $2) ON CONFLICT ("domain") DO NOTHING`, settingsDomain, now); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	rows, err := s.database.QueryContext(ctx, `SELECT "key", "value" FROM `+settingsTable+` ORDER BY "key"`)
	if err != nil {
		return nil, 0, err
	}
	values := make(map[string]any, len(definitions))
	for rows.Next() {
		var key string
		var raw any
		if err := rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return nil, 0, err
		}
		definition, wanted := byKey[key]
		if !wanted {
			continue
		}
		value, err := decodeValue(definition, raw)
		if err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("decode global setting %s: %w", key, err)
		}
		values[key] = value
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	var revision int64
	if err := s.database.QueryRowContext(ctx, `SELECT "revision" FROM `+revisionsTable+` WHERE "domain" = $1`, settingsDomain).Scan(&revision); err != nil {
		return nil, 0, err
	}
	if revision < 0 {
		return nil, 0, errors.New("settings revision cannot be negative")
	}
	return values, uint64(revision), nil
}

func (s *Store) Set(ctx context.Context, definition settings.Definition, value any) (uint64, error) {
	encoded, err := database.EncodeJSON(s.database.Backend(), value)
	if err != nil {
		return 0, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := database.EncodeTime(s.database, time.Now())
	statement := `INSERT INTO ` + settingsTable + ` ("key", "value", "definitionHash", "updatedAt") VALUES ($1, $2, $3, $4)
		ON CONFLICT ("key") DO UPDATE SET "value" = excluded."value", "definitionHash" = excluded."definitionHash", "updatedAt" = excluded."updatedAt"`
	if _, err := tx.ExecContext(ctx, statement, definition.Key, encoded, definitionHash(definition), now); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+revisionsTable+` ("domain", "revision", "updatedAt") VALUES ($1, 1, $2)
		ON CONFLICT ("domain") DO UPDATE SET "revision" = `+revisionsTable+`."revision" + 1, "updatedAt" = excluded."updatedAt"`, settingsDomain, now); err != nil {
		return 0, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT "revision" FROM `+revisionsTable+` WHERE "domain" = $1`, settingsDomain).Scan(&revision); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint64(revision), nil
}

func definitionHash(definition settings.Definition) string {
	encoded, _ := json.Marshal(definition)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func decodeValue(definition settings.Definition, raw any) (any, error) {
	var data []byte
	switch value := raw.(type) {
	case string:
		data = []byte(value)
	case []byte:
		data = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		data = encoded
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	switch definition.Type {
	case settings.TypeInteger:
		number, ok := value.(json.Number)
		if !ok {
			return nil, errors.New("stored value is not an integer")
		}
		return strconv.ParseInt(string(number), 10, 64)
	case settings.TypeByteSize:
		number, ok := value.(json.Number)
		if !ok {
			return nil, errors.New("stored value is not a byte size")
		}
		integer, err := strconv.ParseInt(string(number), 10, 64)
		return settings.ByteSize(integer), err
	case settings.TypeBoolean:
		boolean, ok := value.(bool)
		if !ok {
			return nil, errors.New("stored value is not a boolean")
		}
		return boolean, nil
	case settings.TypeString, settings.TypeEnum:
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("stored value is not a string")
		}
		return text, nil
	default:
		return nil, fmt.Errorf("unsupported setting type %q", definition.Type)
	}
}
