CREATE TABLE IF NOT EXISTS _8020_catalog (
    catalog_id text PRIMARY KEY,
    catalog_version bigint NOT NULL,
    initialized bigint NOT NULL,
    package_set_hash text NOT NULL,
    package_set_json text NOT NULL,
    descriptor_set_hash text NOT NULL,
    created_at text NOT NULL,
    initialized_at text NOT NULL,
    updated_at text NOT NULL,
    last_error text NOT NULL,
    last_deployment_at text NOT NULL,
    last_deployment_error text NOT NULL
)
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_tables (
    table_id text PRIMARY KEY,
    descriptor_hash text NOT NULL,
    descriptor_json text NOT NULL,
    source_package text NOT NULL,
    source_commit text NOT NULL,
    source_module text NOT NULL,
    state text NOT NULL,
    synchronization_state text NOT NULL,
    synchronized_at text NOT NULL,
    error text NOT NULL
)
-- 8020:next
CREATE INDEX IF NOT EXISTS _8020_tables_source_package ON _8020_tables (source_package)
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_columns (
    table_id text NOT NULL,
    column_name text NOT NULL,
    logical_type text NOT NULL,
    definition_hash text NOT NULL,
    definition_json text NOT NULL,
    state text NOT NULL,
    PRIMARY KEY (table_id, column_name)
)
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_dependencies (
    table_id text NOT NULL,
    module_path text NOT NULL,
    PRIMARY KEY (table_id, module_path)
)
-- 8020:next
CREATE INDEX IF NOT EXISTS _8020_dependencies_module_path ON _8020_dependencies (module_path)
-- 8020:next
CREATE TABLE IF NOT EXISTS _8020_pending_deployment (
    deployment_id text PRIMARY KEY,
    previous_package_set_hash text NOT NULL,
    previous_package_set_json text NOT NULL,
    candidate_package_set_hash text NOT NULL,
    candidate_package_set_json text NOT NULL,
    candidates_json text NOT NULL,
    stage text NOT NULL,
    error text NOT NULL,
    started_at text NOT NULL,
    updated_at text NOT NULL
)
