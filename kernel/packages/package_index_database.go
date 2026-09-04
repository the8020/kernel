package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"the8020/kernel/database"
)

const packagesTable = `"the8020__packages__packages"`

// PackageIndexStore owns durable desired and active package identity. Package
// checkout paths are deliberately derived from the node-local packages root.
type PackageIndexStore interface {
	List(context.Context) ([]PackageIndex, error)
	Get(context.Context, string) (PackageIndex, bool, error)
	Put(context.Context, PackageIndex) error
	SetActivation(context.Context, string, string, string, error) error
	Revision(context.Context) (uint64, error)
}

type DatabasePackageIndexStore struct{ database database.Store }

func NewDatabasePackageIndexStore(store database.Store) (*DatabasePackageIndexStore, error) {
	if store == nil {
		return nil, errors.New("database is required")
	}
	return &DatabasePackageIndexStore{database: store}, nil
}

// Revision is the cheap cross-node change detector for the active package
// set. Package rows are loaded only after this value advances.
func (s *DatabasePackageIndexStore) Revision(ctx context.Context) (uint64, error) {
	var revision int64
	err := s.database.QueryRowContext(ctx, `SELECT "revision" FROM "the8020__system__revisions" WHERE "domain" = 'packages'`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if revision < 0 {
		return 0, errors.New("package-set revision cannot be negative")
	}
	return uint64(revision), nil
}

func (s *DatabasePackageIndexStore) List(ctx context.Context) ([]PackageIndex, error) {
	rows, err := s.database.QueryContext(ctx, `SELECT "packageId", "author", "repository", "source",
		"requestedCommit", "requestedTag", "secretName", "local", "activeCommit", "state", "error", "revision"
		FROM `+packagesTable+` WHERE "state" <> 'retired' ORDER BY "packageId"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PackageIndex{}
	for rows.Next() {
		entry, err := scanPackageIndex(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (s *DatabasePackageIndexStore) Get(ctx context.Context, packageID string) (PackageIndex, bool, error) {
	if _, err := ParsePackageID(packageID); err != nil {
		return PackageIndex{}, false, err
	}
	entry, err := scanPackageIndex(s.database.QueryRowContext(ctx, `SELECT "packageId", "author", "repository", "source",
		"requestedCommit", "requestedTag", "secretName", "local", "activeCommit", "state", "error", "revision"
		FROM `+packagesTable+` WHERE "packageId" = $1 AND "state" <> 'retired'`, packageID))
	if errors.Is(err, sql.ErrNoRows) {
		return PackageIndex{}, false, nil
	}
	return entry, err == nil, err
}

type packageIndexRow interface{ Scan(...any) error }

func scanPackageIndex(row packageIndexRow) (PackageIndex, error) {
	var entry PackageIndex
	var source, commit, tag, secret, activeCommit, failure sql.NullString
	var revision int64
	if err := row.Scan(&entry.PackageID, &entry.Author, &entry.Repository, &source, &commit, &tag, &secret,
		&entry.Local, &activeCommit, &entry.State, &failure, &revision); err != nil {
		return PackageIndex{}, err
	}
	if revision < 0 {
		return PackageIndex{}, errors.New("package revision cannot be negative")
	}
	entry.Source, entry.Commit, entry.Tag, entry.Secret = source.String, commit.String, tag.String, secret.String
	entry.ActiveCommit, entry.Error, entry.Revision = activeCommit.String, failure.String, uint64(revision)
	entry.Valid = true
	return entry, nil
}

func (s *DatabasePackageIndexStore) Put(ctx context.Context, entry PackageIndex) error {
	now := database.EncodeTime(s.database, time.Now())
	_, err := s.database.ExecContext(ctx, `INSERT INTO `+packagesTable+` (
		"packageId", "author", "repository", "source", "requestedCommit", "requestedTag", "secretName",
		"local", "state", "revision", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'desired', 0, $9, $9)
		ON CONFLICT ("packageId") DO UPDATE SET "author" = excluded."author",
		"repository" = excluded."repository", "source" = excluded."source",
		"requestedCommit" = excluded."requestedCommit", "requestedTag" = excluded."requestedTag",
		"secretName" = excluded."secretName", "local" = excluded."local",
		"state" = CASE WHEN `+packagesTable+`."activeCommit" IS NULL THEN 'desired' ELSE `+packagesTable+`."state" END,
		"revision" = `+packagesTable+`."revision" + 1, "updatedAt" = excluded."updatedAt"`,
		entry.PackageID, entry.Author, entry.Repository, nullableText(entry.Source), nullableText(entry.Commit),
		nullableText(entry.Tag), nullableText(entry.Secret), entry.Local, now)
	return err
}

func (s *DatabasePackageIndexStore) SetActivation(ctx context.Context, packageID, state, commit string, failure error) error {
	if _, err := ParsePackageID(packageID); err != nil {
		return err
	}
	if state != "desired" && state != "activating" && state != "ready" && state != "failed" && state != "retired" {
		return fmt.Errorf("invalid package state %q", state)
	}
	message := any(nil)
	if failure != nil {
		message = failure.Error()
	}
	activeCommit := any(nil)
	if commit != "" {
		activeCommit = commit
	}
	_, err := s.database.ExecContext(ctx, `UPDATE `+packagesTable+` SET "state" = $1,
		"activeCommit" = COALESCE($2, "activeCommit"), "error" = $3,
		"revision" = "revision" + 1, "updatedAt" = $4 WHERE "packageId" = $5`,
		state, activeCommit, message, database.EncodeTime(s.database, time.Now()), packageID)
	return err
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
