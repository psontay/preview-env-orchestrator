#!/usr/bin/env node
// Authenticated, read-only diagnostics. Credentials are printed only in your
// terminal for Postman, never sent to GitHub comments or browser URLs.
import { readFile } from 'node:fs/promises';

try {
  const contents = await readFile(new URL('../.env', import.meta.url), 'utf8');
  const vars = Object.fromEntries(contents.split(/\r?\n/).filter(line => /^[A-Za-z_][A-Za-z0-9_]*=/.test(line)).map(line => {
    const index = line.indexOf('=');
    return [line.slice(0, index), line.slice(index + 1).trim().replace(/^(['"])(.*)\1$/, '$2')];
  }));
  const args = process.argv.slice(2);
  const safe = args.includes('--safe');
  const shaIndex = args.indexOf('--sha');
  const commit = shaIndex >= 0 ? args[shaIndex + 1] : undefined;
  const number = args[0];
  if (shaIndex >= 0 ? !/^[a-fA-F0-9]{40}$/.test(commit || '') : !/^[1-9][0-9]*$/.test(number || '')) {
    throw new Error('Usage: node scripts/preview.mjs PR_NUMBER [--safe], or --sha FULL_SHA --safe');
  }
  const repo = vars.PREVIEW_REPOSITORY || 'psontay/todolist';
  const endpoint = commit ? `commits/${commit}/pulls` : `pulls/${number}`;
  const response = await fetch(`https://api.github.com/repos/${repo}/${endpoint}`, {
    headers: { Authorization: `Bearer ${vars.GITHUB_TOKEN}`, Accept: 'application/vnd.github+json', 'User-Agent': 'preview-env-local' },
    signal: AbortSignal.timeout(10000),
  });
  if (!response.ok) throw new Error(`GitHub returned HTTP ${response.status}`);
  const data = await response.json();
  const pr = commit ? data.find(item => item.head.sha === commit) ?? data[0] : data;
  if (!pr) throw new Error('GitHub returned no pull request for this commit');
  const id = `${repo}:${pr.node_id}:${commit || pr.head.sha}`;
  const state = await fetch(`http://127.0.0.1:18080/environments/${encodeURIComponent(id)}`, {
    headers: { Authorization: `Bearer ${vars.GITHUB_WEBHOOK_SECRET}` },
    signal: AbortSignal.timeout(10000),
  });
  if (!state.ok) throw new Error(`Orchestrator returned HTTP ${state.status}${state.status === 404 ? ': no preview for this commit; check webhook delivery' : ''}`);
  const result = await state.json();
  if (safe && typeof result.last_error === 'string') {
    for (const [key, value] of Object.entries(vars)) {
      if (/(PASSWORD|SECRET|TOKEN|KEY|GMAIL_PASS)/i.test(key) && value.length >= 6) result.last_error = result.last_error.split(value).join('[REDACTED]');
    }
    result.last_error = result.last_error
      .replace(/\b(?:github_pat_[a-z0-9_]+|gh[pousr]_[a-z0-9_]+)\b/gi, '[REDACTED]')
      .replace(/([?&](?:token|access_token)=)[^&\s]+/gi, '$1[REDACTED]')
      .replace(/(https?:\/\/)[^/\s:@]+:[^@\s/]+@/gi, '$1[REDACTED]@');
  }
  const output = safe ? Object.fromEntries(['id', 'status', 'commit_sha', 'url', 'resource_group', 'last_error'].map(key => [key, result[key]])) : result;
  console.log(JSON.stringify(output, null, 2));
} catch (error) {
  console.error(/^(Usage:|GitHub returned|Orchestrator returned)/.test(error.message) ? error.message : 'Cannot fetch preview status. Check that the orchestrator is running, .env is valid and GitHub is reachable.');
  process.exitCode = 1;
}
