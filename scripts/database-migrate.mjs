#!/usr/bin/env node
// Targeted upgrade of an audited 001-005 database, without starting app/workers.
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

const args = process.argv.slice(2);
if (args.length !== 1 || !['--plan', '--apply'].includes(args[0])) {
  console.log('Usage: node scripts/database-migrate.mjs --plan | --apply');
  console.log('--plan: print SQL without contacting Docker.');
  console.log('--apply: upgrade peo-postgres from 001-005 to 007 in one transaction.');
  process.exit(args.length === 1 && args[0] === '--help' ? 0 : 2);
}

const baseline = [
  '001_init.sql', '002_idempotency_and_pr_lookup.sql',
  '003_preview_lifecycle.sql', '004_deletion_notification.sql',
  '005_destruction_notice.sql',
];
const upgrades = [
  '006_remove_duplicate_idempotency_index.sql',
  '007_environment_status_check.sql',
];
const sqlString = value => `'${value.replaceAll("'", "''")}'`;
const list = names => names.map(sqlString).join(', ');
const parts = [String.raw`\pset pager off
\echo === Database-only upgrade: 006 and 007 ===
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
-- Same transaction advisory lock as postgres.RunMigrations.
SELECT pg_advisory_xact_lock(731925107);
DO $preflight$
BEGIN
    IF current_schema() IS DISTINCT FROM 'public' THEN
        RAISE EXCEPTION 'Expected the audited public schema';
    END IF;
    IF to_regclass('public.schema_migrations') IS NULL THEN
        RAISE EXCEPTION 'Missing schema_migrations; use the application migration runner for initial setup';
    END IF;
END;
$preflight$;`, `
DO $baseline$
BEGIN
    IF (SELECT count(*) FROM schema_migrations WHERE name IN (${list(baseline)})) <> ${baseline.length} THEN
        RAISE EXCEPTION 'Expected all migrations 001-005; run read-only audit first';
    END IF;
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE name NOT IN (${list([...baseline, ...upgrades])})) THEN
        RAISE EXCEPTION 'Unknown migration version; review before using this targeted upgrader';
    END IF;
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE name = ${sqlString(upgrades[1])})
       AND NOT EXISTS (SELECT 1 FROM schema_migrations WHERE name = ${sqlString(upgrades[0])}) THEN
        RAISE EXCEPTION 'Migration history is out of order';
    END IF;
END;
$baseline$;`];

// All files are read before connecting: a missing migration cannot half-apply.
for (const name of upgrades) {
  const ddl = readFileSync(new URL(`../migrations/${name}`, import.meta.url), 'utf8');
  parts.push(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = ${sqlString(name)}) AS migration_applied \\gset
\\if :migration_applied
  \\echo Already applied: ${name}
\\else
  \\echo Applying: ${name}
${ddl}
INSERT INTO schema_migrations(name) VALUES (${sqlString(name)});
\\endif`);
}
parts.push(String.raw`COMMIT;
\echo === Upgrade committed; run database-audit.mjs to verify ===
SELECT name, applied_at FROM schema_migrations ORDER BY name;`);
const sql = parts.join('\n');

if (args[0] === '--plan') {
  process.stdout.write(sql + '\n');
  process.exit(0);
}

const result = spawnSync('docker', [
  'exec', '-i', 'peo-postgres', 'sh', '-c',
  'exec psql -X --set=ON_ERROR_STOP=1 --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"',
], { input: sql, stdio: ['pipe', 'inherit', 'inherit'] });
if (result.error || result.status !== 0) {
  console.error('Upgrade was not confirmed. Audit before retrying; do not reset the database.');
  process.exit(result.status || 1);
}
