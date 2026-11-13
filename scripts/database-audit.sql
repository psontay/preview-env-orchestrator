-- Read-only preflight for the orchestrator DB, not a schema migration.
-- Run through scripts/database-audit.mjs; never print credentials or row data.
\pset pager off
\echo === Orchestrator database audit (read only) ===
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '10s';
SET LOCAL lock_timeout = '3s';

SELECT current_setting('server_version') AS postgres_version,
       current_schema() AS schema,
       current_setting('transaction_read_only') AS read_only;

\echo === Tables and approximate storage (catalog, not a benchmark) ===
SELECT c.relname AS table_name,
       pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = current_schema()
  AND c.relkind = 'r'
  AND c.relname IN ('pull_requests', 'preview_environments',
                   'idempotency_keys', 'schema_migrations')
ORDER BY c.relname;

\echo === Actual column definitions ===
SELECT table_name, column_name, data_type, is_nullable,
       column_default, is_generated, generation_expression
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name IN ('pull_requests', 'preview_environments',
                    'idempotency_keys', 'schema_migrations')
ORDER BY table_name, ordinal_position;

\echo === Constraints and their supporting indexes ===
SELECT r.relname AS table_name, c.conname, c.contype,
       pg_get_constraintdef(c.oid) AS definition,
       CASE WHEN c.conindid <> 0 THEN c.conindid::regclass::text END AS supporting_index
FROM pg_constraint c
JOIN pg_class r ON r.oid = c.conrelid
JOIN pg_namespace n ON n.oid = r.relnamespace
WHERE n.nspname = current_schema()
  AND r.relname IN ('pull_requests', 'preview_environments',
                   'idempotency_keys', 'schema_migrations')
ORDER BY r.relname, c.conname;

\echo === Index definitions and validity ===
SELECT r.relname AS table_name, x.relname AS index_name,
       i.indisunique, i.indisvalid, i.indisready,
       pg_get_indexdef(i.indexrelid) AS definition
FROM pg_index i
JOIN pg_class r ON r.oid = i.indrelid
JOIN pg_class x ON x.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = r.relnamespace
WHERE n.nspname = current_schema()
  AND r.relname IN ('pull_requests', 'preview_environments',
                   'idempotency_keys', 'schema_migrations')
ORDER BY r.relname, x.relname;

-- Keep catalog evidence even when the DB has not reached the current schema.
SELECT to_regclass('schema_migrations') IS NOT NULL AS has_migration_table \gset
\if :has_migration_table
  \echo === Applied migrations ===
  SELECT name, applied_at FROM schema_migrations ORDER BY name;
\else
  \echo No schema_migrations table; legacy or uninitialized schema needs review.
\endif

SELECT to_regclass('preview_environments') IS NOT NULL AS has_environments \gset
\if :has_environments
  \echo === Environment counts (no keys, URLs or credentials) ===
  SELECT status, count(*) AS environments
  FROM preview_environments GROUP BY status ORDER BY status;

  \echo === Potential CHECK preconditions (no changes applied) ===
  SELECT count(*) FILTER (WHERE status NOT IN
           ('PROVISIONING', 'RUNNING', 'MARK_FOR_DELETION',
            'DESTROYING', 'DESTROYED', 'FAILED')) AS unknown_status,
         count(*) FILTER (WHERE ttl_seconds <= 0) AS nonpositive_ttl
  FROM preview_environments;
\else
  \echo No preview_environments table; do not apply index cleanup yet.
\endif

\echo === End of read-only audit; no migrations or data changes applied ===
ROLLBACK;
