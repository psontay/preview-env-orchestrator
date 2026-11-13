// Package memory chứa các cài đặt (adapter) in-memory cho các repository
// interface định nghĩa ở internal/domain. Dùng để chạy demo / unit test
// mà không cần một Postgres thật. Khi lên production, thay bằng package
// postgres tương ứng (xem docs/POSTGRES_INTEGRATION.md).
package memory

import (
	"context"
	"sync"

	"preview-env-orchestrator/internal/domain"
)

type PullRequestRepository struct {
	mu    sync.RWMutex
	byKey map[string]*domain.PullRequest
}

func NewPullRequestRepository() *PullRequestRepository {
	return &PullRequestRepository{byKey: make(map[string]*domain.PullRequest)}
}

func (r *PullRequestRepository) FindByIdempotencyKey(_ context.Context, key string) (*domain.PullRequest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pr, ok := r.byKey[key]
	if !ok {
		return nil, nil
	}
	result := *pr
	return &result, nil
}

func (r *PullRequestRepository) Save(_ context.Context, pr *domain.PullRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := *pr
	r.byKey[pr.IdempotencyKey()] = &result
	return nil
}
