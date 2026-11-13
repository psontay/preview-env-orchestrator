package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunMigrations versions every SQL file in one transaction. Schemas created by
// the original compose init directory are adopted without replaying old DDL.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("postgres: không tìm thấy file .sql nào trong %s", dir)
	}
	sort.Strings(files)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731925107)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(name)
		SELECT '001_init.sql' WHERE to_regclass('pull_requests') IS NOT NULL AND to_regclass('preview_environments') IS NOT NULL ON CONFLICT DO NOTHING;
		INSERT INTO schema_migrations(name) SELECT '002_idempotency_and_pr_lookup.sql'
		WHERE to_regclass('idempotency_keys') IS NOT NULL AND EXISTS
		(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='pull_requests' AND column_name='idempotency_key')
		ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	for _, f := range files {
		name := filepath.Base(f)
		var applied bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		content, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("postgres: migration %s lỗi: %w", filepath.Base(f), err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
