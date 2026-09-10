package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunMigrations áp dụng lần lượt mọi file *.sql trong dir theo thứ tự tên file
// (001_..., 002_..., ...). Cố ý giữ tối giản, không bảng schema_migrations —
// đủ cho phạm vi đồ án và cho integration test tự dựng schema trên một
// Postgres tạm. Với production nên thay bằng golang-migrate hoặc tương đương.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("postgres: không tìm thấy file .sql nào trong %s", dir)
	}
	sort.Strings(files)
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("postgres: migration %s lỗi: %w", filepath.Base(f), err)
		}
	}
	return nil
}
