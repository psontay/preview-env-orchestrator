package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"preview-env-orchestrator/internal/domain"
)

// PullRequestRepository là cài đặt Postgres của domain.PullRequestRepository.
type PullRequestRepository struct {
	pool *pgxpool.Pool
}

func NewPullRequestRepository(pool *pgxpool.Pool) *PullRequestRepository {
	return &PullRequestRepository{pool: pool}
}

var _ domain.PullRequestRepository = (*PullRequestRepository)(nil)

const prColumns = `id, repo, number, author, commit_hash, status`

// FindByIdempotencyKey dùng cột generated idempotency_key (migration 002),
// khớp đúng công thức repo:id:commit_hash trong domain.PullRequest.IdempotencyKey().
func (r *PullRequestRepository) FindByIdempotencyKey(ctx context.Context, key string) (*domain.PullRequest, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+prColumns+` FROM pull_requests WHERE idempotency_key = $1`, key)

	pr, err := scanPR(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pr, nil
}

// Save upsert theo khóa chính id (GitHub node_id): webhook "synchronize" có
// thể gửi lại cùng PR với commit mới.
func (r *PullRequestRepository) Save(ctx context.Context, pr *domain.PullRequest) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO pull_requests (id, repo, number, author, commit_hash, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			repo        = EXCLUDED.repo,
			number      = EXCLUDED.number,
			author      = EXCLUDED.author,
			commit_hash = EXCLUDED.commit_hash,
			status      = EXCLUDED.status`,
		pr.ID, pr.Repo, pr.Number, pr.Author, pr.CommitHash, string(pr.Status))
	return err
}

func scanPR(s scannable) (*domain.PullRequest, error) {
	var (
		pr     domain.PullRequest
		status string
	)
	if err := s.Scan(&pr.ID, &pr.Repo, &pr.Number, &pr.Author, &pr.CommitHash, &status); err != nil {
		return nil, err
	}
	pr.Status = domain.PRStatus(status)
	return &pr, nil
}
