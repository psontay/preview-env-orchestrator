//go:build integration

// Integration test cho adapter Postgres. Tách bằng build tag `integration`
// để `go test ./...` thông thường KHÔNG chạy (tránh phụ thuộc Docker).
//
// Chạy:  go test -tags=integration ./internal/infrastructure/postgres/...
// Yêu cầu: Docker daemon khả dụng (testcontainers-go tự bật Postgres tạm).
package postgres_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"preview-env-orchestrator/internal/domain"
	pgadapter "preview-env-orchestrator/internal/infrastructure/postgres"
)

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller thất bại")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "migrations")
}

// newTestPool bật một Postgres tạm bằng testcontainers, chạy toàn bộ migration
// và trả về pool đã sẵn sàng. Container được dọn qua t.Cleanup.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("preview"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("bật Postgres container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("lấy DSN: %v", err)
	}

	pool, err := pgadapter.NewPoolWithDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("tạo pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pgadapter.RunMigrations(ctx, pool, migrationsDir(t)); err != nil {
		t.Fatalf("chạy migration: %v", err)
	}
	return pool
}

func TestPullRequestRepository(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	repo := pgadapter.NewPullRequestRepository(pool)

	pr := &domain.PullRequest{
		ID: "PR_1", Repo: "org/app", Number: 7, Author: "alice",
		CommitHash: "abc123", Status: domain.PRStatusOpen,
	}
	if err := repo.Save(ctx, pr); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.FindByIdempotencyKey(ctx, pr.IdempotencyKey())
	if err != nil {
		t.Fatalf("FindByIdempotencyKey: %v", err)
	}
	if got == nil || got.ID != "PR_1" || got.Repo != "org/app" || got.Number != 7 || got.Author != "alice" {
		t.Fatalf("PR round-trip sai: %+v", got)
	}

	// Save là upsert (webhook synchronize gửi lại cùng PR).
	pr.Author = "bob"
	if err := repo.Save(ctx, pr); err != nil {
		t.Fatalf("Save (upsert): %v", err)
	}
	got, _ = repo.FindByIdempotencyKey(ctx, pr.IdempotencyKey())
	if got.Author != "bob" {
		t.Fatalf("upsert không cập nhật author, got %q", got.Author)
	}

	miss, err := repo.FindByIdempotencyKey(ctx, "khong-ton-tai")
	if err != nil {
		t.Fatalf("FindByIdempotencyKey (miss): %v", err)
	}
	if miss != nil {
		t.Fatalf("key không tồn tại phải trả nil, got %+v", miss)
	}
}

func TestEnvironmentRepository_TryAcquireLock(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	envRepo := pgadapter.NewEnvironmentRepository(pool)

	ok, err := envRepo.TryAcquireLock(ctx, "repo:PR_1:sha")
	if err != nil || !ok {
		t.Fatalf("lock lần 1 phải thành công: ok=%v err=%v", ok, err)
	}

	ok, err = envRepo.TryAcquireLock(ctx, "repo:PR_1:sha")
	if err != nil {
		t.Fatalf("lock lần 2 lỗi: %v", err)
	}
	if ok {
		t.Fatal("lock lần 2 cho cùng key phải trả false (duplicate webhook)")
	}

	ok, err = envRepo.TryAcquireLock(ctx, "repo:PR_2:sha")
	if err != nil || !ok {
		t.Fatalf("lock cho key khác phải thành công: ok=%v err=%v", ok, err)
	}
}

func TestEnvironmentRepository_QuotaAndLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	prRepo := pgadapter.NewPullRequestRepository(pool)
	envRepo := pgadapter.NewEnvironmentRepository(pool)

	mkPR := func(id, commit string) *domain.PullRequest {
		pr := &domain.PullRequest{
			ID: id, Repo: "org/app", Number: 1, Author: "a",
			CommitHash: commit, Status: domain.PRStatusOpen,
		}
		if err := prRepo.Save(ctx, pr); err != nil {
			t.Fatalf("save PR %s: %v", id, err)
		}
		return pr
	}
	mkEnv := func(pr *domain.PullRequest, status domain.EnvStatus, createdAt time.Time, ttl time.Duration) *domain.PreviewEnvironment {
		env := &domain.PreviewEnvironment{
			ID:             pr.IdempotencyKey(),
			PullRequestID:  pr.ID,
			IdempotencyKey: pr.IdempotencyKey(),
			Status:         status,
			CreatedAt:      createdAt,
			TTL:            domain.TTL{Duration: ttl},
		}
		if err := envRepo.Save(ctx, env); err != nil {
			t.Fatalf("save env %s: %v", pr.ID, err)
		}
		return env
	}

	mkEnv(mkPR("PR_1", "c1"), domain.EnvRunning, time.Now(), time.Hour)
	mkEnv(mkPR("PR_2", "c2"), domain.EnvProvisioning, time.Now(), time.Hour)

	// Quota: PROVISIONING + RUNNING đều tính, và phải lọc đúng theo repo.
	n, err := envRepo.CountActiveByRepo(ctx, "org/app")
	if err != nil {
		t.Fatalf("CountActiveByRepo: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 env active, got %d", n)
	}
	if n, _ := envRepo.CountActiveByRepo(ctx, "other/repo"); n != 0 {
		t.Fatalf("repo khác phải là 0, got %d", n)
	}

	// TTL sweep: chỉ env RUNNING đã quá created_at + ttl.
	prX := mkPR("PR_X", "cx")
	mkEnv(prX, domain.EnvRunning, time.Now().Add(-48*time.Hour), time.Hour)
	expired, err := envRepo.FindExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("FindExpired: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != prX.IdempotencyKey() {
		t.Fatalf("want đúng 1 env hết hạn (PR_X), got %+v", expired)
	}

	// Đánh dấu xoá -> round-trip cột nullable marked_for_deletion_at.
	e := expired[0]
	e.MarkForDeletion(time.Now())
	if err := envRepo.Save(ctx, e); err != nil {
		t.Fatalf("Save (marked): %v", err)
	}
	marked, err := envRepo.FindMarkedForDeletion(ctx, time.Now())
	if err != nil {
		t.Fatalf("FindMarkedForDeletion: %v", err)
	}
	if len(marked) != 1 || marked[0].MarkedForDeletionAt == nil {
		t.Fatalf("want 1 env chờ xoá kèm timestamp, got %+v", marked)
	}
	// env đã đánh dấu không còn được coi là "expired/RUNNING" nữa.
	if again, _ := envRepo.FindExpired(ctx, time.Now()); len(again) != 0 {
		t.Fatalf("env đã MARK_FOR_DELETION không được xuất hiện lại trong FindExpired, got %+v", again)
	}

	// FindByIdempotencyKey round-trip đầy đủ field.
	got, err := envRepo.FindByIdempotencyKey(ctx, "org/app:PR_1:c1")
	if err != nil || got == nil {
		t.Fatalf("FindByIdempotencyKey: got=%+v err=%v", got, err)
	}
	if got.Status != domain.EnvRunning || got.TTL.Duration != time.Hour || got.PullRequestID != "PR_1" {
		t.Fatalf("env round-trip sai: %+v", got)
	}
}
