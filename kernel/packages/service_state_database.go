package packages

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"the8020/kernel/database"
)

const (
	servicesTable         = `"the8020__services__services"`
	serviceOverridesTable = `"the8020__services__overrides"`
	serviceVersionsTable  = `"the8020__services__versions"`
)

// DatabaseServiceStateStore persists desired service enablement and explicit
// operator overrides. Package declarations and immutable version snapshots are
// synchronized separately during activation.
type DatabaseServiceStateStore struct {
	database database.Store
	locks    sync.Map
}

type PackageServiceState struct {
	ServiceID string
	Active    bool
}

// PackageServices returns the small affected-service set used after one
// package revision changes. It deliberately includes retired declarations so
// each node can remove obsolete local runtime capacity.
func (s *DatabaseServiceStateStore) PackageServices(ctx context.Context, packageID string) ([]PackageServiceState, error) {
	if _, err := ParsePackageID(packageID); err != nil {
		return nil, err
	}
	rows, err := s.database.QueryContext(ctx, `SELECT "serviceId", "active" FROM `+servicesTable+` WHERE "packageId" = $1 ORDER BY "serviceId"`, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PackageServiceState{}
	for rows.Next() {
		var item PackageServiceState
		if err := rows.Scan(&item.ServiceID, &item.Active); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func NewDatabaseServiceStateStore(store database.Store) (*DatabaseServiceStateStore, error) {
	if store == nil {
		return nil, errors.New("database is required")
	}
	return &DatabaseServiceStateStore{database: store}, nil
}

func (s *DatabaseServiceStateStore) InstallDefinition(ctx context.Context, definition Definition, state DesiredServiceState, effective EffectiveConfiguration, packageCommit string) error {
	return s.writeDefinition(ctx, definition, state, effective, packageCommit, false)
}

// UpdateDesiredDefinition is the operator-mutation path. Package activation
// uses InstallDefinition because its existing package revision publishes the
// affected service set only after source and schema activation is complete.
func (s *DatabaseServiceStateStore) UpdateDesiredDefinition(ctx context.Context, definition Definition, state DesiredServiceState, effective EffectiveConfiguration, packageCommit string) error {
	return s.writeDefinition(ctx, definition, state, effective, packageCommit, true)
}

func (s *DatabaseServiceStateStore) writeDefinition(ctx context.Context, definition Definition, state DesiredServiceState, effective EffectiveConfiguration, packageCommit string, publish bool) error {
	encoded, err := json.Marshal(definition.Service)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(encoded)
	manifestHash := hex.EncodeToString(hash[:])
	if packageCommit == "" {
		packageCommit = "local:" + manifestHash
	}
	policy, err := json.Marshal(struct {
		Lifecycle LifecycleConfiguration `json:"lifecycle"`
		Scaling   ScalingConfiguration   `json:"scaling"`
		Placement PlacementConfiguration `json:"placement"`
	}{effective.Lifecycle, effective.Scaling, effective.Placement})
	if err != nil {
		return err
	}
	policyDigest := sha256.Sum256(policy)
	policyHash := hex.EncodeToString(policyDigest[:])
	now := database.EncodeTime(s.database, time.Now())
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	serviceID := definition.Identity.ServiceID()
	if err := writeServiceOverrides(ctx, tx, serviceID, state, now); err != nil {
		return err
	}
	var currentCommit, currentManifest, currentPolicy string
	var currentVersion int64
	err = tx.QueryRowContext(ctx, `SELECT s."packageCommit", s."manifestHash", s."desiredVersion", v."policyHash"
		FROM `+servicesTable+` s JOIN `+serviceVersionsTable+` v
		ON v."serviceId" = s."serviceId" AND v."version" = s."desiredVersion"
		WHERE s."serviceId" = $1`, serviceID).Scan(&currentCommit, &currentManifest, &currentVersion, &currentPolicy)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	desiredVersion := int64(state.Generation)
	if exists {
		desiredVersion = currentVersion
		if int64(state.Generation) > currentVersion {
			desiredVersion = int64(state.Generation)
		} else if currentCommit != packageCommit || currentManifest != manifestHash || currentPolicy != policyHash {
			desiredVersion++
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+servicesTable+` (
		"serviceId", "packageId", "packageCommit", "manifestHash", "description", "entrypoint",
		"accessMode", "unauthenticatedAction", "unauthenticatedStatus", "unauthenticatedMessage",
		"unauthenticatedRedirectUrl", "declaredServiceType", "declaredSessionKeepAliveMs",
		"declaredMinimumWorkers", "declaredMaximumWorkers", "declaredConcurrencyPerWorker",
		"declaredTargetUtilization", "declaredWorkerKeepAliveMs", "declaredSandboxGroup",
		"declaredMinimumSandboxes", "declaredWorkersPerSandbox", "enabled", "active",
		"desiredVersion", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $25)
		ON CONFLICT ("serviceId") DO UPDATE SET
		"packageId" = excluded."packageId", "packageCommit" = excluded."packageCommit",
		"manifestHash" = excluded."manifestHash", "description" = excluded."description",
		"entrypoint" = excluded."entrypoint", "accessMode" = excluded."accessMode",
		"unauthenticatedAction" = excluded."unauthenticatedAction",
		"unauthenticatedStatus" = excluded."unauthenticatedStatus",
		"unauthenticatedMessage" = excluded."unauthenticatedMessage",
		"unauthenticatedRedirectUrl" = excluded."unauthenticatedRedirectUrl",
		"declaredServiceType" = excluded."declaredServiceType",
		"declaredSessionKeepAliveMs" = excluded."declaredSessionKeepAliveMs",
		"declaredMinimumWorkers" = excluded."declaredMinimumWorkers",
		"declaredMaximumWorkers" = excluded."declaredMaximumWorkers",
		"declaredConcurrencyPerWorker" = excluded."declaredConcurrencyPerWorker",
		"declaredTargetUtilization" = excluded."declaredTargetUtilization",
		"declaredWorkerKeepAliveMs" = excluded."declaredWorkerKeepAliveMs",
		"declaredSandboxGroup" = excluded."declaredSandboxGroup",
		"declaredMinimumSandboxes" = excluded."declaredMinimumSandboxes",
		"declaredWorkersPerSandbox" = excluded."declaredWorkersPerSandbox",
		"enabled" = excluded."enabled", "active" = excluded."active",
		"desiredVersion" = excluded."desiredVersion", "updatedAt" = excluded."updatedAt"`,
		serviceID, definition.Identity.PackageID(), packageCommit, manifestHash,
		definition.Service.Description, definition.Service.Entrypoint, definition.Service.Access.Mode,
		definition.Service.Access.Unauthenticated.Action, definition.Service.Access.Unauthenticated.Status,
		definition.Service.Access.Unauthenticated.Message, definition.Service.Access.Unauthenticated.RedirectURL,
		definition.Service.Lifecycle.ServiceType, manifestDuration(definition.Service.Lifecycle.SessionKeepAlive),
		nullableManifestInt(definition.Service.Scaling.MinimumWorkers), nullableManifestInt(definition.Service.Scaling.MaximumWorkers),
		nullableManifestInt(definition.Service.Scaling.ConcurrencyPerWorker), nullableManifestFloat(definition.Service.Scaling.TargetUtilization),
		manifestDuration(definition.Service.Scaling.WorkerKeepAlive), nullableText(definition.Service.Placement.SandboxGroup),
		nullableManifestInt(definition.Service.Placement.MinimumSandboxes), nullableManifestInt(definition.Service.Placement.WorkersPerSandbox),
		state.Enabled, true, desiredVersion, now)
	if err != nil {
		return err
	}
	if exists && desiredVersion == currentVersion {
		if publish {
			if err := publishServiceChange(ctx, tx, serviceID, now); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+serviceVersionsTable+` (
		"serviceId", "version", "packageCommit", "manifestHash", "policyHash", "serviceType",
		"sessionKeepAliveMs", "minimumWorkers", "maximumWorkers", "concurrencyPerWorker",
		"targetUtilization", "workerKeepAliveMs", "sandboxGroup", "minimumSandboxes",
		"workersPerSandbox", "createdAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		serviceID, desiredVersion, packageCommit, manifestHash, policyHash,
		effective.Lifecycle.ServiceType, effective.Lifecycle.SessionKeepAlive.Milliseconds(),
		effective.Scaling.MinimumWorkers, effective.Scaling.MaximumWorkers,
		effective.Scaling.ConcurrencyPerWorker, effective.Scaling.TargetUtilization,
		effective.Scaling.WorkerKeepAlive.Milliseconds(), effective.Placement.SandboxGroup,
		effective.Placement.MinimumSandboxes, effective.Placement.WorkersPerSandbox, now)
	if err != nil {
		return err
	}
	if publish {
		if err := publishServiceChange(ctx, tx, serviceID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RetirePackage marks definitions absent from the newly activated package as
// obsolete without deleting operator history or immutable version snapshots.
func (s *DatabaseServiceStateStore) RetirePackage(ctx context.Context, packageID string, activeServiceIDs []string) error {
	active := make(map[string]bool, len(activeServiceIDs))
	for _, serviceID := range activeServiceIDs {
		active[serviceID] = true
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT "serviceId" FROM `+servicesTable+` WHERE "packageId" = $1 AND "active"`, packageID)
	if err != nil {
		return err
	}
	var retired []string
	for rows.Next() {
		var serviceID string
		if err := rows.Scan(&serviceID); err != nil {
			rows.Close()
			return err
		}
		if !active[serviceID] {
			retired = append(retired, serviceID)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	now := database.EncodeTime(s.database, time.Now())
	for _, serviceID := range retired {
		if _, err := tx.ExecContext(ctx, `UPDATE `+servicesTable+` SET "active" = $1, "enabled" = $1, "updatedAt" = $2 WHERE "serviceId" = $3`, false, now, serviceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *DatabaseServiceStateStore) Get(serviceID string) (DesiredServiceState, bool, error) {
	if _, err := ParseServiceID(serviceID); err != nil {
		return DesiredServiceState{}, false, err
	}
	row := s.database.QueryRowContext(context.Background(), `SELECT s."enabled", s."desiredVersion",
		o."serviceType", o."sessionKeepAliveMs", o."minimumWorkers", o."maximumWorkers",
		o."concurrencyPerWorker", o."targetUtilization", o."workerKeepAliveMs",
		o."sandboxGroup", o."minimumSandboxes", o."workersPerSandbox"
		FROM `+servicesTable+` s LEFT JOIN `+serviceOverridesTable+` o ON o."serviceId" = s."serviceId"
		WHERE s."serviceId" = $1`, serviceID)
	state, err := scanDesiredServiceState(row, nil)
	if errors.Is(err, sql.ErrNoRows) {
		return DesiredServiceState{}, false, nil
	}
	return state, err == nil, err
}

type serviceStateRow interface{ Scan(...any) error }

func scanDesiredServiceState(row serviceStateRow, serviceID *string) (DesiredServiceState, error) {
	var state DesiredServiceState
	var generation int64
	var serviceType, sandboxGroup sql.NullString
	var sessionKeepAlive, minimumWorkers, maximumWorkers, concurrency, workerKeepAlive, minimumSandboxes, workersPerSandbox sql.NullInt64
	var target sql.NullFloat64
	destinations := []any{
		&state.Enabled, &generation, &serviceType, &sessionKeepAlive,
		&minimumWorkers, &maximumWorkers, &concurrency, &target,
		&workerKeepAlive, &sandboxGroup, &minimumSandboxes, &workersPerSandbox,
	}
	if serviceID != nil {
		destinations = append([]any{serviceID}, destinations...)
	}
	if err := row.Scan(destinations...); err != nil {
		return DesiredServiceState{}, err
	}
	if generation < 0 {
		return DesiredServiceState{}, errors.New("service desired version cannot be negative")
	}
	state.Generation = uint64(generation)
	state.Lifecycle.ServiceType = stringPointer(serviceType)
	state.Lifecycle.SessionKeepAlive = durationPointer(sessionKeepAlive)
	state.Scaling.MinimumWorkers = intPointer(minimumWorkers)
	state.Scaling.MaximumWorkers = intPointer(maximumWorkers)
	state.Scaling.ConcurrencyPerWorker = intPointer(concurrency)
	state.Scaling.TargetUtilization = floatPointer(target)
	state.Scaling.WorkerKeepAlive = durationPointer(workerKeepAlive)
	state.Placement.SandboxGroup = stringPointer(sandboxGroup)
	state.Placement.MinimumSandboxes = intPointer(minimumSandboxes)
	state.Placement.WorkersPerSandbox = intPointer(workersPerSandbox)
	return state, nil
}

func (s *DatabaseServiceStateStore) Put(serviceID string, state DesiredServiceState) error {
	if _, err := ParseServiceID(serviceID); err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := database.EncodeTime(s.database, time.Now())
	result, err := tx.ExecContext(ctx, `UPDATE `+servicesTable+` SET "enabled" = $1, "desiredVersion" = $2, "updatedAt" = $3 WHERE "serviceId" = $4`, state.Enabled, int64(state.Generation), now, serviceID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return fmt.Errorf("service %q is not installed", serviceID)
	}
	if err := writeServiceOverrides(ctx, tx, serviceID, state, now); err != nil {
		return err
	}
	if err := publishServiceChange(ctx, tx, serviceID, now); err != nil {
		return err
	}
	return tx.Commit()
}

type serviceStateExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func writeServiceOverrides(ctx context.Context, executor serviceStateExecer, serviceID string, state DesiredServiceState, now any) error {
	if noServiceOverrides(state) {
		_, err := executor.ExecContext(ctx, `DELETE FROM `+serviceOverridesTable+` WHERE "serviceId" = $1`, serviceID)
		return err
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO `+serviceOverridesTable+` ("serviceId", "serviceType", "sessionKeepAliveMs", "minimumWorkers", "maximumWorkers", "concurrencyPerWorker", "targetUtilization", "workerKeepAliveMs", "sandboxGroup", "minimumSandboxes", "workersPerSandbox", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT ("serviceId") DO UPDATE SET "serviceType" = excluded."serviceType",
		"sessionKeepAliveMs" = excluded."sessionKeepAliveMs", "minimumWorkers" = excluded."minimumWorkers",
		"maximumWorkers" = excluded."maximumWorkers", "concurrencyPerWorker" = excluded."concurrencyPerWorker",
		"targetUtilization" = excluded."targetUtilization", "workerKeepAliveMs" = excluded."workerKeepAliveMs",
		"sandboxGroup" = excluded."sandboxGroup", "minimumSandboxes" = excluded."minimumSandboxes",
		"workersPerSandbox" = excluded."workersPerSandbox", "updatedAt" = excluded."updatedAt"`,
		serviceID, nullableString(state.Lifecycle.ServiceType), nullableDuration(state.Lifecycle.SessionKeepAlive),
		nullableInt(state.Scaling.MinimumWorkers), nullableInt(state.Scaling.MaximumWorkers),
		nullableInt(state.Scaling.ConcurrencyPerWorker), nullableFloat(state.Scaling.TargetUtilization),
		nullableDuration(state.Scaling.WorkerKeepAlive), nullableString(state.Placement.SandboxGroup),
		nullableInt(state.Placement.MinimumSandboxes), nullableInt(state.Placement.WorkersPerSandbox), now)
	return err
}

func (s *DatabaseServiceStateStore) Delete(serviceID string) error {
	if _, err := ParseServiceID(serviceID); err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM ` + serviceOverridesTable + ` WHERE "serviceId" = $1`,
		`DELETE FROM ` + serviceVersionsTable + ` WHERE "serviceId" = $1`,
		`DELETE FROM ` + servicesTable + ` WHERE "serviceId" = $1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, serviceID); err != nil {
			return err
		}
	}
	if err := publishServiceChange(ctx, tx, serviceID, database.EncodeTime(s.database, time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

// ServiceRevision is the single-row no-change check used by every node.
func (s *DatabaseServiceStateStore) ServiceRevision(ctx context.Context) (uint64, error) {
	var revision int64
	err := s.database.QueryRowContext(ctx, `SELECT "revision" FROM "the8020__system__revisions" WHERE "domain" = 'services'`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if revision < 0 {
		return 0, errors.New("service revision cannot be negative")
	}
	return uint64(revision), nil
}

// ServiceChanges returns only services whose latest change falls within the
// requested revision window. One marker per service bounds retained state.
func (s *DatabaseServiceStateStore) ServiceChanges(ctx context.Context, after, through uint64) ([]ServiceChange, error) {
	if through < after {
		return nil, errors.New("service revision window moved backwards")
	}
	rows, err := s.database.QueryContext(ctx, `SELECT r."domain", COALESCE(s."active", FALSE)
		FROM "the8020__system__revisions" r
		LEFT JOIN `+servicesTable+` s ON s."serviceId" = SUBSTR(r."domain", 9)
		WHERE r."domain" LIKE 'service:%' AND r."revision" > $1 AND r."revision" <= $2
		ORDER BY r."revision", r."domain"`, int64(after), int64(through))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []ServiceChange{}
	for rows.Next() {
		var domain string
		var change ServiceChange
		if err := rows.Scan(&domain, &change.Active); err != nil {
			return nil, err
		}
		change.ServiceID = strings.TrimPrefix(domain, "service:")
		if domain == change.ServiceID {
			return nil, fmt.Errorf("invalid service change domain %q", domain)
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func publishServiceChange(ctx context.Context, tx *sql.Tx, serviceID string, now any) error {
	var revision int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt")
		VALUES ('services', 1, $1)
		ON CONFLICT ("domain") DO UPDATE SET "revision" = "the8020__system__revisions"."revision" + 1,
		"updatedAt" = excluded."updatedAt" RETURNING "revision"`, now).Scan(&revision); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt")
		VALUES ($1, $2, $3)
		ON CONFLICT ("domain") DO UPDATE SET "revision" = excluded."revision", "updatedAt" = excluded."updatedAt"`,
		"service:"+serviceID, revision, now)
	return err
}

func (s *DatabaseServiceStateStore) List() ([]StoredServiceState, error) {
	rows, err := s.database.QueryContext(context.Background(), `SELECT s."serviceId", s."enabled", s."desiredVersion",
		o."serviceType", o."sessionKeepAliveMs", o."minimumWorkers", o."maximumWorkers",
		o."concurrencyPerWorker", o."targetUtilization", o."workerKeepAliveMs",
		o."sandboxGroup", o."minimumSandboxes", o."workersPerSandbox"
		FROM `+servicesTable+` s LEFT JOIN `+serviceOverridesTable+` o ON o."serviceId" = s."serviceId"
		WHERE s."active" ORDER BY s."serviceId"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []StoredServiceState{}
	for rows.Next() {
		var serviceID string
		state, err := scanDesiredServiceState(rows, &serviceID)
		if err != nil {
			return nil, err
		}
		result = append(result, StoredServiceState{ServiceID: serviceID, State: state})
	}
	return result, rows.Err()
}

func (s *DatabaseServiceStateStore) Lock(ctx context.Context, serviceID string) (UnlockFunc, error) {
	if _, err := ParseServiceID(serviceID); err != nil {
		return nil, err
	}
	value, _ := s.locks.LoadOrStore(serviceID, make(chan struct{}, 1))
	semaphore := value.(chan struct{})
	select {
	case semaphore <- struct{}{}:
		var once sync.Once
		return func() error {
			once.Do(func() { <-semaphore })
			return nil
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func noServiceOverrides(state DesiredServiceState) bool {
	return state.Lifecycle.ServiceType == nil && state.Lifecycle.SessionKeepAlive == nil &&
		state.Scaling.MinimumWorkers == nil && state.Scaling.MaximumWorkers == nil &&
		state.Scaling.ConcurrencyPerWorker == nil && state.Scaling.TargetUtilization == nil &&
		state.Scaling.WorkerKeepAlive == nil && state.Placement.SandboxGroup == nil &&
		state.Placement.MinimumSandboxes == nil && state.Placement.WorkersPerSandbox == nil
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableDuration(value *string) any {
	if value == nil {
		return nil
	}
	duration, err := time.ParseDuration(*value)
	if err != nil {
		return *value
	}
	return duration.Milliseconds()
}

func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func intPointer(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}

func floatPointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func durationPointer(value sql.NullInt64) *string {
	if !value.Valid {
		return nil
	}
	result := (time.Duration(value.Int64) * time.Millisecond).String()
	return &result
}

func manifestDuration(value string) any {
	if value == "" {
		return nil
	}
	duration, _ := time.ParseDuration(value)
	return duration.Milliseconds()
}

func nullableManifestInt(value *int) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}

func nullableManifestFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
