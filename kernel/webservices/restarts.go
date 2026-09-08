package webservices

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/packages"
)

// RestartRevision is generic lifecycle intent, independent of package commits
// and effective configuration. The shared index revision delivers it to nodes.
type RestartRevision struct {
	Revision uint64
	Hard     uint64
}

const revisionsTable = `"the8020__system__revisions"`

// Restart publishes one cluster-wide restart. A positive updateRevision is a
// monotonic caller-supplied update identity; repeated/older updates are no-ops.
// Zero requests a new manual restart. Only soft updates may be deduplicated.
func (m *Manager) Restart(ctx context.Context, serviceID, mode string, updateRevision uint64) (Status, error) {
	if err := m.RequestRestart(ctx, serviceID, mode, updateRevision); err != nil {
		return Status{}, err
	}
	return m.Reconcile(ctx, serviceID)
}

// RequestRestart publishes intent before a package reindex so configuration
// and source changes enter one replacement generation.
func (m *Manager) RequestRestart(ctx context.Context, serviceID, mode string, updateRevision uint64) error {
	if _, err := m.index.ReadService(serviceID); err != nil {
		return err
	}
	if mode != "soft" && mode != "hard" || updateRevision > 0 && mode != "soft" {
		return errors.New("restart mode must be soft or hard; update revisions require soft restart")
	}
	if m.database == nil {
		return errors.New("shared restart revisions are unavailable")
	}
	if err := m.publishRestart(ctx, serviceID, mode, updateRevision); err != nil {
		return err
	}
	return m.readRestart(ctx, serviceID)
}

func (m *Manager) publishRestart(ctx context.Context, serviceID, mode string, updateRevision uint64) error {
	tx, err := m.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := database.EncodeTime(m.database, time.Now())
	// The existing scalar is also the short transaction lock used by Deno
	// configuration writers on both SQLite and PostgreSQL.
	var revision uint64
	err = tx.QueryRowContext(ctx, `INSERT INTO `+revisionsTable+` ("domain", "revision", "updatedAt")
		VALUES ('indexes', 0, $1) ON CONFLICT ("domain") DO UPDATE
		SET "revision" = `+revisionsTable+`."revision" RETURNING "revision"`, now).Scan(&revision)
	if err != nil {
		return err
	}
	if updateRevision > 0 {
		var previous uint64
		err := tx.QueryRowContext(ctx, `SELECT "revision" FROM `+revisionsTable+` WHERE "domain" = $1`, "restart-update:"+serviceID).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if previous >= updateRevision {
			return nil
		}
	}
	revision++
	markers := map[string]uint64{"indexes": revision, "restart:" + serviceID: revision}
	if mode == "hard" {
		markers["restart-hard:"+serviceID] = revision
	}
	if updateRevision > 0 {
		markers["restart-update:"+serviceID] = updateRevision
	}
	for domain, value := range markers {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+revisionsTable+` ("domain", "revision", "updatedAt") VALUES ($1, $2, $3)
			ON CONFLICT ("domain") DO UPDATE SET "revision" = excluded."revision", "updatedAt" = excluded."updatedAt"`, domain, value, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *Manager) loadRestarts(ctx context.Context) error {
	rows, err := m.database.QueryContext(ctx, `SELECT "domain", "revision" FROM `+revisionsTable+` WHERE "domain" LIKE 'restart:%' OR "domain" LIKE 'restart-hard:%'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	restarts := map[string]RestartRevision{}
	for rows.Next() {
		var domain string
		var value uint64
		if err := rows.Scan(&domain, &value); err != nil {
			return err
		}
		_, id, _ := strings.Cut(domain, ":")
		if _, err := packages.ParseServiceID(id); err != nil {
			return err
		}
		restart := restarts[id]
		if strings.HasPrefix(domain, "restart-hard:") {
			restart.Hard = value
		} else {
			restart.Revision = value
		}
		restarts[id] = restart
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, restart := range restarts {
		m.index.setRestart(id, restart)
	}
	return nil
}

// RefreshRestart applies the latest published intent on this node. Revisions
// coalesce, but the last hard revision remains until every node has applied it.
func (m *Manager) RefreshRestart(ctx context.Context, serviceID string) (Status, error) {
	if err := m.readRestart(ctx, serviceID); err != nil {
		return Status{}, err
	}
	return m.reconcileService(ctx, serviceID, false)
}

func (m *Manager) readRestart(ctx context.Context, serviceID string) error {
	if _, err := packages.ParseServiceID(serviceID); err != nil {
		return err
	}
	var restart RestartRevision
	err := m.database.QueryRowContext(ctx, `SELECT r."revision", COALESCE(h."revision", 0)
		FROM `+revisionsTable+` r LEFT JOIN `+revisionsTable+` h ON h."domain" = $2 WHERE r."domain" = $1`,
		"restart:"+serviceID, "restart-hard:"+serviceID).Scan(&restart.Revision, &restart.Hard)
	if err != nil {
		return err
	}
	m.index.setRestart(serviceID, restart)
	return nil
}

// terminateGenerations runs under the selected service's capacity lock. It
// removes routing first and keeps failed cleanup retryable on ordinary reconcile.
func (m *Manager) terminateGenerations(ctx context.Context, serviceID string, hardRevision uint64) error {
	records, err := m.pools.ListForService(serviceID)
	if err != nil {
		return err
	}
	older := map[string]bool{}
	for _, record := range records {
		if record.RestartRevision < hardRevision {
			older[record.ServiceID] = true
		}
	}
	if len(older) == 0 {
		return nil
	}
	m.mu.Lock()
	if runtime := m.services[serviceID]; runtime != nil {
		runtime.sandboxes = slices.DeleteFunc(runtime.sandboxes, func(sandbox *runtimeSandbox) bool { return older[sandbox.status.PoolID] })
		runtime.retired = slices.DeleteFunc(runtime.retired, func(sandbox *runtimeSandbox) bool { return older[sandbox.status.PoolID] })
		runtime.status.State = StateRestarting
		runtime.status.LoadedVersion = 0
		runtime.rejected = false
	}
	m.mu.Unlock()
	var failures error
	for _, record := range records {
		if !older[record.ServiceID] {
			continue
		}
		if len(record.WorkerIDs) > 0 {
			m.restartDemand.Store(serviceID, true)
		}
		stopped, err := m.pools.Kill(ctx, record.ServiceID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = errors.Join(failures, fmt.Errorf("kill pool %s: %w", record.ServiceID, err))
			continue
		}
		if stopped || errors.Is(err, os.ErrNotExist) {
			err = m.pools.RemoveStopped(record.ServiceID)
			if !errors.Is(err, os.ErrNotExist) {
				failures = errors.Join(failures, err)
			}
		} else {
			failures = errors.Join(failures, fmt.Errorf("pool %s has not terminated", record.ServiceID))
		}
	}
	return failures
}
