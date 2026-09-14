// Database authority and package-owned schema delegation.
package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"the8020/kernel/identity"
	"time"
)

const postgresSchemaLock int64 = 802020260901

type deploymentLockContextKey struct{}
type activationLockContextKey struct {
	manager *Manager
	id      string
}

// TableDescriptor preserves package-owned metadata without interpreting its types.
// Only the canonical table identity participates in native source containment.
type TableDescriptor struct {
	TableID string
	encoded json.RawMessage
}

func (d *TableDescriptor) UnmarshalJSON(encoded []byte) error {
	var identity struct {
		TableID string `json:"table_id"`
	}
	if err := json.Unmarshal(encoded, &identity); err != nil {
		return err
	}
	if identity.TableID == "" {
		return errors.New("schema descriptor requires a table identity")
	}
	d.TableID, d.encoded = identity.TableID, append(d.encoded[:0], encoded...)
	return nil
}
func (d TableDescriptor) MarshalJSON() ([]byte, error) {
	if len(d.encoded) == 0 {
		return []byte("null"), nil
	}
	return d.encoded, nil
}

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
	ID                      string
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

type TableSummary struct {
	TableID              string `json:"table_id"`
	SourcePackage        string `json:"source_package"`
	SourceCommit         string `json:"source_commit"`
	SourceModule         string `json:"source_module"`
	State                string `json:"state"`
	SynchronizationState string `json:"synchronization_state"`
	DescriptorHash       string `json:"descriptor_hash"`
	SynchronizedAt       string `json:"synchronized_at,omitempty"`
	ActiveColumns        int    `json:"active_columns"`
	RetiredColumns       int    `json:"retired_columns"`
	Error                string `json:"error,omitempty"`
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
	DefinitionState       string           `json:"definition_state,omitempty"`
	CurrentSourceCommit   string           `json:"current_source_commit,omitempty"`
	Columns               []CatalogColumn  `json:"columns"`
	Physical              []PhysicalColumn `json:"physical_columns"`
	PhysicalIndexes       []PhysicalIndex  `json:"physical_indexes"`
	PhysicalChecks        []string         `json:"physical_checks"`
	Differences           []string         `json:"differences"`
}

type CatalogColumn struct {
	TableID        string `json:"table_id"`
	ColumnName     string `json:"column_name"`
	Ordinal        int    `json:"ordinal"`
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

type TableSource struct {
	TableID       string
	SourcePackage string
	SourceCommit  string
	SourceModule  string
}

type SynchronizationOptions struct {
	Full                    bool
	Recovery                bool
	SkipReferenceValidation bool
	RetireMissingPackages   []string
	RetireTables            []string
	PackageCommits          map[string]string
}

type DefinitionEvaluator func(context.Context, []string) (DefinitionSet, error)
type FullSynchronizer func(context.Context, bool) ([]SynchronizationResult, error)
type SourceEvaluator func(context.Context, TableSource) (*EvaluatedTable, error)

// SchemaExecutor invokes the db package through the ordinary job runtime.
type SchemaRequest struct {
	Operation           string `json:"operation"`
	Input               any    `json:"input"`
	PublicationLockHeld bool   `json:"publication_lock_held"`
}
type SchemaExecutor func(context.Context, SchemaRequest) (json.RawMessage, error)

func (m *Manager) SetSchemaExecutor(executor SchemaExecutor) {
	m.evaluatorMu.Lock()
	m.schemaExecutor = executor
	m.evaluatorMu.Unlock()
}
func (m *Manager) schemaCall(ctx context.Context, operation string, input any, target any) error {
	m.evaluatorMu.RLock()
	executor := m.schemaExecutor
	m.evaluatorMu.RUnlock()
	if executor == nil {
		return errors.New("database schema package is unavailable; native SQL remains available for repair")
	}
	encoded, err := executor(ctx, SchemaRequest{Operation: operation, Input: input, PublicationLockHeld: ctx.Value(deploymentLockContextKey{}) == m})
	if err != nil {
		return err
	}
	var response struct {
		Value  json.RawMessage `json:"value"`
		Error  string          `json:"error"`
		Status json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		return fmt.Errorf("decode database schema result: %w", err)
	}
	if len(response.Value) == 0 {
		return errors.New("database schema operation returned no result envelope")
	}
	if target != nil && len(response.Value) > 0 {
		if err := json.Unmarshal(response.Value, target); err != nil {
			return err
		}
	}
	if response.Error != "" {
		if response.Error == sql.ErrNoRows.Error() {
			return sql.ErrNoRows
		}
		return errors.New(response.Error)
	}
	if len(response.Status) > 0 && string(response.Status) != "null" {
		// The package publishes schema readiness; physical pool fields stay native.
		var status struct {
			State               string `json:"state"`
			CatalogVersion      int    `json:"catalog_version"`
			Initialized         bool   `json:"initialized"`
			PendingDeployment   bool   `json:"pending_deployment"`
			PackageSetHash      string `json:"package_set_hash"`
			DescriptorSetHash   string `json:"descriptor_set_hash"`
			InitializedAt       string `json:"initialized_at"`
			CatalogError        string `json:"catalog_error"`
			LastDeploymentAt    string `json:"last_deployment_at"`
			LastDeploymentError string `json:"last_deployment_error"`
		}
		if err := json.Unmarshal(response.Status, &status); err != nil {
			return err
		}
		m.statusMu.Lock()
		m.status.State, m.status.CatalogVersion = status.State, status.CatalogVersion
		m.status.Initialized, m.status.PendingDeployment = status.Initialized, status.PendingDeployment
		m.status.PackageSetHash, m.status.DescriptorSetHash = status.PackageSetHash, status.DescriptorSetHash
		m.status.InitializedAt, m.status.CatalogError = status.InitializedAt, status.CatalogError
		m.status.LastDeploymentAt, m.status.LastDeploymentError = status.LastDeploymentAt, status.LastDeploymentError
		m.status.Error = ""
		m.statusMu.Unlock()
	}
	return nil
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

func (m *Manager) AcquireDeploymentLock(ctx context.Context) (context.Context, func(), error) {
	if ctx.Value(deploymentLockContextKey{}) == m {
		return ctx, func() {}, nil
	}
	m.deploymentMu.Lock()
	if m.status.Backend != BackendPostgreSQL {
		return context.WithValue(ctx, deploymentLockContextKey{}, m), sync.OnceFunc(m.deploymentMu.Unlock), nil
	}
	connection, err := m.db.Conn(ctx)
	if err != nil {
		m.deploymentMu.Unlock()
		return ctx, nil, err
	}
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, postgresSchemaLock); err != nil {
		_ = connection.Raw(func(any) error { return driver.ErrBadConn })
		connection.Close()
		m.deploymentMu.Unlock()
		return ctx, nil, err
	}
	return context.WithValue(ctx, deploymentLockContextKey{}, m), sync.OnceFunc(func() {
		releaseAdvisoryLock(connection, postgresSchemaLock)
		m.deploymentMu.Unlock()
	}), nil
}

func (m *Manager) AcquireActivationLock(ctx context.Context, id string) (context.Context, func(), error) {
	if !identity.Is(id, "act") {
		return ctx, nil, errors.New("invalid activation identity")
	}
	key := activationLockContextKey{manager: m, id: id}
	if ctx.Value(key) == true {
		return ctx, func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if _, busy := m.activationOperations.LoadOrStore(id, true); busy {
		return ctx, nil, fmt.Errorf("activation %s is already executing", id)
	}
	release := func() { m.activationOperations.Delete(id) }
	if m.status.Backend == BackendPostgreSQL {
		connection, err := m.db.Conn(ctx)
		if err != nil {
			release()
			return ctx, nil, err
		}
		hash := fnv.New64a()
		_, _ = hash.Write([]byte("the8020:activation:" + id))
		lockID := int64(hash.Sum64())
		var acquired bool
		err = connection.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&acquired)
		if err != nil || !acquired {
			if err != nil {
				_ = connection.Raw(func(any) error { return driver.ErrBadConn })
			}
			_ = connection.Close()
			release()
			if err != nil {
				return ctx, nil, err
			}
			return ctx, nil, fmt.Errorf("activation %s is already executing", id)
		}
		release = func() {
			releaseAdvisoryLock(connection, lockID)
			m.activationOperations.Delete(id)
		}
	}
	return context.WithValue(ctx, key, true), sync.OnceFunc(release), nil
}

func releaseAdvisoryLock(connection *sql.Conn, id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, id); err != nil {
		// An uncertain unlock must never return a session lock to the pool.
		_ = connection.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = connection.Close()
}

func PackageSetHash(packages map[string]string) string {
	entries := make([]string, 0, len(packages))
	for packageID, commit := range packages {
		entries = append(entries, packageID+"="+commit)
	}
	sort.Strings(entries)
	digest := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(digest[:])
}

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
	return full[:56] + "_" + hex.EncodeToString(hash[:])[:6], nil
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

func (m *Manager) setCatalogFailure(err error) {
	m.statusMu.Lock()
	m.status.State = StateInitializationFailed
	m.status.Error = err.Error()
	m.status.CatalogError = err.Error()
	m.statusMu.Unlock()
}

func (m *Manager) BeginInitialization(ctx context.Context) error {
	if err := m.schemaCall(ctx, "beginInitialization", nil, nil); err != nil {
		return err
	}
	m.statusMu.Lock()
	m.status.State = StateInitializing
	m.status.Error = ""
	m.status.CatalogError = ""
	m.statusMu.Unlock()
	return nil
}

func (m *Manager) InitializeCatalog(ctx context.Context) (Status, error) {
	if _, err := m.Check(ctx); err != nil {
		return m.Status(), err
	}
	err := m.schemaCall(ctx, "initialize", nil, nil)
	if err != nil {
		m.setCatalogFailure(err)
	}
	return m.Status(), err
}
func (m *Manager) CompleteInitialization(ctx context.Context, commits map[string]string) error {
	return m.schemaCall(ctx, "completeInitialization", map[string]any{"commits": commits}, nil)
}
func (m *Manager) SetInitializationFailure(ctx context.Context, failure error) {
	if failure == nil {
		return
	}
	_ = m.schemaCall(ctx, "failure", map[string]any{"error": failure.Error()}, nil)
	m.setCatalogFailure(failure)
}
func (m *Manager) BeginDeployment(ctx context.Context, id string, candidates []DeploymentCandidate) (PendingDeployment, error) {
	var result PendingDeployment
	err := m.schemaCall(ctx, "beginDeployment", map[string]any{"id": id, "candidates": candidates}, &result)
	return result, err
}
func (m *Manager) CompleteDeployment(ctx context.Context, id string, activated bool) error {
	return m.schemaCall(ctx, "completeDeployment", map[string]any{"id": id, "activated": activated}, nil)
}
func (m *Manager) PendingDeployment(ctx context.Context) (PendingDeployment, bool, error) {
	return m.PendingDeploymentFor(ctx, "")
}
func (m *Manager) PendingDeploymentFor(ctx context.Context, id string) (PendingDeployment, bool, error) {
	if id != "" && !identity.Is(id, "act") {
		return PendingDeployment{}, false, errors.New("invalid deployment identity")
	}
	var result *PendingDeployment
	err := m.schemaCall(ctx, "pending", map[string]any{"id": id}, &result)
	if result == nil {
		return PendingDeployment{}, false, err
	}
	return *result, true, err
}
func (m *Manager) UpdatePendingDeployment(ctx context.Context, id, stage string, failure error) error {
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	return m.schemaCall(ctx, "updatePending", map[string]any{"id": id, "stage": stage, "error": message}, nil)
}
func (m *Manager) CatalogState(ctx context.Context) (CatalogState, error) {
	var result CatalogState
	err := m.schemaCall(ctx, "catalogState", nil, &result)
	return result, err
}
func (m *Manager) TableSourcesForPackages(ctx context.Context, packages []string) ([]TableSource, error) {
	var result []TableSource
	err := m.schemaCall(ctx, "sources", map[string]any{"values": packages}, &result)
	return result, err
}
func (m *Manager) TableSourcesForDependencies(ctx context.Context, modules []string) ([]TableSource, error) {
	var result []TableSource
	err := m.schemaCall(ctx, "sources", map[string]any{"values": modules, "dependencies": true}, &result)
	return result, err
}
func (m *Manager) CompletedTableIDs(ctx context.Context, commits map[string]string) (map[string]bool, error) {
	var result map[string]bool
	err := m.schemaCall(ctx, "completed", map[string]any{"commits": commits}, &result)
	return result, err
}
func (m *Manager) Synchronize(ctx context.Context, tables []EvaluatedTable, options SynchronizationOptions) ([]SynchronizationResult, error) {
	var result []SynchronizationResult
	err := m.schemaCall(ctx, "synchronize", map[string]any{"tables": tables, "options": options}, &result)
	return result, err
}
func (m *Manager) FinalizeFullSynchronization(ctx context.Context, ids []string, commits map[string]string) error {
	return m.schemaCall(ctx, "finalize", map[string]any{"ids": ids, "commits": commits}, nil)
}
func (m *Manager) ValidateCatalogReferences(ctx context.Context) error {
	return m.schemaCall(ctx, "references", nil, nil)
}
func (m *Manager) ListTables(ctx context.Context) ([]TableSummary, error) {
	var result []TableSummary
	err := m.schemaCall(ctx, "list", nil, &result)
	return result, err
}
func (m *Manager) ListDefinitions(ctx context.Context) ([]DefinitionSummary, error) {
	definitions, err := m.EvaluateDefinitions(ctx, nil)
	if err != nil {
		return nil, err
	}
	var result []DefinitionSummary
	err = m.schemaCall(ctx, "definitions", map[string]any{"tables": definitions.Tables}, &result)
	return result, err
}
func (m *Manager) SynchronizeDefinition(ctx context.Context, id, sourcePackage string) (SynchronizationResult, error) {
	var source TableSource
	if err := m.schemaCall(ctx, "source", map[string]any{"id": id, "package": sourcePackage}, &source); err != nil {
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
		return SynchronizationResult{}, fmt.Errorf("activated table definition not found: %s", id)
	}
	results, err := m.Synchronize(ctx, []EvaluatedTable{*table}, SynchronizationOptions{})
	if len(results) == 0 {
		return SynchronizationResult{}, err
	}
	return results[0], err
}
func (m *Manager) InspectTable(ctx context.Context, id string) (TableDetail, error) {
	var result TableDetail
	err := m.schemaCall(ctx, "inspect", map[string]any{"id": id}, &result)
	return result, err
}
func (m *Manager) CompareTable(ctx context.Context, id string) (TableDetail, error) {
	detail, err := m.InspectTable(ctx, id)
	if err != nil {
		return TableDetail{}, err
	}
	var current *EvaluatedTable
	sourceError := ""
	if detail.SourcePackage != "" {
		m.evaluatorMu.RLock()
		evaluator := m.sourceEvaluator
		m.evaluatorMu.RUnlock()
		if evaluator == nil {
			return TableDetail{}, errors.New("database table evaluator is unavailable")
		}
		current, err = evaluator(ctx, TableSource{TableID: id, SourcePackage: detail.SourcePackage, SourceCommit: detail.SourceCommit, SourceModule: detail.SourceModule})
		if err != nil {
			sourceError = err.Error()
		}
	}
	err = m.schemaCall(ctx, "compare", map[string]any{"detail": detail, "current": current, "error": sourceError}, &detail)
	return detail, err
}
func (m *Manager) Trim(ctx context.Context, id string, columns []string, drop bool) error {
	return m.schemaCall(ctx, "trim", map[string]any{"id": id, "columns": columns, "drop": drop}, nil)
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
