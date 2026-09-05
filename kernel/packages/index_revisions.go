package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"the8020/kernel/database"
)

// IndexRevisionFollower consumes the generic index invalidation contract in
// system revisions. It knows package selection, never application tables or
// what configuration a package's providers read.
type IndexRevisionFollower struct {
	database database.Store
	mu       sync.Mutex
	revision uint64
	pending  uint64
}

func NewIndexRevisionFollower(ctx context.Context, store database.Store) (*IndexRevisionFollower, error) {
	follower := &IndexRevisionFollower{database: store}
	revision, err := follower.current(ctx)
	if err != nil {
		return nil, err
	}
	follower.revision = revision
	return follower, nil
}

func (f *IndexRevisionFollower) current(ctx context.Context) (uint64, error) {
	var revision int64
	err := f.database.QueryRowContext(ctx, `SELECT "revision" FROM "the8020__system__revisions" WHERE "domain" = 'indexes'`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if revision < 0 {
		return 0, errors.New("index revision cannot be negative")
	}
	return uint64(revision), nil
}

func (f *IndexRevisionFollower) Poll(ctx context.Context) (PackageSetUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	revision, err := f.current(ctx)
	if err != nil {
		return PackageSetUpdate{}, err
	}
	if revision < f.revision {
		return PackageSetUpdate{}, errors.New("index revision moved backwards")
	}
	if revision == f.revision {
		return PackageSetUpdate{}, nil
	}
	rows, err := f.database.QueryContext(ctx, `SELECT "domain" FROM "the8020__system__revisions"
		WHERE "domain" LIKE 'index:%' AND "revision" > $1 AND "revision" <= $2 ORDER BY "domain"`, int64(f.revision), int64(revision))
	if err != nil {
		return PackageSetUpdate{}, err
	}
	defer rows.Close()
	update := PackageSetUpdate{Revision: revision}
	for rows.Next() {
		var domain string
		if err := rows.Scan(&domain); err != nil {
			return PackageSetUpdate{}, err
		}
		id := strings.TrimPrefix(domain, "index:")
		if _, err := ParsePackageID(id); err != nil {
			return PackageSetUpdate{}, fmt.Errorf("invalid package-index marker: %w", err)
		}
		update.Packages = append(update.Packages, id)
	}
	if err := rows.Err(); err != nil {
		return PackageSetUpdate{}, err
	}
	f.pending = revision
	return update, nil
}

func (f *IndexRevisionFollower) Acknowledge(revision uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if revision == 0 || revision != f.pending {
		return fmt.Errorf("index revision %d is not pending", revision)
	}
	f.revision, f.pending = revision, 0
	return nil
}
