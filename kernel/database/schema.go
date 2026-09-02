package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const catalogVersion = 1
const postgresSchemaLock int64 = 802020260901

type deploymentLockContextKey struct{}

//go:embed catalog_sqlite.sql
var sqliteCatalogSQL string

//go:embed catalog_postgresql.sql
var postgresqlCatalogSQL string

type DefaultDescriptor struct {
	Kind  string `json:"kind"`
	Value any    `json:"value,omitempty"`
}

type ReferenceDescriptor struct {
	Table  string `json:"table"`
	Column string `json:"column"`
}

type ColumnDescriptor struct {
	Name        string               `json:"name"`
	LogicalType string               `json:"logical_type"`
	Precision   int                  `json:"precision,omitempty"`
	Scale       int                  `json:"scale,omitempty"`
	EnumValues  []string             `json:"enum_values,omitempty"`
	Nullable    bool                 `json:"nullable"`
	Default     *DefaultDescriptor   `json:"default,omitempty"`
	Generated   bool                 `json:"generated"`
	PrimaryKey  bool                 `json:"primary_key"`
	Unique      bool                 `json:"unique"`
	Reference   *ReferenceDescriptor `json:"reference,omitempty"`
}

type IndexDescriptor struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
}

type TableDescriptor struct {
	FormatVersion int                `json:"format_version"`
	TableID       string             `json:"table_id"`
	Columns       []ColumnDescriptor `json:"columns"`
	PrimaryKey    []string           `json:"primary_key"`
	Indexes       []IndexDescriptor  `json:"indexes"`
}

// EvaluatedTable is the trusted evaluator's serializable result.
type EvaluatedTable struct {
	Descriptor     TableDescriptor `json:"descriptor"`
	DescriptorJSON string          `json:"descriptor_json"`
	DescriptorHash string          `json:"descriptor_hash"`
	SourceModule   string          `json:"source_module"`
	SourcePackage  string          `json:"source_package"`
	SourceCommit   string          `json:"source_commit"`
	Dependencies   []string        `json:"dependencies"`
}

type SynchronizationResult struct {
	TableID string `json:"table_id"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

type DefinitionSet struct {
	Tables         []EvaluatedTable
	Packages       []string
	PackageCommits map[string]string
	PackageSetHash string
}

type CatalogState struct {
	PackageSetHash    string
	PackageCommits    map[string]string
	DescriptorSetHash string
}

type DeploymentCandidate struct {
	PackageID       string `json:"package_id"`
	PreviousCommit  string `json:"previous_commit,omitempty"`
	CandidateCommit string `json:"candidate_commit"`
}

type PendingDeployment struct {
	PreviousPackageSetHash  string
	PreviousPackageCommits  map[string]string
	CandidatePackageSetHash string
	CandidatePackageCommits map[string]string
	Candidates              []DeploymentCandidate
	Stage                   string
	Error                   string
	StartedAt               string
	UpdatedAt               string
}

type DefinitionEvaluator func(context.Context, []string) (DefinitionSet, error)
type FullSynchronizer func(context.Context, bool) ([]SynchronizationResult, error)
type SourceInspector func(context.Context, []TableSource) (map[string]SourceStatus, error)
type SourceEvaluator func(context.Context, TableSource) (*EvaluatedTable, error)

type SourceStatus struct {
	Exists        bool
	CurrentCommit string
	Error         string
}

func (m *Manager) SetDefinitionEvaluator(evaluator DefinitionEvaluator) {
	m.evaluatorMu.Lock()
	m.evaluator = evaluator
	m.evaluatorMu.Unlock()
}

func (m *Manager) SetFullSynchronizer(synchronizer FullSynchronizer) {
	m.evaluatorMu.Lock()
	m.fullSynchronizer = synchronizer
	m.evaluatorMu.Unlock()
}

func (m *Manager) SetSourceInspector(inspector SourceInspector) {
	m.evaluatorMu.Lock()
	m.sourceInspector = inspector
	m.evaluatorMu.Unlock()
}

func (m *Manager) SetSourceEvaluator(evaluator SourceEvaluator) {
	m.evaluatorMu.Lock()
	m.sourceEvaluator = evaluator
	m.evaluatorMu.Unlock()
}

func (m *Manager) EvaluateDefinitions(ctx context.Context, packages []string) (DefinitionSet, error) {
	m.evaluatorMu.RLock()
	evaluator := m.evaluator
	m.evaluatorMu.RUnlock()
	if evaluator == nil {
		return DefinitionSet{}, errors.New("database table evaluator is unavailable")
	}
	return evaluator(ctx, packages)
}

func (m *Manager) SynchronizeDefinitions(ctx context.Context, packages []string, full bool) ([]SynchronizationResult, error) {
	if full && len(packages) == 0 {
		m.evaluatorMu.RLock()
		synchronizer := m.fullSynchronizer
		m.evaluatorMu.RUnlock()
		if synchronizer != nil {
			return synchronizer(ctx, true)
		}
	}
	definitions, err := m.EvaluateDefinitions(ctx, packages)
	if err != nil {
		return nil, err
	}
	options := SynchronizationOptions{}
	if full {
		options.Full = true
		options.PackageCommits = definitions.PackageCommits
	} else {
		options.RetireMissingPackages = definitions.Packages
	}
	return m.Synchronize(ctx, definitions.Tables, options)
}

// BeginDeployment records the narrow crash-recovery boundary before schema changes.
func (m *Manager) BeginDeployment(ctx context.Context, candidates []DeploymentCandidate) (PendingDeployment, error) {
	if len(candidates) == 0 {
		return PendingDeployment{}, errors.New("database deployment candidates are required")
	}
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return PendingDeployment{}, err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return PendingDeployment{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM _8020_pending_deployment`).Scan(&count); err != nil {
		return PendingDeployment{}, err
	}
	if count != 0 {
		return PendingDeployment{}, errors.New("a database schema deployment is already pending recovery")
	}
	state, err := catalogState(ctx, tx)
	if err != nil {
		return PendingDeployment{}, err
	}
	if !m.Status().Initialized {
		return PendingDeployment{}, errors.New("database catalog is not initialized")
	}
	candidatePackages := clonePackageSet(state.PackageCommits)
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.PackageID == "" || candidate.CandidateCommit == "" {
			return PendingDeployment{}, errors.New("database deployment candidate package and commit are required")
		}
		candidate.PreviousCommit = candidatePackages[candidate.PackageID]
		candidatePackages[candidate.PackageID] = candidate.CandidateCommit
	}
	previousJSON, _ := json.Marshal(state.PackageCommits)
	candidateJSON, _ := json.Marshal(candidatePackages)
	candidatesJSON, _ := json.Marshal(candidates)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	pending := PendingDeployment{
		PreviousPackageSetHash: state.PackageSetHash, PreviousPackageCommits: state.PackageCommits,
		CandidatePackageSetHash: PackageSetHash(candidatePackages), CandidatePackageCommits: candidatePackages,
		Candidates: candidates, Stage: "preparing", StartedAt: now, UpdatedAt: now,
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO _8020_pending_deployment
		(deployment_id, previous_package_set_hash, previous_package_set_json, candidate_package_set_hash,
		candidate_package_set_json, candidates_json, stage, error, started_at, updated_at)
		VALUES ('current', $1, $2, $3, $4, $5, $6, '', $7, $7)`,
		pending.PreviousPackageSetHash, string(previousJSON), pending.CandidatePackageSetHash,
		string(candidateJSON), string(candidatesJSON), pending.Stage, now)
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		m.statusMu.Lock()
		m.status.PendingDeployment = true
		m.statusMu.Unlock()
	}
	return pending, err
}

// CompleteDeployment closes the durable package/schema switch boundary.
func (m *Manager) CompleteDeployment(ctx context.Context, activated bool) error {
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return err
	}
	pending, exists, err := pendingDeployment(ctx, tx)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("database schema deployment is not pending")
	}
	var activatedState CatalogState
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if activated {
		descriptorHash, err := descriptorSetHash(ctx, tx)
		if err != nil {
			return err
		}
		packagesJSON, _ := json.Marshal(pending.CandidatePackageCommits)
		if _, err := tx.ExecContext(ctx, `UPDATE _8020_catalog SET package_set_hash = $1, package_set_json = $2,
			descriptor_set_hash = $3, updated_at = $4, last_error = '', last_deployment_at = $4,
			last_deployment_error = '' WHERE catalog_id = 'system'`,
			pending.CandidatePackageSetHash, string(packagesJSON), descriptorHash, now); err != nil {
			return err
		}
		for _, candidate := range pending.Candidates {
			if _, err := tx.ExecContext(ctx, `UPDATE _8020_tables SET source_commit = $1
				WHERE source_package = $2 AND state = 'active'`, candidate.CandidateCommit, candidate.PackageID); err != nil {
				return err
			}
		}
		activatedState = CatalogState{
			PackageSetHash: pending.CandidatePackageSetHash, PackageCommits: pending.CandidatePackageCommits,
			DescriptorSetHash: descriptorHash,
		}
	} else {
		message := pending.Error
		if message == "" {
			message = "database schema deployment was rolled back before source activation"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE _8020_catalog SET updated_at = $1,
			last_deployment_at = $1, last_deployment_error = $2 WHERE catalog_id = 'system'`, now, message); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM _8020_pending_deployment WHERE deployment_id = 'current'`)
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		m.statusMu.Lock()
		m.status.PendingDeployment = false
		if activated {
			m.status.PackageSetHash = activatedState.PackageSetHash
			m.status.DescriptorSetHash = activatedState.DescriptorSetHash
		}
		m.status.LastDeploymentAt = now
		if activated {
			m.status.LastDeploymentError = ""
		} else {
			m.status.LastDeploymentError = pending.Error
			if m.status.LastDeploymentError == "" {
				m.status.LastDeploymentError = "database schema deployment was rolled back before source activation"
			}
		}
		m.statusMu.Unlock()
	}
	return err
}

func (m *Manager) PendingDeployment(ctx context.Context) (PendingDeployment, bool, error) {
	return pendingDeployment(ctx, m.db)
}

func (m *Manager) UpdatePendingDeployment(ctx context.Context, stage string, failure error) error {
	if stage == "" {
		return errors.New("database deployment stage is required")
	}
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE _8020_pending_deployment SET stage = $1, error = $2, updated_at = $3 WHERE deployment_id = 'current'`,
		stage, message, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("database schema deployment is not pending")
	}
	return tx.Commit()
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func pendingDeployment(ctx context.Context, query rowQuerier) (PendingDeployment, bool, error) {
	var pending PendingDeployment
	var previousJSON, candidateJSON, candidatesJSON string
	err := query.QueryRowContext(ctx, `SELECT previous_package_set_hash, previous_package_set_json,
		candidate_package_set_hash, candidate_package_set_json, candidates_json, stage, error, started_at, updated_at
		FROM _8020_pending_deployment WHERE deployment_id = 'current'`).Scan(
		&pending.PreviousPackageSetHash, &previousJSON, &pending.CandidatePackageSetHash, &candidateJSON,
		&candidatesJSON, &pending.Stage, &pending.Error, &pending.StartedAt, &pending.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingDeployment{}, false, nil
	}
	if err != nil {
		return PendingDeployment{}, false, err
	}
	if err := json.Unmarshal([]byte(previousJSON), &pending.PreviousPackageCommits); err != nil {
		return PendingDeployment{}, false, fmt.Errorf("decode previous package set: %w", err)
	}
	if err := json.Unmarshal([]byte(candidateJSON), &pending.CandidatePackageCommits); err != nil {
		return PendingDeployment{}, false, fmt.Errorf("decode candidate package set: %w", err)
	}
	if err := json.Unmarshal([]byte(candidatesJSON), &pending.Candidates); err != nil {
		return PendingDeployment{}, false, fmt.Errorf("decode deployment candidates: %w", err)
	}
	return pending, true, nil
}

func clonePackageSet(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for packageID, commit := range source {
		result[packageID] = commit
	}
	return result
}

// PackageSetHash is the durable identity of sorted package IDs and exact commits.
func PackageSetHash(packages map[string]string) string {
	entries := make([]string, 0, len(packages))
	for packageID, commit := range packages {
		entries = append(entries, packageID+"="+commit)
	}
	sort.Strings(entries)
	digest := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(digest[:])
}

func catalogState(ctx context.Context, query rowQuerier) (CatalogState, error) {
	var result CatalogState
	var encoded string
	err := query.QueryRowContext(ctx, `SELECT package_set_hash, package_set_json, descriptor_set_hash
		FROM _8020_catalog WHERE catalog_id = 'system'`).Scan(&result.PackageSetHash, &encoded, &result.DescriptorSetHash)
	if err != nil {
		return CatalogState{}, err
	}
	if err := json.Unmarshal([]byte(encoded), &result.PackageCommits); err != nil {
		return CatalogState{}, fmt.Errorf("decode catalog package set: %w", err)
	}
	if result.PackageCommits == nil {
		result.PackageCommits = map[string]string{}
	}
	if result.PackageSetHash != PackageSetHash(result.PackageCommits) {
		return CatalogState{}, errors.New("database catalog package-set hash differs")
	}
	return result, nil
}

func (m *Manager) CatalogState(ctx context.Context) (CatalogState, error) {
	return catalogState(ctx, m.db)
}

func descriptorSetHash(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (string, error) {
	rows, err := query.QueryContext(ctx, `SELECT table_id, descriptor_hash FROM _8020_tables WHERE state = 'active' ORDER BY table_id`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	entries := []string{}
	for rows.Next() {
		var tableID, descriptorHash string
		if err := rows.Scan(&tableID, &descriptorHash); err != nil {
			return "", err
		}
		entries = append(entries, tableID+"="+descriptorHash)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(digest[:]), nil
}

type TableSummary struct {
	TableID              string `json:"table_id"`
	SourcePackage        string `json:"source_package"`
	SourceCommit         string `json:"source_commit"`
	SourceModule         string `json:"source_module"`
	State                string `json:"state"`
	SynchronizationState string `json:"synchronization_state"`
	DescriptorHash       string `json:"descriptor_hash"`
	DescriptorJSON       string `json:"descriptor_json,omitempty"`
	SynchronizedAt       string `json:"synchronized_at,omitempty"`
	ActiveColumns        int    `json:"active_columns"`
	RetiredColumns       int    `json:"retired_columns"`
	Error                string `json:"error,omitempty"`
	DefinitionState      string `json:"definition_state,omitempty"`
	CurrentSourceCommit  string `json:"current_source_commit,omitempty"`
}

type DefinitionSummary struct {
	TableID         string `json:"table_id"`
	SourcePackage   string `json:"source_package"`
	SourceCommit    string `json:"source_commit"`
	SourceModule    string `json:"source_module"`
	DescriptorHash  string `json:"descriptor_hash"`
	CatalogState    string `json:"catalog_state"`
	CatalogHash     string `json:"catalog_hash,omitempty"`
	Synchronization string `json:"synchronization_state"`
	Error           string `json:"error,omitempty"`
}

type TableDetail struct {
	TableSummary
	Descriptor            TableDescriptor  `json:"descriptor"`
	CurrentDescriptor     *TableDescriptor `json:"current_descriptor,omitempty"`
	CurrentDescriptorHash string           `json:"current_descriptor_hash,omitempty"`
	Columns               []CatalogColumn  `json:"columns"`
	Physical              []PhysicalColumn `json:"physical_columns"`
	PhysicalIndexes       []PhysicalIndex  `json:"physical_indexes"`
	PhysicalChecks        []string         `json:"physical_checks"`
	Differences           []string         `json:"differences"`
}

type CatalogColumn struct {
	TableID        string `json:"table_id"`
	ColumnName     string `json:"column_name"`
	LogicalType    string `json:"logical_type"`
	DefinitionHash string `json:"definition_hash"`
	DefinitionJSON string `json:"definition_json"`
	State          string `json:"state"`
}

type PhysicalColumn struct {
	Name               string `json:"name"`
	Type               string `json:"type"`
	Nullable           bool   `json:"nullable"`
	Default            string `json:"default,omitempty"`
	PrimaryKey         bool   `json:"primary_key"`
	PrimaryKeyPosition int    `json:"primary_key_position,omitempty"`
	Generated          bool   `json:"generated"`
}

type PhysicalIndex struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
}

var sqliteNamedCheck = regexp.MustCompile(`(?i)CONSTRAINT\s+"([^"]+)"\s+CHECK\s*\(`)

type TableSource struct {
	TableID       string
	SourcePackage string
	SourceCommit  string
	SourceModule  string
}

func (m *Manager) TableSourcesForPackages(ctx context.Context, packages []string) ([]TableSource, error) {
	result := []TableSource{}
	seen := map[string]bool{}
	for _, packageID := range packages {
		rows, err := m.db.QueryContext(ctx, `SELECT table_id, source_package, source_commit, source_module
			FROM _8020_tables WHERE source_package = $1 AND state = 'active' ORDER BY table_id`, packageID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source TableSource
			if err := rows.Scan(&source.TableID, &source.SourcePackage, &source.SourceCommit, &source.SourceModule); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[source.TableID] {
				seen[source.TableID] = true
				result = append(result, source)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TableID < result[j].TableID })
	return result, nil
}

func (m *Manager) TableSourcesForDependencies(ctx context.Context, modules []string) ([]TableSource, error) {
	result := []TableSource{}
	seen := map[string]bool{}
	for _, module := range modules {
		rows, err := m.db.QueryContext(ctx, `SELECT t.table_id, t.source_package, t.source_commit, t.source_module
			FROM _8020_dependencies d JOIN _8020_tables t ON t.table_id = d.table_id
			WHERE d.module_path = $1 AND t.state = 'active' ORDER BY t.table_id`, module)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source TableSource
			if err := rows.Scan(&source.TableID, &source.SourcePackage, &source.SourceCommit, &source.SourceModule); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[source.TableID] {
				seen[source.TableID] = true
				result = append(result, source)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TableID < result[j].TableID })
	return result, nil
}

func (m *Manager) CompletedTableIDs(ctx context.Context, packageCommits map[string]string) (map[string]bool, error) {
	result := map[string]bool{}
	for packageID, commit := range packageCommits {
		rows, err := m.db.QueryContext(ctx, `SELECT table_id FROM _8020_tables
			WHERE source_package = $1 AND source_commit = $2 AND state = 'active' AND synchronization_state = 'synchronized'`, packageID, commit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tableID string
			if err := rows.Scan(&tableID); err != nil {
				rows.Close()
				return nil, err
			}
			result[tableID] = true
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// InitializeCatalog bootstraps only the platform-owned metadata catalog.
func (m *Manager) InitializeCatalog(ctx context.Context) (Status, error) {
	if m == nil || m.db == nil {
		return m.Status(), errors.New("database is unavailable")
	}
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	if err := m.ping(ctx); err != nil {
		m.statusMu.Lock()
		m.status.State = StateUnavailable
		m.status.Error = err.Error()
		m.status.CatalogError = err.Error()
		m.statusMu.Unlock()
		return m.Status(), err
	}
	m.statusMu.Lock()
	m.status.State = StateInitializing
	m.status.Error = ""
	m.statusMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		m.setCatalogFailure(err)
		return m.Status(), err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		m.setCatalogFailure(err)
		return m.Status(), err
	}
	for _, statement := range catalogStatements(m.status.Backend) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			failure := fmt.Errorf("initialize database catalog: %w", err)
			m.setCatalogFailure(failure)
			return m.Status(), failure
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	emptyPackages := map[string]string{}
	emptyPackagesJSON, _ := json.Marshal(emptyPackages)
	if _, err := tx.ExecContext(ctx, `INSERT INTO _8020_catalog
		(catalog_id, catalog_version, initialized, package_set_hash, package_set_json, descriptor_set_hash,
		created_at, initialized_at, updated_at, last_error, last_deployment_at, last_deployment_error)
		VALUES ('system', $1, 0, $2, $3, '', $4, '', $4, '', '', '') ON CONFLICT (catalog_id) DO NOTHING`,
		catalogVersion, PackageSetHash(emptyPackages), string(emptyPackagesJSON), now); err != nil {
		m.setCatalogFailure(err)
		return m.Status(), err
	}
	var version int
	var initialized int
	var packageSetHash, descriptorHash, initializedAt, catalogError, lastDeploymentAt, lastDeploymentError string
	if err := tx.QueryRowContext(ctx, `SELECT catalog_version, initialized, package_set_hash, descriptor_set_hash,
		initialized_at, last_error, last_deployment_at, last_deployment_error
		FROM _8020_catalog WHERE catalog_id = 'system'`).Scan(
		&version, &initialized, &packageSetHash, &descriptorHash, &initializedAt, &catalogError,
		&lastDeploymentAt, &lastDeploymentError,
	); err != nil {
		m.setCatalogFailure(err)
		return m.Status(), err
	}
	if version != catalogVersion {
		err := fmt.Errorf("unsupported database catalog version %d", version)
		m.setCatalogFailure(err)
		return m.Status(), err
	}
	if err := validateCatalogContract(ctx, tx); err != nil {
		failure := fmt.Errorf("validate database catalog: %w", err)
		m.setCatalogFailure(failure)
		return m.Status(), failure
	}
	if err := tx.Commit(); err != nil {
		m.setCatalogFailure(err)
		return m.Status(), err
	}
	var pending int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM _8020_pending_deployment`).Scan(&pending); err != nil {
		m.setCatalogFailure(err)
		return m.Status(), err
	}
	m.statusMu.Lock()
	m.status.State = StateConnected
	if initialized != 0 {
		m.status.State = StateReady
	}
	if catalogError != "" {
		m.status.State = StateDegraded
	}
	m.status.Error = ""
	m.status.CatalogError = catalogError
	m.status.CatalogVersion = version
	m.status.Initialized = initialized != 0
	m.status.PendingDeployment = pending != 0
	m.status.PackageSetHash = packageSetHash
	m.status.DescriptorSetHash = descriptorHash
	m.status.InitializedAt = initializedAt
	m.status.LastDeploymentAt = lastDeploymentAt
	m.status.LastDeploymentError = lastDeploymentError
	m.statusMu.Unlock()
	return m.Status(), nil
}

func validateCatalogContract(ctx context.Context, tx *sql.Tx) error {
	queries := []string{
		`SELECT catalog_id, catalog_version, initialized, package_set_hash, package_set_json, descriptor_set_hash,
			created_at, initialized_at, updated_at, last_error, last_deployment_at, last_deployment_error
			FROM _8020_catalog WHERE 1 = 0`,
		`SELECT table_id, descriptor_hash, descriptor_json, source_package, source_commit, source_module,
			state, synchronization_state, synchronized_at, error FROM _8020_tables WHERE 1 = 0`,
		`SELECT table_id, column_name, logical_type, definition_hash, definition_json, state
			FROM _8020_columns WHERE 1 = 0`,
		`SELECT table_id, module_path FROM _8020_dependencies WHERE 1 = 0`,
		`SELECT deployment_id, previous_package_set_hash, previous_package_set_json, candidate_package_set_hash,
			candidate_package_set_json, candidates_json, stage, error, started_at, updated_at
			FROM _8020_pending_deployment WHERE 1 = 0`,
	}
	for _, query := range queries {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func catalogStatements(backend string) []string {
	script := sqliteCatalogSQL
	if backend == BackendPostgreSQL {
		script = postgresqlCatalogSQL
	}
	parts := strings.Split(script, "\n-- 8020:next\n")
	result := make([]string, 0, len(parts))
	for _, statement := range parts {
		if statement = strings.TrimSpace(statement); statement != "" {
			result = append(result, statement)
		}
	}
	return result
}

func (m *Manager) lockSchema(ctx context.Context, tx *sql.Tx) error {
	if m.status.Backend != BackendPostgreSQL || ctx.Value(deploymentLockContextKey{}) == true {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, postgresSchemaLock)
	return err
}

// AcquireDeploymentLock serializes a complete PostgreSQL schema deployment.
// SQLite is deliberately single-node and needs only its local schema mutex.
func (m *Manager) AcquireDeploymentLock(ctx context.Context) (context.Context, func(), error) {
	if m.status.Backend != BackendPostgreSQL || ctx.Value(deploymentLockContextKey{}) == true {
		return ctx, func() {}, nil
	}
	connection, err := m.db.Conn(ctx)
	if err != nil {
		return ctx, nil, err
	}
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, postgresSchemaLock); err != nil {
		connection.Close()
		return ctx, nil, err
	}
	release := func() {
		unlockContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = connection.ExecContext(unlockContext, `SELECT pg_advisory_unlock($1)`, postgresSchemaLock)
		_ = connection.Close()
	}
	return context.WithValue(ctx, deploymentLockContextKey{}, true), release, nil
}

func (m *Manager) setCatalogFailure(err error) {
	m.statusMu.Lock()
	m.status.State = StateDegraded
	m.status.Error = err.Error()
	m.status.CatalogError = err.Error()
	m.statusMu.Unlock()
}

func (m *Manager) markInitialized(ctx context.Context, packageCommits map[string]string) error {
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return err
	}
	descriptorHash, err := descriptorSetHash(ctx, tx)
	if err != nil {
		return err
	}
	packageJSON, _ := json.Marshal(packageCommits)
	packageHash := PackageSetHash(packageCommits)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `UPDATE _8020_catalog SET initialized = 1, package_set_hash = $1,
		package_set_json = $2, descriptor_set_hash = $3,
		initialized_at = CASE WHEN initialized_at = '' THEN $4 ELSE initialized_at END,
		updated_at = $4, last_error = '' WHERE catalog_id = 'system'`, packageHash, string(packageJSON), descriptorHash, now)
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		m.statusMu.Lock()
		m.status.Initialized = true
		m.status.State = StateReady
		m.status.Error = ""
		m.status.CatalogError = ""
		m.status.PackageSetHash = packageHash
		m.status.DescriptorSetHash = descriptorHash
		if m.status.InitializedAt == "" {
			m.status.InitializedAt = now
		}
		m.statusMu.Unlock()
	}
	return err
}

// BeginInitialization marks the service plane as unavailable while package tables are synchronized.
func (m *Manager) BeginInitialization() {
	m.statusMu.Lock()
	m.status.State = StateInitializing
	m.status.Error = ""
	m.status.CatalogError = ""
	m.statusMu.Unlock()
}

// SetInitializationFailure keeps SQL recovery available while blocking services.
func (m *Manager) SetInitializationFailure(ctx context.Context, failure error) {
	if failure == nil {
		return
	}
	message := failure.Error()
	if m.db != nil {
		_, _ = m.db.ExecContext(ctx, `UPDATE _8020_catalog SET last_error = $1, updated_at = $2 WHERE catalog_id = 'system'`,
			message, time.Now().UTC().Format(time.RFC3339Nano))
	}
	m.statusMu.Lock()
	m.status.State = StateDegraded
	m.status.Error = message
	m.status.CatalogError = message
	m.statusMu.Unlock()
}

type SynchronizationOptions struct {
	Full                    bool
	Recovery                bool
	SkipReferenceValidation bool
	RetireMissingPackages   []string
	RetireTables            []string
	PackageCommits          map[string]string
}

// Synchronize applies conservative additive changes and records retired definitions.
func (m *Manager) Synchronize(ctx context.Context, tables []EvaluatedTable, options SynchronizationOptions) ([]SynchronizationResult, error) {
	if status := m.Status(); status.CatalogVersion != catalogVersion || status.CatalogError != "" {
		return nil, errors.New("database catalog is not ready")
	}
	sorted := append([]EvaluatedTable(nil), tables...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Descriptor.TableID < sorted[j].Descriptor.TableID })
	referenceErrors := map[string]error{}
	if !options.SkipReferenceValidation {
		var err error
		referenceErrors, err = m.validateReferences(ctx, sorted)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	results := make([]SynchronizationResult, 0, len(sorted))
	var failures []error
	for _, table := range sorted {
		id := table.Descriptor.TableID
		if referenceErr := referenceErrors[id]; referenceErr != nil {
			failures = append(failures, referenceErr)
			results = append(results, SynchronizationResult{TableID: id, State: "error", Error: referenceErr.Error()})
			continue
		}
		if seen[id] {
			err := fmt.Errorf("duplicate canonical table ID %s", id)
			failures = append(failures, err)
			results = append(results, SynchronizationResult{TableID: id, State: "error", Error: err.Error()})
			continue
		}
		seen[id] = true
		state, err := m.synchronizeTable(ctx, table, options.Recovery)
		result := SynchronizationResult{TableID: id, State: state}
		if err != nil {
			result.Error = err.Error()
			failures = append(failures, fmt.Errorf("%s: %w", id, err))
		}
		results = append(results, result)
	}
	if len(failures) == 0 && (options.Full || len(options.RetireMissingPackages) > 0) {
		if err := m.retireMissingTables(ctx, seen, options.RetireMissingPackages, options.Full); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 && len(options.RetireTables) > 0 {
		if err := m.retireTables(ctx, options.RetireTables); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 && options.Full {
		if err := m.markInitialized(ctx, options.PackageCommits); err != nil {
			failures = append(failures, err)
		}
	}
	return results, errors.Join(failures...)
}

func (m *Manager) FinalizeFullSynchronization(ctx context.Context, tableIDs []string, packageCommits map[string]string) error {
	seen := make(map[string]bool, len(tableIDs))
	for _, tableID := range tableIDs {
		if !validTableID(tableID) || seen[tableID] {
			return fmt.Errorf("invalid or duplicate table ID %q", tableID)
		}
		seen[tableID] = true
	}
	if err := m.retireMissingTables(ctx, seen, nil, true); err != nil {
		return err
	}
	if err := m.ValidateCatalogReferences(ctx); err != nil {
		return err
	}
	return m.markInitialized(ctx, packageCommits)
}

// ValidateCatalogReferences verifies logical links after a bounded deployment
// has made every candidate descriptor visible in the catalog.
func (m *Manager) ValidateCatalogReferences(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `SELECT descriptor_json, descriptor_hash, source_package, source_commit, source_module
		FROM _8020_tables WHERE state = 'active' ORDER BY table_id`)
	if err != nil {
		return err
	}
	tables := []EvaluatedTable{}
	for rows.Next() {
		var table EvaluatedTable
		if err := rows.Scan(&table.DescriptorJSON, &table.DescriptorHash, &table.SourcePackage, &table.SourceCommit, &table.SourceModule); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(table.DescriptorJSON), &table.Descriptor); err != nil {
			return fmt.Errorf("decode catalog descriptor: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	failures, err := m.validateReferences(ctx, tables)
	if err != nil {
		return err
	}
	joined := make([]error, 0, len(failures))
	for _, table := range tables {
		if failure := failures[table.Descriptor.TableID]; failure != nil {
			joined = append(joined, failure)
		}
	}
	return errors.Join(joined...)
}

func (m *Manager) validateReferences(ctx context.Context, tables []EvaluatedTable) (map[string]error, error) {
	type columnIdentity struct {
		table, column string
	}
	available := map[columnIdentity]string{}
	rows, err := m.db.QueryContext(ctx, `SELECT c.table_id, c.column_name, c.logical_type
		FROM _8020_columns c JOIN _8020_tables t ON t.table_id = c.table_id
		WHERE c.state = 'active' AND t.state = 'active'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var identity columnIdentity
		var logicalType string
		if err := rows.Scan(&identity.table, &identity.column, &logicalType); err != nil {
			rows.Close()
			return nil, err
		}
		available[identity] = logicalType
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, table := range tables {
		for _, column := range table.Descriptor.Columns {
			available[columnIdentity{table: table.Descriptor.TableID, column: column.Name}] = column.LogicalType
		}
	}
	failures := map[string]error{}
	for _, table := range tables {
		for _, column := range table.Descriptor.Columns {
			if column.Reference == nil {
				continue
			}
			targetType, exists := available[columnIdentity{table: column.Reference.Table, column: column.Reference.Column}]
			if !exists {
				failures[table.Descriptor.TableID] = fmt.Errorf("%s.%s references missing column %s.%s", table.Descriptor.TableID, column.Name, column.Reference.Table, column.Reference.Column)
				break
			}
			if targetType != column.LogicalType {
				failures[table.Descriptor.TableID] = fmt.Errorf("%s.%s type %s does not match referenced %s.%s type %s", table.Descriptor.TableID, column.Name, column.LogicalType, column.Reference.Table, column.Reference.Column, targetType)
				break
			}
		}
	}
	return failures, nil
}

func (m *Manager) synchronizeTable(ctx context.Context, evaluated EvaluatedTable, recovery bool) (string, error) {
	if err := validateEvaluatedTable(evaluated); err != nil {
		return "error", err
	}
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return "error", err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return "error", err
	}
	previous, exists, err := catalogDescriptor(ctx, tx, evaluated.Descriptor.TableID)
	if err != nil {
		return "error", err
	}
	if exists {
		var sourcePackage, sourceModule string
		if err := tx.QueryRowContext(ctx, `SELECT source_package, source_module FROM _8020_tables WHERE table_id = $1`, evaluated.Descriptor.TableID).Scan(&sourcePackage, &sourceModule); err != nil {
			return "error", err
		}
		if sourcePackage != evaluated.SourcePackage || sourceModule != evaluated.SourceModule {
			return "error", fmt.Errorf("canonical table ID %s is already owned by %s at %s", evaluated.Descriptor.TableID, sourcePackage, sourceModule)
		}
	}
	if !exists {
		physical, err := m.physicalColumns(ctx, tx, evaluated.Descriptor.TableID)
		if err != nil {
			return "error", err
		}
		if len(physical) == 0 {
			if err := m.createTable(ctx, tx, evaluated.Descriptor); err != nil {
				return "error", err
			}
		} else if differences := comparePhysical(m.status.Backend, evaluated.Descriptor, physical); len(differences) > 0 {
			return "drift", fmt.Errorf("uncatalogued physical table differs: %s", strings.Join(differences, "; "))
		}
	} else if err := m.applyDescriptorChange(ctx, tx, previous, evaluated.Descriptor, recovery); err != nil {
		return "migration_required", err
	}
	if err := m.ensurePhysicalTable(ctx, tx, evaluated.Descriptor); err != nil {
		return "drift", err
	}
	if err := writeCatalogTable(ctx, tx, evaluated); err != nil {
		return "error", err
	}
	if err := tx.Commit(); err != nil {
		return "error", err
	}
	return "synchronized", nil
}

func validateEvaluatedTable(table EvaluatedTable) error {
	descriptor := table.Descriptor
	if descriptor.FormatVersion != 1 || !validTableID(descriptor.TableID) || len(descriptor.Columns) == 0 {
		return errors.New("invalid table descriptor")
	}
	encoded, err := marshalCanonicalJSON(descriptor)
	if err != nil {
		return err
	}
	if string(encoded) != table.DescriptorJSON {
		return errors.New("descriptor JSON is not canonical")
	}
	hash := sha256.Sum256(encoded)
	if hex.EncodeToString(hash[:]) != table.DescriptorHash {
		return errors.New("descriptor hash does not match descriptor JSON")
	}
	columns := map[string]bool{}
	primary := map[string]bool{}
	for _, name := range descriptor.PrimaryKey {
		if primary[name] {
			return fmt.Errorf("duplicate primary-key column %s", name)
		}
		primary[name] = true
	}
	generated := 0
	for _, column := range descriptor.Columns {
		if !validColumnName(column.Name) || reservedColumnName(column.Name) || columns[column.Name] {
			return fmt.Errorf("invalid or duplicate column %q", column.Name)
		}
		columns[column.Name] = true
		if column.PrimaryKey != primary[column.Name] {
			return fmt.Errorf("primary-key metadata disagrees for column %s", column.Name)
		}
		if _, err := physicalType(BackendSQLite, column); err != nil {
			return err
		}
		if err := validateColumnDescriptor(column); err != nil {
			return fmt.Errorf("column %s: %w", column.Name, err)
		}
		if column.Generated {
			generated++
			if column.LogicalType != "integer" || !column.PrimaryKey || len(descriptor.PrimaryKey) != 1 {
				return errors.New("generated column must be the single integer primary key")
			}
		}
	}
	if generated > 1 {
		return errors.New("only one generated column is supported")
	}
	for name := range primary {
		if !columns[name] {
			return fmt.Errorf("primary key references unknown column %s", name)
		}
	}
	indexNames := map[string]bool{}
	for _, index := range descriptor.Indexes {
		if !validColumnName(index.Name) || len(index.Columns) == 0 || indexNames[index.Name] {
			return fmt.Errorf("invalid index %q", index.Name)
		}
		indexNames[index.Name] = true
		indexColumns := map[string]bool{}
		for _, name := range index.Columns {
			if !columns[name] || indexColumns[name] {
				return fmt.Errorf("index %s references unknown column %s", index.Name, name)
			}
			indexColumns[name] = true
		}
	}
	return nil
}

func marshalCanonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func reservedColumnName(value string) bool {
	switch value {
	case "table", "select", "selectAll", "insert", "update", "delete":
		return true
	default:
		return false
	}
}

func validateColumnDescriptor(column ColumnDescriptor) error {
	if column.PrimaryKey && column.Nullable {
		return errors.New("primary-key columns cannot be nullable")
	}
	if column.Generated && column.Default != nil {
		return errors.New("generated columns cannot have defaults")
	}
	if column.LogicalType == "enum" {
		if len(column.EnumValues) == 0 {
			return errors.New("enum values are required")
		}
		seen := map[string]bool{}
		for _, value := range column.EnumValues {
			if value == "" || seen[value] {
				return errors.New("enum values must be non-empty and unique")
			}
			seen[value] = true
		}
	} else if len(column.EnumValues) != 0 {
		return errors.New("enum values are valid only for enum columns")
	}
	if column.LogicalType != "decimal" && (column.Precision != 0 || column.Scale != 0) {
		return errors.New("precision and scale are valid only for decimal columns")
	}
	if column.Reference != nil && (!validTableID(column.Reference.Table) || !validColumnName(column.Reference.Column)) {
		return errors.New("invalid logical reference")
	}
	if column.Default == nil {
		return nil
	}
	if column.Default.Kind == "now" {
		if column.LogicalType != "datetime" || column.Default.Value != nil {
			return errors.New("defaultNow() is valid only for datetime columns")
		}
		return nil
	}
	if column.Default.Kind != "literal" {
		return errors.New("invalid default kind")
	}
	switch column.LogicalType {
	case "text":
		if _, ok := column.Default.Value.(string); !ok {
			return errors.New("text default must be a string")
		}
	case "enum":
		value, ok := column.Default.Value.(string)
		if !ok || !containsString(column.EnumValues, value) {
			return errors.New("enum default must be a declared value")
		}
	case "boolean":
		if _, ok := column.Default.Value.(bool); !ok {
			return errors.New("boolean default must be a boolean")
		}
	case "integer":
		value, ok := column.Default.Value.(float64)
		if !ok || value != float64(int64(value)) || value < -9007199254740991 || value > 9007199254740991 {
			return errors.New("integer default must be a JavaScript safe integer")
		}
	case "float":
		value, ok := column.Default.Value.(float64)
		if !ok || value != value || value > 1.7976931348623157e308 || value < -1.7976931348623157e308 {
			return errors.New("float default must be finite")
		}
	case "decimal":
		value, ok := column.Default.Value.(string)
		if !ok {
			return errors.New("decimal default must be a canonical string")
		}
		if _, err := scaledDecimal(value, column.Precision, column.Scale); err != nil {
			return err
		}
	case "datetime":
		value, ok := column.Default.Value.(string)
		if !ok {
			return errors.New("datetime default must be a UTC timestamp")
		}
		instant, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || instant.Location() != time.UTC || instant.Nanosecond()%int(time.Millisecond) != 0 {
			return errors.New("datetime default must be UTC with millisecond precision")
		}
	case "bytes":
		value, ok := column.Default.Value.(string)
		if !ok {
			return errors.New("bytes default must be base64")
		}
		if _, err := base64.StdEncoding.DecodeString(value); err != nil {
			return errors.New("bytes default must be base64")
		}
	case "json":
		if _, err := json.Marshal(column.Default.Value); err != nil {
			return errors.New("JSON default is invalid")
		}
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validTableID(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	if len(value) == 63 && value[52] == '_' {
		if value[0] == '_' {
			return false
		}
		for index, character := range value {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
				return false
			}
			if index > 52 && !((character >= 'a' && character <= 'f') || (character >= '0' && character <= '9')) {
				return false
			}
		}
		return true
	}
	parts := strings.Split(value, "__")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || normalizeIdentity(part) != part {
			return false
		}
	}
	return true
}

func validColumnName(value string) bool {
	if value == "" || len(value) > 63 || (value[0] < 'A' || value[0] > 'Z') && (value[0] < 'a' || value[0] > 'z') && value[0] != '_' {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func catalogDescriptor(ctx context.Context, tx *sql.Tx, tableID string) (TableDescriptor, bool, error) {
	var encoded string
	err := tx.QueryRowContext(ctx, `SELECT descriptor_json FROM _8020_tables WHERE table_id = $1`, tableID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return TableDescriptor{}, false, nil
	}
	if err != nil {
		return TableDescriptor{}, false, err
	}
	var descriptor TableDescriptor
	if err := json.Unmarshal([]byte(encoded), &descriptor); err != nil {
		return TableDescriptor{}, false, fmt.Errorf("decode stored descriptor: %w", err)
	}
	return descriptor, true, nil
}

func (m *Manager) createTable(ctx context.Context, tx *sql.Tx, descriptor TableDescriptor) error {
	parts := make([]string, 0, len(descriptor.Columns)+1)
	inlinePrimary := false
	for _, column := range descriptor.Columns {
		definition, err := m.columnSQL(descriptor.TableID, column, true)
		if err != nil {
			return err
		}
		if column.Generated && m.status.Backend == BackendSQLite {
			inlinePrimary = true
		}
		parts = append(parts, definition)
	}
	if len(descriptor.PrimaryKey) > 0 && !inlinePrimary {
		quoted := make([]string, len(descriptor.PrimaryKey))
		for index, name := range descriptor.PrimaryKey {
			quoted[index] = quoteIdentifier(name)
		}
		parts = append(parts, "PRIMARY KEY ("+strings.Join(quoted, ", ")+")")
	}
	suffix := ""
	if m.status.Backend == BackendSQLite {
		suffix = " STRICT"
	}
	statement := "CREATE TABLE " + quoteIdentifier(descriptor.TableID) + " (" + strings.Join(parts, ", ") + ")" + suffix
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	return m.ensureIndexes(ctx, tx, descriptor)
}

func (m *Manager) applyDescriptorChange(ctx context.Context, tx *sql.Tx, previous, next TableDescriptor, recovery bool) error {
	oldColumns := map[string]ColumnDescriptor{}
	for _, column := range previous.Columns {
		oldColumns[column.Name] = column
	}
	newColumns := map[string]ColumnDescriptor{}
	for _, column := range next.Columns {
		newColumns[column.Name] = column
		old, exists := oldColumns[column.Name]
		if !exists {
			physical, err := m.physicalColumns(ctx, tx, next.TableID)
			if err != nil {
				return err
			}
			var existing *PhysicalColumn
			for index := range physical {
				if physical[index].Name == column.Name {
					existing = &physical[index]
					break
				}
			}
			if existing != nil {
				if recovery && m.status.Backend == BackendPostgreSQL && !column.Nullable && existing.Nullable {
					adjusted := *existing
					adjusted.Nullable = false
					if len(comparePhysicalColumn(m.status.Backend, column, adjusted)) == 0 {
						if _, err := tx.ExecContext(ctx, "ALTER TABLE "+quoteIdentifier(next.TableID)+" ALTER COLUMN "+quoteIdentifier(column.Name)+" SET NOT NULL"); err != nil {
							return fmt.Errorf("restore required column %s: %w", column.Name, err)
						}
						existing.Nullable = false
					}
				}
				if differences := comparePhysicalColumn(m.status.Backend, column, *existing); len(differences) > 0 {
					return fmt.Errorf("existing column %s differs: %s", column.Name, strings.Join(differences, "; "))
				}
				continue
			}
			if column.PrimaryKey || column.Generated || (!column.Nullable && (column.Default == nil || column.Default.Kind != "literal")) {
				return fmt.Errorf("adding column %s requires a migration", column.Name)
			}
			definition, err := m.columnSQL(next.TableID, column, false)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "ALTER TABLE "+quoteIdentifier(next.TableID)+" ADD COLUMN "+definition); err != nil {
				return fmt.Errorf("add column %s: %w", column.Name, err)
			}
			continue
		}
		if !samePhysicalColumnDefinition(old, column) {
			return fmt.Errorf("changing column %s requires a migration", column.Name)
		}
	}
	for name, old := range oldColumns {
		if _, exists := newColumns[name]; exists {
			continue
		}
		if !old.Nullable && old.Default == nil {
			if m.status.Backend == BackendPostgreSQL {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE "+quoteIdentifier(next.TableID)+" ALTER COLUMN "+quoteIdentifier(name)+" DROP NOT NULL"); err != nil {
					return fmt.Errorf("retire required column %s: %w", name, err)
				}
			} else {
				return fmt.Errorf("retiring required SQLite column %s requires a migration to relax nullability", name)
			}
		}
	}
	oldIndexes := map[string]IndexDescriptor{}
	for _, index := range previous.Indexes {
		oldIndexes[index.Name] = index
	}
	for _, index := range next.Indexes {
		if old, exists := oldIndexes[index.Name]; exists {
			oldJSON, _ := json.Marshal(old)
			newJSON, _ := json.Marshal(index)
			if string(oldJSON) != string(newJSON) {
				return fmt.Errorf("changing index %s requires a migration", index.Name)
			}
		}
		delete(oldIndexes, index.Name)
	}
	for name, index := range oldIndexes {
		retiredOnly := true
		for _, column := range index.Columns {
			if _, active := newColumns[column]; active {
				retiredOnly = false
				break
			}
		}
		if !retiredOnly {
			if recovery {
				if _, err := tx.ExecContext(ctx, "DROP INDEX IF EXISTS "+quoteIdentifier(name)); err != nil {
					return fmt.Errorf("remove candidate index %s: %w", name, err)
				}
				continue
			}
			return fmt.Errorf("removing index %s requires a migration", name)
		}
	}
	return m.ensureIndexes(ctx, tx, next)
}

func samePhysicalColumnDefinition(left, right ColumnDescriptor) bool {
	left.Reference, right.Reference = nil, nil
	left.Unique, right.Unique = false, false
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func (m *Manager) columnSQL(tableID string, column ColumnDescriptor, creating bool) (string, error) {
	dataType, err := physicalType(m.status.Backend, column)
	if err != nil {
		return "", err
	}
	parts := []string{quoteIdentifier(column.Name), dataType}
	if column.Generated {
		if m.status.Backend == BackendSQLite {
			parts = append(parts, "PRIMARY KEY")
		} else {
			parts = append(parts, "GENERATED BY DEFAULT AS IDENTITY")
		}
	}
	if !column.Nullable && !(column.Generated && m.status.Backend == BackendSQLite) {
		parts = append(parts, "NOT NULL")
	}
	if column.Default != nil {
		if !creating && column.Default.Kind == "now" {
			return "", fmt.Errorf("adding defaultNow() column %s requires a migration", column.Name)
		}
		value, err := m.defaultSQL(column)
		if err != nil {
			return "", err
		}
		parts = append(parts, "DEFAULT "+value)
	}
	if expression := checkExpression(m.status.Backend, column); expression != "" {
		parts = append(parts, "CONSTRAINT "+quoteIdentifier(checkName(tableID, column.Name))+" CHECK ("+expression+")")
	}
	return strings.Join(parts, " "), nil
}

func checkExpression(backend string, column ColumnDescriptor) string {
	name := quoteIdentifier(column.Name)
	switch column.LogicalType {
	case "boolean":
		if backend == BackendSQLite {
			return name + " IN (0, 1)"
		}
	case "integer":
		return name + " BETWEEN -9007199254740991 AND 9007199254740991"
	case "decimal":
		maximum := strings.Repeat("9", column.Precision)
		return name + " BETWEEN -" + maximum + " AND " + maximum
	case "enum":
		values := make([]string, len(column.EnumValues))
		for index, value := range column.EnumValues {
			values[index] = quoteLiteral(value)
		}
		return name + " IN (" + strings.Join(values, ", ") + ")"
	}
	return ""
}

func checkName(tableID, column string) string {
	return boundedIdentifier(tableID + "__" + column + "__check")
}

func boundedIdentifier(value string) string {
	if len(value) <= 63 {
		return value
	}
	hash := sha256.Sum256([]byte(value))
	return value[:52] + "_" + hex.EncodeToString(hash[:])[:10]
}

func physicalType(backend string, column ColumnDescriptor) (string, error) {
	switch column.LogicalType {
	case "text", "enum":
		if backend == BackendSQLite {
			return "TEXT", nil
		}
		return "text", nil
	case "boolean":
		if backend == BackendSQLite {
			return "INTEGER", nil
		}
		return "boolean", nil
	case "integer", "decimal":
		if column.LogicalType == "decimal" && (column.Precision < 1 || column.Precision > 18 || column.Scale < 0 || column.Scale > column.Precision) {
			return "", errors.New("decimal precision must be 1..18 and scale 0..precision")
		}
		if backend == BackendSQLite {
			return "INTEGER", nil
		}
		return "bigint", nil
	case "float":
		if backend == BackendSQLite {
			return "REAL", nil
		}
		return "double precision", nil
	case "datetime":
		if backend == BackendSQLite {
			return "TEXT", nil
		}
		return "timestamp with time zone", nil
	case "bytes":
		if backend == BackendSQLite {
			return "BLOB", nil
		}
		return "bytea", nil
	case "json":
		if backend == BackendSQLite {
			return "TEXT", nil
		}
		return "jsonb", nil
	default:
		return "", fmt.Errorf("unsupported logical type %q", column.LogicalType)
	}
}

func (m *Manager) defaultSQL(column ColumnDescriptor) (string, error) {
	return defaultSQLForBackend(m.status.Backend, column)
}

func defaultSQLForBackend(backend string, column ColumnDescriptor) (string, error) {
	if column.Default == nil {
		return "", nil
	}
	if column.Default.Kind == "now" {
		if column.LogicalType != "datetime" {
			return "", errors.New("defaultNow() is valid only for datetime")
		}
		if backend == BackendSQLite {
			return `(strftime('%Y-%m-%dT%H:%M:%fZ','now'))`, nil
		}
		return "date_trunc('milliseconds', CURRENT_TIMESTAMP)", nil
	}
	if column.Default.Kind != "literal" {
		return "", errors.New("unknown column default kind")
	}
	switch column.LogicalType {
	case "text", "enum", "datetime":
		value, ok := column.Default.Value.(string)
		if !ok {
			return "", errors.New("invalid string default")
		}
		return quoteLiteral(value), nil
	case "boolean":
		value, ok := column.Default.Value.(bool)
		if !ok {
			return "", errors.New("invalid boolean default")
		}
		if backend == BackendPostgreSQL {
			if value {
				return "TRUE", nil
			}
			return "FALSE", nil
		}
		if value {
			return "1", nil
		}
		return "0", nil
	case "integer", "float":
		return fmt.Sprint(column.Default.Value), nil
	case "decimal":
		value, ok := column.Default.Value.(string)
		if !ok {
			return "", errors.New("invalid decimal default")
		}
		scaled, err := scaledDecimal(value, column.Precision, column.Scale)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(scaled, 10), nil
	case "bytes":
		value, ok := column.Default.Value.(string)
		if !ok {
			return "", errors.New("invalid bytes default")
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", errors.New("invalid bytes default")
		}
		if backend == BackendSQLite {
			return "X'" + hex.EncodeToString(decoded) + "'", nil
		}
		return "decode(" + quoteLiteral(value) + "::text, 'base64'::text)", nil
	case "json":
		encoded, err := json.Marshal(column.Default.Value)
		if err != nil {
			return "", errors.New("invalid JSON default")
		}
		literal := quoteLiteral(string(encoded))
		if backend == BackendPostgreSQL {
			literal += "::jsonb"
		}
		return literal, nil
	default:
		return "", errors.New("unsupported default type")
	}
}

func (m *Manager) ensureIndexes(ctx context.Context, tx *sql.Tx, descriptor TableDescriptor) error {
	for _, index := range descriptor.Indexes {
		columns := make([]string, len(index.Columns))
		for position, name := range index.Columns {
			columns[position] = quoteIdentifier(name)
		}
		unique := ""
		if index.Unique {
			unique = "UNIQUE "
		}
		statement := "CREATE " + unique + "INDEX IF NOT EXISTS " + quoteIdentifier(index.Name) + " ON " + quoteIdentifier(descriptor.TableID) + " (" + strings.Join(columns, ", ") + ")"
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create index %s: %w", index.Name, err)
		}
	}
	return nil
}

func (m *Manager) ensurePhysicalTable(ctx context.Context, tx *sql.Tx, descriptor TableDescriptor) error {
	columns, err := m.physicalColumns(ctx, tx, descriptor.TableID)
	if err != nil {
		return err
	}
	actual := map[string]PhysicalColumn{}
	for _, column := range columns {
		actual[column.Name] = column
	}
	for _, expected := range descriptor.Columns {
		column, exists := actual[expected.Name]
		if !exists {
			return fmt.Errorf("physical column %s is missing", expected.Name)
		}
		if differences := comparePhysicalColumn(m.status.Backend, expected, column); len(differences) > 0 {
			return errors.New(strings.Join(differences, "; "))
		}
	}
	known, err := m.catalogColumnNames(ctx, tx, descriptor.TableID)
	if err != nil {
		return err
	}
	for name := range actual {
		if descriptorColumnByName(descriptor, name) == nil && !known[name] {
			return fmt.Errorf("unexpected physical column %s", name)
		}
	}
	ignored := map[string]bool{}
	for name := range known {
		if descriptorColumnByName(descriptor, name) == nil {
			ignored[name] = true
		}
	}
	if err := m.ensureIndexes(ctx, tx, descriptor); err != nil {
		return err
	}
	indexes, err := m.physicalIndexes(ctx, tx, descriptor.TableID)
	if err != nil {
		return err
	}
	if differences := compareIndexes(descriptor.Indexes, indexes, ignored); len(differences) > 0 {
		return errors.New(strings.Join(differences, "; "))
	}
	checks, err := m.physicalChecks(ctx, tx, descriptor.TableID)
	if err != nil {
		return err
	}
	if differences := compareChecks(m.status.Backend, descriptor, checks, ignored); len(differences) > 0 {
		return errors.New(strings.Join(differences, "; "))
	}
	return nil
}

func descriptorColumnByName(descriptor TableDescriptor, name string) *ColumnDescriptor {
	for index := range descriptor.Columns {
		if descriptor.Columns[index].Name == name {
			return &descriptor.Columns[index]
		}
	}
	return nil
}

func (m *Manager) catalogColumnNames(ctx context.Context, runner interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, tableID string) (map[string]bool, error) {
	rows, err := runner.QueryContext(ctx, `SELECT column_name FROM _8020_columns WHERE table_id = $1`, tableID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result[name] = true
	}
	return result, rows.Err()
}

func normalizePhysicalType(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func (m *Manager) physicalColumns(ctx context.Context, runner interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, tableID string) ([]PhysicalColumn, error) {
	if m.status.Backend == BackendSQLite {
		rows, err := runner.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(tableID)+")")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := []PhysicalColumn{}
		for rows.Next() {
			var sequence, required, primary int
			var name, dataType string
			var defaultValue sql.NullString
			if err := rows.Scan(&sequence, &name, &dataType, &required, &defaultValue, &primary); err != nil {
				return nil, err
			}
			result = append(result, PhysicalColumn{
				Name: name, Type: dataType, Nullable: required == 0 && primary == 0,
				Default: defaultValue.String, PrimaryKey: primary > 0, PrimaryKeyPosition: primary,
			})
		}
		primaryCount := 0
		for _, column := range result {
			if column.PrimaryKey {
				primaryCount++
			}
		}
		if primaryCount == 1 {
			for index := range result {
				if result[index].PrimaryKey && normalizePhysicalType(result[index].Type) == "integer" {
					result[index].Generated = true
				}
			}
		}
		return result, rows.Err()
	}
	rows, err := runner.QueryContext(ctx, `SELECT c.column_name, c.data_type, c.is_nullable, COALESCE(c.column_default, ''),
		COALESCE((SELECT kcu.ordinal_position FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu
		ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = current_schema() AND tc.table_name = c.table_name
		AND kcu.column_name = c.column_name LIMIT 1), 0), c.is_identity
		FROM information_schema.columns c WHERE c.table_schema = current_schema() AND c.table_name = $1 ORDER BY c.ordinal_position`, tableID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PhysicalColumn{}
	for rows.Next() {
		var column PhysicalColumn
		var nullable, identity string
		if err := rows.Scan(&column.Name, &column.Type, &nullable, &column.Default, &column.PrimaryKeyPosition, &identity); err != nil {
			return nil, err
		}
		column.Nullable = nullable == "YES"
		column.PrimaryKey = column.PrimaryKeyPosition > 0
		column.Generated = identity == "YES"
		result = append(result, column)
	}
	return result, rows.Err()
}

func (m *Manager) physicalChecks(ctx context.Context, runner interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, tableID string) ([]string, error) {
	if m.status.Backend == BackendSQLite {
		rows, err := runner.QueryContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = $1`, tableID)
		if err != nil {
			return nil, err
		}
		var statement sql.NullString
		if rows.Next() {
			if err := rows.Scan(&statement); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		result := []string{}
		for _, match := range sqliteNamedCheck.FindAllStringSubmatch(statement.String, -1) {
			if len(match) == 2 {
				result = append(result, match[1])
			}
		}
		sort.Strings(result)
		return result, nil
	}
	rows, err := runner.QueryContext(ctx, `SELECT constraint_data.conname
		FROM pg_constraint constraint_data
		JOIN pg_class table_data ON table_data.oid = constraint_data.conrelid
		JOIN pg_namespace namespace ON namespace.oid = table_data.relnamespace
		WHERE namespace.nspname = current_schema() AND table_data.relname = $1
		AND constraint_data.contype = 'c' ORDER BY constraint_data.conname`, tableID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	return result, rows.Err()
}

func (m *Manager) physicalIndexes(ctx context.Context, runner interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, tableID string) ([]PhysicalIndex, error) {
	if m.status.Backend == BackendSQLite {
		rows, err := runner.QueryContext(ctx, "PRAGMA index_list("+quoteIdentifier(tableID)+")")
		if err != nil {
			return nil, err
		}
		type listed struct {
			name   string
			unique bool
		}
		var indexes []listed
		for rows.Next() {
			var sequence, unique, partial int
			var name, origin string
			if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
				rows.Close()
				return nil, err
			}
			if origin == "c" {
				indexes = append(indexes, listed{name: name, unique: unique != 0})
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		result := make([]PhysicalIndex, 0, len(indexes))
		for _, index := range indexes {
			columns, err := runner.QueryContext(ctx, "PRAGMA index_info("+quoteIdentifier(index.name)+")")
			if err != nil {
				return nil, err
			}
			item := PhysicalIndex{Name: index.name, Unique: index.unique, Columns: []string{}}
			for columns.Next() {
				var sequence, columnSequence int
				var name string
				if err := columns.Scan(&sequence, &columnSequence, &name); err != nil {
					columns.Close()
					return nil, err
				}
				item.Columns = append(item.Columns, name)
			}
			if err := columns.Close(); err != nil {
				return nil, err
			}
			result = append(result, item)
		}
		return result, nil
	}
	rows, err := runner.QueryContext(ctx, `SELECT index_class.relname, index_data.indisunique,
		json_agg(attribute.attname ORDER BY key.ordinality)::text
		FROM pg_class table_class
		JOIN pg_namespace namespace ON namespace.oid = table_class.relnamespace
		JOIN pg_index index_data ON index_data.indrelid = table_class.oid
		JOIN pg_class index_class ON index_class.oid = index_data.indexrelid
		JOIN unnest(index_data.indkey) WITH ORDINALITY AS key(attribute_number, ordinality) ON true
		JOIN pg_attribute attribute ON attribute.attrelid = table_class.oid AND attribute.attnum = key.attribute_number
		LEFT JOIN pg_constraint constraint_data ON constraint_data.conindid = index_class.oid
		WHERE namespace.nspname = current_schema() AND table_class.relname = $1 AND constraint_data.oid IS NULL
		GROUP BY index_class.relname, index_data.indisunique ORDER BY index_class.relname`, tableID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PhysicalIndex{}
	for rows.Next() {
		var item PhysicalIndex
		var encoded string
		if err := rows.Scan(&item.Name, &item.Unique, &encoded); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &item.Columns); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func writeCatalogTable(ctx context.Context, tx *sql.Tx, evaluated EvaluatedTable) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `INSERT INTO _8020_tables
		(table_id, descriptor_hash, descriptor_json, source_package, source_commit, source_module, state, synchronization_state, synchronized_at, error)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', 'synchronized', $7, '')
		ON CONFLICT (table_id) DO UPDATE SET descriptor_hash = excluded.descriptor_hash, descriptor_json = excluded.descriptor_json,
		source_package = excluded.source_package, source_commit = excluded.source_commit, source_module = excluded.source_module,
		state = 'active', synchronization_state = 'synchronized', synchronized_at = excluded.synchronized_at, error = ''`,
		evaluated.Descriptor.TableID, evaluated.DescriptorHash, evaluated.DescriptorJSON, evaluated.SourcePackage, evaluated.SourceCommit, evaluated.SourceModule, now)
	if err != nil {
		return err
	}
	active := map[string]bool{}
	for _, column := range evaluated.Descriptor.Columns {
		active[column.Name] = true
		encoded, _ := json.Marshal(column)
		hash := sha256.Sum256(encoded)
		_, err := tx.ExecContext(ctx, `INSERT INTO _8020_columns
			(table_id, column_name, logical_type, definition_hash, definition_json, state)
			VALUES ($1, $2, $3, $4, $5, 'active')
			ON CONFLICT (table_id, column_name) DO UPDATE SET logical_type = excluded.logical_type,
			definition_hash = excluded.definition_hash, definition_json = excluded.definition_json, state = 'active'`,
			evaluated.Descriptor.TableID, column.Name, column.LogicalType, hex.EncodeToString(hash[:]), string(encoded))
		if err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT column_name FROM _8020_columns WHERE table_id = $1`, evaluated.Descriptor.TableID)
	if err != nil {
		return err
	}
	var retired []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !active[name] {
			retired = append(retired, name)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, name := range retired {
		if _, err := tx.ExecContext(ctx, `UPDATE _8020_columns SET state = 'retired' WHERE table_id = $1 AND column_name = $2`, evaluated.Descriptor.TableID, name); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM _8020_dependencies WHERE table_id = $1`, evaluated.Descriptor.TableID); err != nil {
		return err
	}
	for _, dependency := range evaluated.Dependencies {
		if _, err := tx.ExecContext(ctx, `INSERT INTO _8020_dependencies (table_id, module_path) VALUES ($1, $2)`, evaluated.Descriptor.TableID, dependency); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) retireTables(ctx context.Context, tableIDs []string) error {
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, tableID := range tableIDs {
		if !validTableID(tableID) || seen[tableID] {
			if !validTableID(tableID) {
				return fmt.Errorf("invalid retired table ID %q", tableID)
			}
			continue
		}
		seen[tableID] = true
		result, err := tx.ExecContext(ctx, `UPDATE _8020_tables SET state = 'retired', synchronization_state = 'retired', error = '' WHERE table_id = $1`, tableID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return fmt.Errorf("catalog table does not exist: %s", tableID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE _8020_columns SET state = 'retired' WHERE table_id = $1`, tableID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *Manager) retireMissingTables(ctx context.Context, seen map[string]bool, scopedPackages []string, full bool) error {
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return err
	}
	packages := map[string]bool{}
	for _, packageID := range scopedPackages {
		packages[packageID] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT table_id, source_package FROM _8020_tables WHERE state = 'active'`)
	if err != nil {
		return err
	}
	type candidate struct{ id, packageID string }
	var retired []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.packageID); err != nil {
			rows.Close()
			return err
		}
		if !seen[item.id] && (full || packages[item.packageID]) {
			retired = append(retired, item)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range retired {
		if _, err := tx.ExecContext(ctx, `UPDATE _8020_tables SET state = 'retired', synchronization_state = 'retired', error = '' WHERE table_id = $1`, item.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE _8020_columns SET state = 'retired' WHERE table_id = $1`, item.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListTables returns the database catalog, including retired definitions.
func (m *Manager) ListTables(ctx context.Context) ([]TableSummary, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT t.table_id, t.source_package, t.source_commit, t.source_module, t.state,
		t.synchronization_state, t.descriptor_hash, t.synchronized_at, t.error,
		SUM(CASE WHEN c.state = 'active' THEN 1 ELSE 0 END), SUM(CASE WHEN c.state = 'retired' THEN 1 ELSE 0 END)
		FROM _8020_tables t LEFT JOIN _8020_columns c ON c.table_id = t.table_id
		GROUP BY t.table_id, t.source_package, t.source_commit, t.source_module, t.state, t.synchronization_state,
		t.descriptor_hash, t.synchronized_at, t.error ORDER BY t.table_id`)
	if err != nil {
		return nil, err
	}
	result := []TableSummary{}
	sources := []TableSource{}
	catalogued := map[string]bool{}
	for rows.Next() {
		var table TableSummary
		if err := rows.Scan(&table.TableID, &table.SourcePackage, &table.SourceCommit, &table.SourceModule, &table.State,
			&table.SynchronizationState, &table.DescriptorHash, &table.SynchronizedAt, &table.Error,
			&table.ActiveColumns, &table.RetiredColumns); err != nil {
			rows.Close()
			return nil, err
		}
		catalogued[table.TableID] = true
		sources = append(sources, TableSource{
			TableID: table.TableID, SourcePackage: table.SourcePackage,
			SourceCommit: table.SourceCommit, SourceModule: table.SourceModule,
		})
		result = append(result, table)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	m.evaluatorMu.RLock()
	inspector := m.sourceInspector
	m.evaluatorMu.RUnlock()
	if inspector != nil && len(sources) > 0 {
		statuses, err := inspector(ctx, sources)
		if err != nil {
			return nil, err
		}
		for index := range result {
			status := statuses[result[index].TableID]
			result[index].CurrentSourceCommit = status.CurrentCommit
			switch {
			case status.Error != "":
				result[index].DefinitionState = "error"
				result[index].Error = joinStatusError(result[index].Error, status.Error)
			case !status.Exists:
				result[index].DefinitionState = "missing"
			case status.CurrentCommit != result[index].SourceCommit:
				result[index].DefinitionState = "commit_mismatch"
			default:
				result[index].DefinitionState = "present"
			}
		}
	}
	physical, err := m.physicalTableNames(ctx)
	if err != nil {
		return nil, err
	}
	physicalSet := make(map[string]bool, len(physical))
	for _, tableID := range physical {
		physicalSet[tableID] = true
		if catalogued[tableID] {
			continue
		}
		result = append(result, TableSummary{
			TableID: tableID, State: "uncatalogued", SynchronizationState: "uncatalogued",
			DefinitionState: "unknown", Error: "physical table is not recorded in the 80|20 catalog",
		})
	}
	for index := range result {
		if result[index].State == "uncatalogued" || physicalSet[result[index].TableID] {
			continue
		}
		result[index].SynchronizationState = "drift"
		result[index].Error = joinStatusError(result[index].Error, "physical table is missing")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TableID < result[j].TableID })
	return result, nil
}

func joinStatusError(current, next string) string {
	if current == "" {
		return next
	}
	if next == "" || strings.Contains(current, next) {
		return current
	}
	return current + "; " + next
}

func (m *Manager) physicalTableNames(ctx context.Context) ([]string, error) {
	statement := `SELECT name FROM sqlite_schema WHERE type = 'table'
		AND substr(name, 1, 7) <> 'sqlite_' AND substr(name, 1, 6) <> '_8020_' ORDER BY name`
	if m.status.Backend == BackendPostgreSQL {
		statement = `SELECT table_name FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
			AND left(table_name, 6) <> '_8020_' ORDER BY table_name`
	}
	rows, err := m.db.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	return result, rows.Err()
}

// ListDefinitions compares activated TypeScript definitions with the
// database-first catalog without mutating either side.
func (m *Manager) ListDefinitions(ctx context.Context) ([]DefinitionSummary, error) {
	definitions, err := m.EvaluateDefinitions(ctx, nil)
	if err != nil {
		return nil, err
	}
	catalog, err := m.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]TableSummary, len(catalog))
	for _, table := range catalog {
		byID[table.TableID] = table
	}
	seen := make(map[string]bool, len(definitions.Tables))
	result := make([]DefinitionSummary, 0, len(definitions.Tables))
	for _, table := range definitions.Tables {
		seen[table.Descriptor.TableID] = true
		stored, exists := byID[table.Descriptor.TableID]
		state := "new"
		if exists && stored.State == "active" && stored.DescriptorHash == table.DescriptorHash && stored.SourceCommit == table.SourceCommit {
			continue
		}
		if exists {
			state = "changed"
			if stored.State == "active" && stored.DescriptorHash == table.DescriptorHash {
				state = "source_commit_mismatch"
			}
		}
		result = append(result, DefinitionSummary{
			TableID: table.Descriptor.TableID, SourcePackage: table.SourcePackage,
			SourceCommit: table.SourceCommit, SourceModule: table.SourceModule,
			DescriptorHash: table.DescriptorHash, CatalogState: stored.State,
			CatalogHash: stored.DescriptorHash, Synchronization: state,
		})
	}
	for _, stored := range catalog {
		if stored.State != "active" || seen[stored.TableID] || stored.SourcePackage == "" {
			continue
		}
		result = append(result, DefinitionSummary{
			TableID: stored.TableID, SourcePackage: stored.SourcePackage,
			SourceCommit: stored.SourceCommit, SourceModule: stored.SourceModule,
			CatalogState: stored.State, CatalogHash: stored.DescriptorHash,
			Synchronization: "deleted",
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TableID < result[j].TableID })
	return result, nil
}

// SynchronizeDefinition evaluates and synchronizes one activated table.
func (m *Manager) SynchronizeDefinition(ctx context.Context, tableID, sourcePackage string) (SynchronizationResult, error) {
	if !validTableID(tableID) {
		return SynchronizationResult{}, errors.New("valid table ID is required")
	}
	source := TableSource{TableID: tableID, SourcePackage: strings.TrimSpace(sourcePackage)}
	err := m.db.QueryRowContext(ctx, `SELECT source_package, source_module FROM _8020_tables WHERE table_id = $1`, tableID).
		Scan(&source.SourcePackage, &source.SourceModule)
	if errors.Is(err, sql.ErrNoRows) {
		if source.SourcePackage == "" {
			return SynchronizationResult{}, errors.New("source package is required for a new table definition")
		}
	} else if err != nil {
		return SynchronizationResult{}, err
	}
	m.evaluatorMu.RLock()
	evaluator := m.sourceEvaluator
	m.evaluatorMu.RUnlock()
	if evaluator == nil {
		return SynchronizationResult{}, errors.New("database table evaluator is unavailable")
	}
	table, err := evaluator(ctx, source)
	if err != nil {
		return SynchronizationResult{}, err
	}
	if table == nil {
		return SynchronizationResult{}, fmt.Errorf("activated table definition not found: %s", tableID)
	}
	referenceErrors, err := m.validateReferences(ctx, []EvaluatedTable{*table})
	if err != nil {
		return SynchronizationResult{}, err
	}
	if referenceErr := referenceErrors[tableID]; referenceErr != nil {
		return SynchronizationResult{TableID: tableID, State: "error", Error: referenceErr.Error()}, referenceErr
	}
	state, syncErr := m.synchronizeTable(ctx, *table, false)
	result := SynchronizationResult{TableID: tableID, State: state}
	if syncErr != nil {
		result.Error = syncErr.Error()
	}
	return result, syncErr
}

// InspectTable returns catalog and physical state with drift differences.
func (m *Manager) InspectTable(ctx context.Context, tableID string) (TableDetail, error) {
	if tableID == "" || len(tableID) > 255 || strings.ContainsRune(tableID, '\x00') {
		return TableDetail{}, errors.New("valid table name is required")
	}
	var detail TableDetail
	err := m.db.QueryRowContext(ctx, `SELECT table_id, source_package, source_commit, source_module, state,
		synchronization_state, descriptor_hash, descriptor_json, synchronized_at, error FROM _8020_tables WHERE table_id = $1`, tableID).
		Scan(&detail.TableID, &detail.SourcePackage, &detail.SourceCommit, &detail.SourceModule, &detail.State,
			&detail.SynchronizationState, &detail.DescriptorHash, &detail.DescriptorJSON, &detail.SynchronizedAt, &detail.Error)
	if errors.Is(err, sql.ErrNoRows) {
		exists, existenceErr := m.physicalTableExists(ctx, tableID)
		if existenceErr != nil {
			return TableDetail{}, existenceErr
		}
		if !exists {
			return TableDetail{}, sql.ErrNoRows
		}
		detail.TableID = tableID
		detail.State = "uncatalogued"
		detail.SynchronizationState = "uncatalogued"
		detail.DefinitionState = "unknown"
		detail.Error = "physical table is not recorded in the 80|20 catalog"
		detail.Physical, err = m.physicalColumns(ctx, m.db, tableID)
		if err != nil {
			return TableDetail{}, err
		}
		detail.PhysicalIndexes, err = m.physicalIndexes(ctx, m.db, tableID)
		if err != nil {
			return TableDetail{}, err
		}
		detail.PhysicalChecks, err = m.physicalChecks(ctx, m.db, tableID)
		if err != nil {
			return TableDetail{}, err
		}
		detail.Differences = []string{"uncatalogued physical table"}
		return detail, nil
	}
	if err != nil {
		return TableDetail{}, err
	}
	if err := json.Unmarshal([]byte(detail.DescriptorJSON), &detail.Descriptor); err != nil {
		return TableDetail{}, err
	}
	rows, err := m.db.QueryContext(ctx, `SELECT table_id, column_name, logical_type, definition_hash, definition_json, state FROM _8020_columns WHERE table_id = $1 ORDER BY column_name`, tableID)
	if err != nil {
		return TableDetail{}, err
	}
	for rows.Next() {
		var column CatalogColumn
		if err := rows.Scan(&column.TableID, &column.ColumnName, &column.LogicalType, &column.DefinitionHash, &column.DefinitionJSON, &column.State); err != nil {
			rows.Close()
			return TableDetail{}, err
		}
		detail.Columns = append(detail.Columns, column)
		if column.State == "active" {
			detail.ActiveColumns++
		} else {
			detail.RetiredColumns++
		}
	}
	if err := rows.Close(); err != nil {
		return TableDetail{}, err
	}
	detail.Physical, err = m.physicalColumns(ctx, m.db, tableID)
	if err != nil {
		return TableDetail{}, err
	}
	detail.PhysicalIndexes, err = m.physicalIndexes(ctx, m.db, tableID)
	if err != nil {
		return TableDetail{}, err
	}
	detail.Differences = compareCatalog(detail)
	detail.Differences = append(detail.Differences, comparePhysical(m.status.Backend, detail.Descriptor, detail.Physical)...)
	retiredColumns := map[string]bool{}
	for _, column := range detail.Columns {
		if column.State == "retired" {
			retiredColumns[column.ColumnName] = true
		}
	}
	detail.Differences = append(detail.Differences, compareIndexes(detail.Descriptor.Indexes, detail.PhysicalIndexes, retiredColumns)...)
	detail.PhysicalChecks, err = m.physicalChecks(ctx, m.db, tableID)
	if err != nil {
		return TableDetail{}, err
	}
	detail.Differences = append(detail.Differences, compareChecks(m.status.Backend, detail.Descriptor, detail.PhysicalChecks, retiredColumns)...)
	detail.Differences = append(detail.Differences, compareRetiredPhysical(detail.Columns, detail.Descriptor, detail.Physical)...)
	m.evaluatorMu.RLock()
	sourceEvaluator := m.sourceEvaluator
	m.evaluatorMu.RUnlock()
	if sourceEvaluator != nil {
		current, sourceErr := sourceEvaluator(ctx, TableSource{
			TableID: detail.TableID, SourcePackage: detail.SourcePackage,
			SourceCommit: detail.SourceCommit, SourceModule: detail.SourceModule,
		})
		switch {
		case sourceErr != nil:
			detail.DefinitionState = "error"
			detail.Error = joinStatusError(detail.Error, sourceErr.Error())
			detail.Differences = append(detail.Differences, "activated definition is invalid: "+sourceErr.Error())
		case current == nil:
			detail.DefinitionState = "missing"
			detail.Differences = append(detail.Differences, "activated definition file is missing")
		default:
			detail.CurrentDescriptor = &current.Descriptor
			detail.CurrentDescriptorHash = current.DescriptorHash
			detail.CurrentSourceCommit = current.SourceCommit
			detail.DefinitionState = "present"
			if current.DescriptorHash != detail.DescriptorHash {
				detail.DefinitionState = "changed"
				detail.Differences = append(detail.Differences, "activated definition differs from deployed descriptor")
			} else if current.SourceCommit != detail.SourceCommit {
				detail.DefinitionState = "commit_mismatch"
				detail.Differences = append(detail.Differences, "activated package commit differs from catalog source commit")
			}
		}
	}
	sort.Strings(detail.Differences)
	if len(detail.Differences) > 0 && detail.State == "active" {
		detail.SynchronizationState = "drift"
	}
	return detail, nil
}

func compareRetiredPhysical(catalog []CatalogColumn, descriptor TableDescriptor, physical []PhysicalColumn) []string {
	known := map[string]string{}
	for _, column := range catalog {
		known[column.ColumnName] = column.State
	}
	actual := map[string]bool{}
	for _, column := range physical {
		actual[column.Name] = true
		if descriptorColumnByName(descriptor, column.Name) == nil && known[column.Name] == "" {
			known[column.Name] = "unexpected"
		}
	}
	var differences []string
	for name, state := range known {
		switch {
		case state == "retired" && !actual[name]:
			differences = append(differences, "retired physical column "+name+" is missing")
		case state == "unexpected":
			differences = append(differences, "unexpected physical column "+name)
		}
	}
	return differences
}

func (m *Manager) physicalTableExists(ctx context.Context, tableID string) (bool, error) {
	statement := `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = $1`
	if m.status.Backend == BackendPostgreSQL {
		statement = `SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name = $1`
	}
	var count int
	if err := m.db.QueryRowContext(ctx, statement, tableID).Scan(&count); err != nil {
		return false, err
	}
	return count != 0, nil
}

func compareCatalog(detail TableDetail) []string {
	var differences []string
	encoded, err := json.Marshal(detail.Descriptor)
	if err != nil {
		return []string{"stored descriptor is invalid"}
	}
	hash := sha256.Sum256(encoded)
	if hex.EncodeToString(hash[:]) != detail.DescriptorHash || string(encoded) != detail.DescriptorJSON {
		differences = append(differences, "stored table descriptor hash differs")
	}
	actual := make(map[string]CatalogColumn, len(detail.Columns))
	for _, column := range detail.Columns {
		if column.State == "active" {
			actual[column.ColumnName] = column
		}
	}
	for _, column := range detail.Descriptor.Columns {
		stored, exists := actual[column.Name]
		if !exists {
			differences = append(differences, "logical catalog column "+column.Name+" is missing")
			continue
		}
		columnJSON, _ := json.Marshal(column)
		columnHash := sha256.Sum256(columnJSON)
		if stored.LogicalType != column.LogicalType || stored.DefinitionJSON != string(columnJSON) || stored.DefinitionHash != hex.EncodeToString(columnHash[:]) {
			differences = append(differences, "logical catalog column "+column.Name+" differs")
		}
		delete(actual, column.Name)
	}
	for name := range actual {
		differences = append(differences, "unexpected active logical catalog column "+name)
	}
	sort.Strings(differences)
	return differences
}

func compareIndexes(expected []IndexDescriptor, physical []PhysicalIndex, ignoredColumns map[string]bool) []string {
	actual := make(map[string]PhysicalIndex, len(physical))
	for _, index := range physical {
		actual[index.Name] = index
	}
	var differences []string
	for _, index := range expected {
		found, exists := actual[index.Name]
		if !exists {
			differences = append(differences, "missing index "+index.Name)
			continue
		}
		if found.Unique != index.Unique || strings.Join(found.Columns, "\x00") != strings.Join(index.Columns, "\x00") {
			differences = append(differences, "index "+index.Name+" definition differs")
		}
		delete(actual, index.Name)
	}
	for name := range actual {
		ignore := len(actual[name].Columns) > 0
		for _, column := range actual[name].Columns {
			ignore = ignore && ignoredColumns[column]
		}
		if ignore {
			continue
		}
		differences = append(differences, "unexpected index "+name)
	}
	sort.Strings(differences)
	return differences
}

func comparePhysical(backend string, descriptor TableDescriptor, physical []PhysicalColumn) []string {
	actual := map[string]PhysicalColumn{}
	for _, column := range physical {
		actual[column.Name] = column
	}
	var differences []string
	for _, expected := range descriptor.Columns {
		column, exists := actual[expected.Name]
		if !exists {
			differences = append(differences, "missing column "+expected.Name)
			continue
		}
		differences = append(differences, comparePhysicalColumn(backend, expected, column)...)
		if expected.PrimaryKey {
			wanted := 0
			for keyPosition, key := range descriptor.PrimaryKey {
				if key == expected.Name {
					wanted = keyPosition + 1
					break
				}
			}
			if column.PrimaryKeyPosition != wanted {
				differences = append(differences, fmt.Sprintf("column %s primary-key position is %d; expected %d", expected.Name, column.PrimaryKeyPosition, wanted))
			}
		}
	}
	return differences
}

func comparePhysicalColumn(backend string, expected ColumnDescriptor, column PhysicalColumn) []string {
	var differences []string
	wantType, err := physicalType(backend, expected)
	if err != nil {
		return []string{err.Error()}
	}
	if normalizePhysicalType(column.Type) != normalizePhysicalType(wantType) {
		differences = append(differences, fmt.Sprintf("column %s type is %s; expected %s", expected.Name, column.Type, wantType))
	}
	if column.Nullable != expected.Nullable && !expected.PrimaryKey {
		differences = append(differences, "column "+expected.Name+" nullability differs")
	}
	if column.PrimaryKey != expected.PrimaryKey {
		differences = append(differences, "column "+expected.Name+" primary key differs")
	}
	if expected.Generated && !column.Generated {
		differences = append(differences, "column "+expected.Name+" generated identity is missing")
	}
	if backend == BackendPostgreSQL && !expected.Generated && column.Generated {
		differences = append(differences, "column "+expected.Name+" has an unexpected generated identity")
	}
	if !defaultMatches(backend, expected, column.Default) {
		differences = append(differences, "column "+expected.Name+" default differs")
	}
	return differences
}

func defaultMatches(backend string, column ColumnDescriptor, actual string) bool {
	if column.Generated {
		return true
	}
	if column.Default == nil {
		return strings.TrimSpace(actual) == ""
	}
	expected, err := defaultSQLForBackend(backend, column)
	if err != nil || strings.TrimSpace(actual) == "" {
		return false
	}
	want, found := normalizeDefault(expected), normalizeDefault(actual)
	if want == found || (backend == BackendPostgreSQL && strings.HasPrefix(found, want+"::")) {
		return true
	}
	if column.Default.Kind == "now" {
		return strings.Contains(found, "current_timestamp") || strings.Contains(found, "now()") ||
			(backend == BackendSQLite && strings.Contains(found, "strftime(") && strings.Contains(found, "'now'"))
	}
	return false
}

func normalizeDefault(value string) string {
	value = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
	for len(value) >= 2 && value[0] == '(' && value[len(value)-1] == ')' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func compareChecks(backend string, descriptor TableDescriptor, physical []string, ignoredColumns map[string]bool) []string {
	actual := make(map[string]bool, len(physical))
	for _, name := range physical {
		actual[name] = true
	}
	var differences []string
	for _, column := range descriptor.Columns {
		if checkExpression(backend, column) == "" {
			continue
		}
		name := checkName(descriptor.TableID, column.Name)
		if !actual[name] {
			differences = append(differences, "missing check constraint "+name)
		}
		delete(actual, name)
	}
	for name := range actual {
		ignored := false
		for column := range ignoredColumns {
			if name == checkName(descriptor.TableID, column) {
				ignored = true
				break
			}
		}
		if ignored {
			continue
		}
		differences = append(differences, "unexpected check constraint "+name)
	}
	sort.Strings(differences)
	return differences
}

// Trim permanently removes explicitly selected retired database objects.
func (m *Manager) Trim(ctx context.Context, tableID string, columns []string, dropTable bool) error {
	if !validTableID(tableID) {
		return errors.New("valid table ID is required")
	}
	m.schemaMu.Lock()
	defer m.schemaMu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.lockSchema(ctx, tx); err != nil {
		return err
	}
	if dropTable {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM _8020_tables WHERE table_id = $1`, tableID).Scan(&state); err != nil {
			return err
		}
		if state != "retired" {
			return errors.New("only retired tables may be trimmed")
		}
		if _, err := tx.ExecContext(ctx, "DROP TABLE "+quoteIdentifier(tableID)); err != nil {
			return err
		}
		for _, statement := range []string{`DELETE FROM _8020_dependencies WHERE table_id = $1`, `DELETE FROM _8020_columns WHERE table_id = $1`, `DELETE FROM _8020_tables WHERE table_id = $1`} {
			if _, err := tx.ExecContext(ctx, statement, tableID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	for _, column := range columns {
		if !validColumnName(column) {
			return fmt.Errorf("invalid column %q", column)
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM _8020_columns WHERE table_id = $1 AND column_name = $2`, tableID, column).Scan(&state); err != nil {
			return err
		}
		if state != "retired" {
			return fmt.Errorf("column %s is not retired", column)
		}
		if m.status.Backend == BackendSQLite {
			indexes, err := m.physicalIndexes(ctx, tx, tableID)
			if err != nil {
				return err
			}
			for _, index := range indexes {
				if containsString(index.Columns, column) {
					if _, err := tx.ExecContext(ctx, "DROP INDEX "+quoteIdentifier(index.Name)); err != nil {
						return fmt.Errorf("drop retired column index %s: %w", index.Name, err)
					}
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+quoteIdentifier(tableID)+" DROP COLUMN "+quoteIdentifier(column)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM _8020_columns WHERE table_id = $1 AND column_name = $2`, tableID, column); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
func quoteLiteral(value string) string    { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }

// CanonicalTableID applies the one portable table-name normalization rule.
func CanonicalTableID(namespace, packageName, tableName string) (string, error) {
	parts := []string{normalizeIdentity(namespace), normalizeIdentity(packageName), normalizeIdentity(tableName)}
	for _, part := range parts {
		if part == "" {
			return "", errors.New("table identity components must contain an ASCII letter or digit")
		}
	}
	full := strings.Join(parts, "__")
	if len(full) <= 63 {
		return full, nil
	}
	hash := sha256.Sum256([]byte(full))
	return full[:52] + "_" + hex.EncodeToString(hash[:])[:10], nil
}

func normalizeIdentity(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	separator := false
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			result.WriteRune(character)
			separator = false
		} else if result.Len() > 0 && !separator {
			result.WriteByte('_')
			separator = true
		}
	}
	return strings.Trim(result.String(), "_")
}
