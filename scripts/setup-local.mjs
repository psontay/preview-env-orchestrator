#!/usr/bin/env node
// Run locally. Never prints credentials or invokes Docker, migrations or Git.
import { readFile, writeFile, stat, chmod } from 'node:fs/promises';
import { randomBytes } from 'node:crypto';

try {
  const envURL = new URL('../.env', import.meta.url);
  let content;
  try { content = await readFile(envURL, 'utf8'); }
  catch (error) {
    if (error.code !== 'ENOENT') throw error;
    content = await readFile(new URL('../.env.example', import.meta.url), 'utf8');
  }
  function get(key) {
    const match = content.match(new RegExp(`^${key}=(.*)$`, 'm'));
    return match?.[1]?.trim().replace(/^(['"])(.*)\1$/, '$2') ?? '';
  }
  function set(key, value) {
    const expression = new RegExp(`^${key}=.*$`, 'm');
    content = expression.test(content) ? content.replace(expression, () => `${key}=${value}`) : `${content.trimEnd()}\n${key}=${value}\n`;
  }
  const socket = await stat('/var/run/docker.sock');
  set('DOCKER_GID', String(socket.gid));
  set('STORAGE_DRIVER', 'postgres'); set('DOCKER_DRIVER', 'real'); set('GITHUB_DRIVER', 'real');
  set('OTEL_EXPORTER_OTLP_ENDPOINT', 'jaeger:4317');
  for (const [key, value] of Object.entries({ PREVIEW_REPOSITORY: 'psontay/todolist', PREVIEW_ALLOWED_ACTOR: 'psontay', PREVIEW_TTL: '10m', DELETION_GRACE_PERIOD: '1m', CLEANUP_INTERVAL: '5s', PROVISION_TIMEOUT: '20m', PREVIEW_READY_TIMEOUT: '3m' })) {
    if (!get(key) || get(key).includes('CHANGE_ME')) set(key, value);
  }
  for (const key of ['POSTGRES_PASSWORD', 'GITHUB_WEBHOOK_SECRET']) {
    if (!get(key) || /CHANGE_ME|YOUR_|<[^>]+>/.test(get(key))) set(key, randomBytes(32).toString('hex'));
  }
  // Compose generates the real DSN. Avoid leaving stale credential placeholders.
  set('DATABASE_URL', `postgres://${encodeURIComponent(get('POSTGRES_USER') || 'preview')}:${encodeURIComponent(get('POSTGRES_PASSWORD'))}@postgres:5432/${encodeURIComponent(get('POSTGRES_DB') || 'preview')}?sslmode=disable`);
  const publicIndex = process.argv.indexOf('--public-url');
  if (publicIndex >= 0) {
    const url = new URL(process.argv[publicIndex + 1]);
    if (url.protocol !== 'https:' || url.username || url.password || url.pathname !== '/' || url.search || url.hash) throw new Error('Public URL must be an HTTPS origin');
    set('PUBLIC_BASE_URL', url.origin);
  }
  // Validate before writing, so an incomplete setup never replaces .env.
  for (const key of ['GITHUB_TOKEN', 'GITHUB_WEBHOOK_SECRET', 'PUBLIC_BASE_URL']) {
    if (!get(key) || /CHANGE_ME|YOUR_|<[^>]+>/.test(get(key))) throw new Error(`Fill ${key} in .env first (do not paste its value into chat)`);
  }
  if (new URL(get('PUBLIC_BASE_URL')).protocol !== 'https:') throw new Error('Use --public-url with the HTTPS tunnel URL');
  await writeFile(envURL, content, { mode: 0o600 });
  await chmod(envURL, 0o600);
  console.log('Local setup saved; existing token and secrets preserved.');
  console.log(`Orchestrator: http://127.0.0.1:18080; preview TTL=${get('PREVIEW_TTL')}; grace=${get('DELETION_GRACE_PERIOD')}.`);
  console.log('Next: docker compose up -d --build');
} catch (error) {
  console.error(error.message.startsWith('Fill ') || error.message.startsWith('Use ') || error.message.startsWith('Public URL') ? error.message : 'Setup failed. Check .env format, Docker socket path and file permissions. No credentials were printed.');
  process.exitCode = 1;
}
