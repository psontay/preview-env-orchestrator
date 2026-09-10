package memory

import (
	"context"
	"sync"
	"time"

	"preview-env-orchestrator/internal/domain"
)

// EnvironmentRepository là cài đặt in-memory của domain.EnvironmentRepository.
// TryAcquireLock mô phỏng hành vi của một Postgres advisory lock / bảng
// idempotency_keys UNIQUE bằng một map + mutex: chỉ goroutine đầu tiên
// gọi TryAcquireLock cho một key mới nhận được true.
type EnvironmentRepository struct {
	mu     sync.Mutex
	byKey  map[string]*domain.PreviewEnvironment
	locked map[string]bool
}

func NewEnvironmentRepository() *EnvironmentRepository {
	return &EnvironmentRepository{
		byKey:  make(map[string]*domain.PreviewEnvironment),
		locked: make(map[string]bool),
	}
}

func (r *EnvironmentRepository) TryAcquireLock(_ context.Context, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked[key] {
		return false, nil
	}
	r.locked[key] = true
	return true, nil
}

func (r *EnvironmentRepository) FindByIdempotencyKey(_ context.Context, key string) (*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	env, ok := r.byKey[key]
	if !ok {
		return nil, nil
	}
	return env, nil
}

func (r *EnvironmentRepository) Save(_ context.Context, env *domain.PreviewEnvironment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[env.IdempotencyKey] = env
	return nil
}

func (r *EnvironmentRepository) CountActiveByRepo(_ context.Context, repo string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, env := range r.byKey {
		if env.Status == domain.EnvProvisioning || env.Status == domain.EnvRunning {
			// Trong bản in-memory này ta không lưu repo trực tiếp trên
			// PreviewEnvironment (repo nằm ở PullRequest); bản Postgres
			// thật nên JOIN hoặc denormalize cột `repo` để truy vấn nhanh.
			_ = repo
			count++
		}
	}
	return count, nil
}

func (r *EnvironmentRepository) FindExpired(_ context.Context, now time.Time) ([]*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.PreviewEnvironment
	for _, env := range r.byKey {
		if env.Status == domain.EnvRunning && env.IsExpired(now) {
			out = append(out, env)
		}
	}
	return out, nil
}

func (r *EnvironmentRepository) FindMarkedForDeletion(_ context.Context, now time.Time) ([]*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.PreviewEnvironment
	for _, env := range r.byKey {
		if env.Status == domain.EnvMarkedForDeletion {
			out = append(out, env)
		}
	}
	return out, nil
}
