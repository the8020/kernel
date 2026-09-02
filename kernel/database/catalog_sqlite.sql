CREATE TABLE IF NOT EXISTS _8020_catalog (
    catalog_id TEXT PRIMARY KEY,
    catalog_version INTEGER NOT NULL,
    initialized INTEGER NOT NULL,
    package_set_hash TEXT NOT NULL,
    package_set_json TEXT NOT NULL,
    descriptor_set_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    initialized_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_error TEXT NOT NULL,
    last_deployment_at TEXT NOT NULL,
    last_deployment_error TEXT NOT NULL
) STRICT
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_tables (
    table_id TEXT PRIMARY KEY,
    descriptor_hash TEXT NOT NULL,
    descriptor_json TEXT NOT NULL,
    source_package TEXT NOT NULL,
    source_commit TEXT NOT NULL,
    source_module TEXT NOT NULL,
    state TEXT NOT NULL,
    synchronization_state TEXT NOT NULL,
    synchronized_at TEXT NOT NULL,
    error TEXT NOT NULL
) STRICT
-- 8020:next
CREATE INDEX IF NOT EXISTS _8020_tables_source_package ON _8020_tables (source_package)
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_columns (
    table_id TEXT NOT NULL,
    column_name TEXT NOT NULL,
    logical_type TEXT NOT NULL,
    definition_hash TEXT NOT NULL,
    definition_json TEXT NOT NULL,
    state TEXT NOT NULL,
    PRIMARY KEY (table_id, column_name)
) STRICT
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_dependencies (
    table_id TEXT NOT NULL,
    module_path TEXT NOT NULL,
    PRIMARY KEY (table_id, module_path)
) STRICT
-- 8020:next
CREATE INDEX IF NOT EXISTS _8020_dependencies_module_path ON _8020_dependencies (module_path)
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_pending_deployment (
    deployment_id TEXT PRIMARY KEY,
    previous_package_set_hash TEXT NOT NULL,
    previous_package_set_json TEXT NOT NULL,
    candidate_package_set_hash TEXT NOT NULL,
    candidate_package_set_json TEXT NOT NULL,
    candidates_json TEXT NOT NULL,
    stage TEXT NOT NULL,
    error TEXT NOT NULL,
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT
