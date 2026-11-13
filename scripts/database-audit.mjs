#!/usr/bin/env node
// Audit only the named orchestrator container; do not start services or load .env.
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

const args = process.argv.slice(2);
if (args.length === 1 && args[0] === '--help') {
  console.log('Usage: node scripts/database-audit.mjs');
  console.log('Read-only audit of peo-postgres through docker exec and psql.');
  console.log('Does not start containers, run migrations, or read the host .env.');
  process.exit(0);
}
if (args.length !== 0) {
  console.error('Unexpected argument. Use --help for usage.');
  process.exit(2);
}

const sql = readFileSync(new URL('./database-audit.sql', import.meta.url), 'utf8');
const result = spawnSync('docker', [
  'exec', '-i', 'peo-postgres', 'sh', '-c',
  'exec psql -X --set=ON_ERROR_STOP=1 --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"',
], { input: sql, stdio: ['pipe', 'inherit', 'inherit'] });

if (result.error) {
  console.error(`Could not run database audit: ${result.error.code ?? 'unknown error'}`);
  process.exit(1);
}
if (result.status !== 0) {
  console.error('Database audit did not complete; no schema changes were attempted.');
  process.exit(result.status ?? 1);
}
