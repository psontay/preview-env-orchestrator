package memory

import (
	"context"
	"strings"
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
	return copyEnv(env), nil
}

func (r *EnvironmentRepository) Save(_ context.Context, env *domain.PreviewEnvironment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[env.IdempotencyKey] = copyEnv(env)
	return nil
}

func (r *EnvironmentRepository) CountActiveByRepo(_ context.Context, repo string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, env := range r.byKey {
		if env.CountsTowardsQuota() && (env.Repo == repo || env.Repo == "" && strings.HasPrefix(env.IdempotencyKey, repo+":")) {
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
			out = append(out, copyEnv(env))
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
			out = append(out, copyEnv(env))
		}
	}
	return out, nil
}

func copyEnv(env *domain.PreviewEnvironment) *domain.PreviewEnvironment {
	if env == nil {
		return nil
	}
	result := *env
	if env.MarkedForDeletionAt != nil {
		t := *env.MarkedForDeletionAt
		result.MarkedForDeletionAt = &t
	}
	if env.ExpiresAt != nil {
		t := *env.ExpiresAt
		result.ExpiresAt = &t
	}
	if env.DeletionNotifiedAt != nil {
		t := *env.DeletionNotifiedAt
		result.DeletionNotifiedAt = &t
	}
	return &result
}

func (r *EnvironmentRepository) Reserve(ctx context.Context, env *domain.PreviewEnvironment, quota domain.Quota) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.byKey[env.IdempotencyKey]; old != nil && old.Status != domain.EnvFailed {
		return false, nil
	}
	count := 0
	for _, existing := range r.byKey {
		if existing.Repo == env.Repo && existing.CountsTowardsQuota() {
			count++
		}
	}
	if quota.IsExceeded(count) {
		return false, domain.ErrQuotaExceeded
	}
	r.locked[env.IdempotencyKey] = true
	r.byKey[env.IdempotencyKey] = copyEnv(env)
	return true, nil
}

func (r *EnvironmentRepository) CancelDeletion(_ context.Context, key string, now time.Time, grace time.Duration) (*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	env := r.byKey[key]
	if env == nil {
		return nil, domain.ErrEnvironmentNotFound
	}
	if err := env.ExtendOnce(now, grace); err != nil {
		return nil, err
	}
	return copyEnv(env), nil
}

func (r *EnvironmentRepository) RecordDeletionNotice(_ context.Context, key string, markedAt, notifiedAt time.Time) (*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	env := r.byKey[key]
	if env == nil || env.Status != domain.EnvMarkedForDeletion || env.MarkedForDeletionAt == nil ||
		!env.MarkedForDeletionAt.Equal(markedAt) || env.DeletionNotifiedAt != nil {
		return nil, nil
	}
	env.DeletionNotifiedAt = &notifiedAt
	return copyEnv(env), nil
}

func (r *EnvironmentRepository) ClaimDestruction(_ context.Context, key string, now time.Time, grace time.Duration) (*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	env := r.byKey[key]
	if env == nil || !env.ReadyToDestroy(now, grace) {
		return nil, nil
	}
	env.Status = domain.EnvDestroying
	return copyEnv(env), nil
}

func (r *EnvironmentRepository) FindProvisioning(_ context.Context) ([]*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.PreviewEnvironment
	for _, env := range r.byKey {
		if env.Status == domain.EnvProvisioning {
			out = append(out, copyEnv(env))
		}
	}
	return out, nil
}

func (r *EnvironmentRepository) RecoverDestructions(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, env := range r.byKey {
		if env.Status == domain.EnvDestroying {
			env.Status = domain.EnvMarkedForDeletion
		}
	}
	return nil
}

func (r *EnvironmentRepository) FindPendingDestructionNotices(_ context.Context) ([]*domain.PreviewEnvironment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.PreviewEnvironment
	for _, env := range r.byKey {
		if env.Status == domain.EnvDestroyed && env.DestructionNoticePending {
			out = append(out, copyEnv(env))
		}
	}
	return out, nil
}

func (r *EnvironmentRepository) ConfirmDestructionNotice(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if env := r.byKey[key]; env != nil && env.Status == domain.EnvDestroyed {
		env.DestructionNoticePending = false
	}
	return nil
}
