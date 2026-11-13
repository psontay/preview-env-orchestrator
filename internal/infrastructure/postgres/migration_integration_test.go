//go:build integration

package postgres_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"preview-env-orchestrator/internal/domain"
	pgadapter "preview-env-orchestrator/internal/infrastructure/postgres"
)

// Build a genuinely old schema before inserting history; newTestPool alone
// only verifies migrations on an empty database.
func oldMigrationsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files, err := filepath.Glob(filepath.Join(migrationsDir(t), "00[1-5]_*.sql"))
	if err != nil || len(files) != 5 {
		t.Fatalf("old migrations: files=%d err=%v", len(files), err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(file)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func seedMigrationEnvironment(t *testing.T, pool *pgxpool.Pool) *domain.PreviewEnvironment {
	t.Helper()
	ctx := t.Context()
	pr := &domain.PullRequest{ID: "PR_upgrade", Repo: "org/upgrade", Number: 1,
		CommitHash: "old-sha", Status: domain.PRStatusOpen}
	if err := pgadapter.NewPullRequestRepository(pool).Save(ctx, pr); err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	env := &domain.PreviewEnvironment{
		ID: pr.IdempotencyKey(), IdempotencyKey: pr.IdempotencyKey(),
		PullRequestID: pr.ID, Repo: pr.Repo, CommitSHA: pr.CommitHash,
		Status: domain.EnvDestroyed, CreatedAt: expires.Add(-time.Hour), ExpiresAt: &expires,
		TTL: domain.TTL{Duration: time.Hour}, ResourceGroup: "peo-upgrade-history",
		DeletionCancelCount: 1, DestructionNoticePending: true,
	}
	if err := pgadapter.NewEnvironmentRepository(pool).Save(ctx, env); err != nil {
		t.Fatal(err)
	}
	return env
}

func requireSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != state {
		t.Fatalf("want SQLSTATE %s, got %v", state, err)
	}
}

func TestDatabaseSchemaUpgradePreservesHistory(t *testing.T) {
	ctx := t.Context()
	pool := newTestPoolWithMigrations(t, oldMigrationsDir(t))
	env := seedMigrationEnvironment(t, pool)
	// Snapshot every field, including NULL/empty values and timestamps.
	var before string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(e)::text FROM preview_environments e WHERE id=$1`, env.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys(idempotency_key) VALUES($1)`, env.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if err := pgadapter.RunMigrations(ctx, pool, migrationsDir(t)); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(e)::text FROM preview_environments e WHERE id=$1`, env.ID).Scan(&after); err != nil || after != before {
		t.Fatalf("history changed: before=%s after=%s err=%v", before, after, err)
	}
	var removed, retained, statusCheck bool
	err := pool.QueryRow(ctx, `SELECT
		to_regclass('idx_env_idempotency_key') IS NULL,
		EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='preview_environments'::regclass AND conname='preview_environments_idempotency_key_key' AND contype='u'),
		EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='preview_environments'::regclass AND conname='preview_environments_status_check' AND convalidated)`).Scan(&removed, &retained, &statusCheck)
	if err != nil || !removed || !retained || !statusCheck {
		t.Fatalf("schema: removed=%v retained=%v check=%v err=%v", removed, retained, statusCheck, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO preview_environments(id,pull_request_id,idempotency_key,repo,status,ttl_seconds)
		VALUES('another-id',$1,$2,$3,'RUNNING',3600)`, env.PullRequestID, env.IdempotencyKey, env.Repo)
	requireSQLState(t, err, "23505")
	_, err = pool.Exec(ctx, `UPDATE preview_environments SET status='UNKNOWN' WHERE id=$1`, env.ID)
	requireSQLState(t, err, "23514")
	for _, status := range []domain.EnvStatus{domain.EnvProvisioning, domain.EnvRunning,
		domain.EnvMarkedForDeletion, domain.EnvDestroying, domain.EnvFailed, domain.EnvDestroyed} {
		if _, err := pool.Exec(ctx, `UPDATE preview_environments SET status=$2 WHERE id=$1`, env.ID, string(status)); err != nil {
			t.Fatalf("supported status %s: %v", status, err)
		}
	}
	// Do not reject domain-supported immediate expiry or truncated subsecond TTL.
	if _, err := pool.Exec(ctx, `UPDATE preview_environments SET ttl_seconds=0 WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	var appliedBefore, appliedAfter string
	if err := pool.QueryRow(ctx, `SELECT json_agg(m ORDER BY name)::text FROM schema_migrations m`).Scan(&appliedBefore); err != nil {
		t.Fatal(err)
	}
	if err := pgadapter.RunMigrations(ctx, pool, migrationsDir(t)); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT json_agg(m ORDER BY name)::text FROM schema_migrations m`).Scan(&appliedAfter); err != nil || appliedAfter != appliedBefore {
		t.Fatalf("rerun changed migration history: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE idempotency_key=$1`, env.IdempotencyKey).Scan(&count); err != nil || count != 1 {
		t.Fatalf("idempotency history changed: count=%d err=%v", count, err)
	}
}

func TestDatabaseSchemaUpgradeFailureRollsBack(t *testing.T) {
	ctx := t.Context()
	pool := newTestPoolWithMigrations(t, oldMigrationsDir(t))
	env := seedMigrationEnvironment(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE preview_environments SET status='LEGACY_UNKNOWN' WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	// 006 succeeds first, then 007 must reject invalid history. Both roll back.
	err := pgadapter.RunMigrations(ctx, pool, migrationsDir(t))
	requireSQLState(t, err, "23514")
	var restored bool
	var versions int
	if err := pool.QueryRow(ctx, `SELECT to_regclass('idx_env_idempotency_key') IS NOT NULL,
		(SELECT count(*) FROM schema_migrations)`).Scan(&restored, &versions); err != nil || !restored || versions != 5 {
		t.Fatalf("partial upgrade: index_restored=%v versions=%d err=%v", restored, versions, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM preview_environments WHERE id=$1`, env.ID).Scan(&status); err != nil || status != "LEGACY_UNKNOWN" {
		t.Fatalf("upgrade must not repair/delete history silently: status=%s err=%v", status, err)
	}
}

func TestDatabaseIndexCleanupGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		fails bool
	}{
		{"missing_unique_constraint", `ALTER TABLE preview_environments DROP CONSTRAINT preview_environments_idempotency_key_key`, true},
		{"different_index_definition", `DROP INDEX idx_env_idempotency_key; CREATE INDEX idx_env_idempotency_key ON preview_environments(repo)`, true},
		{"already_removed_index", `DROP INDEX idx_env_idempotency_key`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			pool := newTestPoolWithMigrations(t, oldMigrationsDir(t))
			if _, err := pool.Exec(ctx, tc.setup); err != nil {
				t.Fatal(err)
			}
			err := pgadapter.RunMigrations(ctx, pool, migrationsDir(t))
			if tc.fails {
				requireSQLState(t, err, "P0001")
				var present bool
				if err := pool.QueryRow(ctx, `SELECT to_regclass('idx_env_idempotency_key') IS NOT NULL`).Scan(&present); err != nil || !present {
					t.Fatalf("guard removed the unexpected index: present=%v err=%v", present, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Execute the psql plan produced by the actual Node entry point on a temporary
// PostgreSQL container. Never run the host wrapper against peo-postgres in tests.
func TestDatabaseOnlyUpgradeScript(t *testing.T) {
	ctx := t.Context()
	script := filepath.Join(migrationsDir(t), "..", "scripts", "database-migrate.mjs")
	plan, err := exec.CommandContext(ctx, "node", script, "--plan").Output()
	if err != nil {
		t.Fatalf("generate database upgrade plan (Node.js required): %v", err)
	}
	pool, ctr := newTestDatabase(t, oldMigrationsDir(t))
	env := seedMigrationEnvironment(t, pool)
	if err := ctr.CopyToContainer(ctx, plan, "/tmp/peo-upgrade.sql", 0600); err != nil {
		t.Fatal(err)
	}
	runPlan := func(wantSuccess bool) {
		t.Helper()
		code, reader, err := ctr.Exec(ctx, []string{"psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "test", "-d", "preview", "-f", "/tmp/peo-upgrade.sql"})
		if err != nil {
			t.Fatal(err)
		}
		output, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if (code == 0) != wantSuccess {
			t.Fatalf("upgrade plan: exit=%d output=%s", code, output)
		}
	}
	runPlan(true)
	var before, after string
	if err := pool.QueryRow(ctx, `SELECT json_agg(m ORDER BY name)::text FROM schema_migrations m`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 7 {
		t.Fatalf("script migration count=%d err=%v", count, err)
	}
	runPlan(true)
	if err := pool.QueryRow(ctx, `SELECT json_agg(m ORDER BY name)::text FROM schema_migrations m`).Scan(&after); err != nil || after != before {
		t.Fatalf("script replayed applied migrations: %v", err)
	}
	got, err := pgadapter.NewEnvironmentRepository(pool).FindByIdempotencyKey(ctx, env.IdempotencyKey)
	if err != nil || got == nil || got.Status != env.Status || got.ResourceGroup != env.ResourceGroup || got.ExpiresAt == nil || !got.ExpiresAt.Equal(*env.ExpiresAt) {
		t.Fatalf("script changed history: got=%+v err=%v", got, err)
	}
	// Refuse a newer unknown schema, instead of applying an old targeted plan.
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES('999_future.sql')`); err != nil {
		t.Fatal(err)
	}
	runPlan(false)
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE name='999_future.sql';
		DELETE FROM schema_migrations WHERE name='006_remove_duplicate_idempotency_index.sql'`); err != nil {
		t.Fatal(err)
	}
	runPlan(false)
}
