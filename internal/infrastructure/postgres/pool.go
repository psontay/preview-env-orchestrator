// Package postgres là adapter THẬT cho các port lưu trữ định nghĩa ở
// internal/domain (PullRequestRepository, EnvironmentRepository), hiện thực
// bằng github.com/jackc/pgx/v5 + pgxpool.
//
// Adapter in-memory ở internal/infrastructure/memory vẫn được giữ nguyên để
// dùng cho unit test và chạy dev nhanh; main.go chọn adapter qua biến môi
// trường STORAGE_DRIVER.
package postgres

import (
	"context"
	"errors"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool tạo một connection pool pgx, đọc DSN từ biến môi trường DATABASE_URL.
// Ví dụ: postgres://user:pass@localhost:5432/preview?sslmode=disable
func NewPool(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("postgres: biến môi trường DATABASE_URL chưa được thiết lập")
	}
	return NewPoolWithDSN(ctx, dsn)
}

// NewPoolWithDSN hữu ích cho test (testcontainers cấp DSN động).
func NewPoolWithDSN(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
