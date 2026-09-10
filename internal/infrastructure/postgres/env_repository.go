package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"preview-env-orchestrator/internal/domain"
)

// scannable là giao diện chung của pgx.Row và pgx.Rows — cho phép dùng lại
// một hàm scan cho cả QueryRow lẫn Query.
type scannable interface {
	Scan(dest ...any) error
}

// EnvironmentRepository là cài đặt Postgres của domain.EnvironmentRepository.
type EnvironmentRepository struct {
	pool *pgxpool.Pool
}

func NewEnvironmentRepository(pool *pgxpool.Pool) *EnvironmentRepository {
	return &EnvironmentRepository{pool: pool}
}

var _ domain.EnvironmentRepository = (*EnvironmentRepository)(nil)

const envColumns = `id, pull_request_id, idempotency_key, status, url, container_id, created_at, ttl_seconds, marked_for_deletion_at`

// TryAcquireLock hiện thực Idempotency ở tầng lưu trữ bằng ĐÚNG MỘT statement:
// INSERT ... ON CONFLICT (idempotency_key) DO NOTHING, rồi xét RowsAffected().
// Không SELECT-rồi-INSERT riêng lẻ (sẽ có race giữa check và insert).
// RowsAffected()==1  -> ta vừa chèn được key này  -> được phép xử lý.
// RowsAffected()==0  -> key đã tồn tại            -> tiến trình khác đang/đã xử lý.
func (r *EnvironmentRepository) TryAcquireLock(ctx context.Context, key string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`INSERT INTO idempotency_keys (idempotency_key) VALUES ($1)
		 ON CONFLICT (idempotency_key) DO NOTHING`, key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *EnvironmentRepository) FindByIdempotencyKey(ctx context.Context, key string) (*domain.PreviewEnvironment, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+envColumns+` FROM preview_environments WHERE idempotency_key = $1`, key)

	env, err := scanEnv(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return env, nil
}

// Save upsert theo khóa chính id. Cột `repo` được denormalize bằng subquery
// từ pull_requests (PR luôn được lưu trước env trong WebhookService), để
// CountActiveByRepo truy vấn nhanh qua index idx_env_repo_status mà không JOIN.
func (r *EnvironmentRepository) Save(ctx context.Context, env *domain.PreviewEnvironment) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO preview_environments
			(id, pull_request_id, idempotency_key, repo, status, url, container_id, created_at, ttl_seconds, marked_for_deletion_at)
		VALUES
			($1, $2, $3, (SELECT repo FROM pull_requests WHERE id = $2), $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			status                 = EXCLUDED.status,
			url                    = EXCLUDED.url,
			container_id           = EXCLUDED.container_id,
			marked_for_deletion_at = EXCLUDED.marked_for_deletion_at`,
		env.ID,
		env.PullRequestID,
		env.IdempotencyKey,
		string(env.Status),
		nullIfEmpty(env.URL),
		nullIfEmpty(env.ContainerID),
		env.CreatedAt,
		int(env.TTL.Duration/time.Second),
		env.MarkedForDeletionAt,
	)
	return err
}

// CountActiveByRepo đếm env đang PROVISIONING/RUNNING của một repo, dùng cột
// denormalized `repo` + index idx_env_repo_status. Không JOIN.
func (r *EnvironmentRepository) CountActiveByRepo(ctx context.Context, repo string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM preview_environments
		WHERE repo = $1 AND status IN ($2, $3)`,
		repo, string(domain.EnvProvisioning), string(domain.EnvRunning)).Scan(&n)
	return n, err
}

// FindExpired: env RUNNING mà created_at + ttl đã trôi qua mốc now (khớp
// domain.PreviewEnvironment.IsExpired: now.After(createdAt + ttl)).
func (r *EnvironmentRepository) FindExpired(ctx context.Context, now time.Time) ([]*domain.PreviewEnvironment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+envColumns+` FROM preview_environments
		WHERE status = $1
		  AND created_at + make_interval(secs => ttl_seconds) < $2`,
		string(domain.EnvRunning), now)
	if err != nil {
		return nil, err
	}
	return collectEnvs(rows)
}

// FindMarkedForDeletion: mọi env đang ở trạng thái chờ xoá; việc xét đã đủ
// grace period chưa là trách nhiệm của domain (ReadyToDestroy).
func (r *EnvironmentRepository) FindMarkedForDeletion(ctx context.Context, _ time.Time) ([]*domain.PreviewEnvironment, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+envColumns+` FROM preview_environments WHERE status = $1`,
		string(domain.EnvMarkedForDeletion))
	if err != nil {
		return nil, err
	}
	return collectEnvs(rows)
}

func collectEnvs(rows pgx.Rows) ([]*domain.PreviewEnvironment, error) {
	defer rows.Close()
	var out []*domain.PreviewEnvironment
	for rows.Next() {
		env, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

func scanEnv(s scannable) (*domain.PreviewEnvironment, error) {
	var (
		env         domain.PreviewEnvironment
		status      string
		url         *string
		containerID *string
		ttlSeconds  int
		markedAt    *time.Time
	)
	err := s.Scan(
		&env.ID,
		&env.PullRequestID,
		&env.IdempotencyKey,
		&status,
		&url,
		&containerID,
		&env.CreatedAt,
		&ttlSeconds,
		&markedAt,
	)
	if err != nil {
		return nil, err
	}
	env.Status = domain.EnvStatus(status)
	if url != nil {
		env.URL = *url
	}
	if containerID != nil {
		env.ContainerID = *containerID
	}
	env.TTL = domain.TTL{Duration: time.Duration(ttlSeconds) * time.Second}
	env.MarkedForDeletionAt = markedAt
	return &env, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
