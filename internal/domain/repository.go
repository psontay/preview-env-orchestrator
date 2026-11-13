package domain

import (
	"context"
	"time"
)

// PullRequestRepository là một "port" — domain định nghĩa interface,
// còn infrastructure (Postgres, in-memory, ...) sẽ implement nó.
// Nhờ vậy domain KHÔNG phụ thuộc ngược vào chi tiết lưu trữ (Dependency
// Inversion Principle trong SOLID).
type PullRequestRepository interface {
	FindByIdempotencyKey(ctx context.Context, key string) (*PullRequest, error)
	Save(ctx context.Context, pr *PullRequest) error
}

// EnvironmentRepository quản lý vòng đời lưu trữ của PreviewEnvironment,
// bao gồm cả cơ chế khóa dùng cho Idempotency.
type EnvironmentRepository interface {
	FindByIdempotencyKey(ctx context.Context, key string) (*PreviewEnvironment, error)
	Save(ctx context.Context, env *PreviewEnvironment) error

	// CountActiveByRepo includes provisioning, running and deletion-pending
	// resources: a slot is released only after destruction or a clean failure.
	CountActiveByRepo(ctx context.Context, repo string) (int, error)

	// FindExpired trả về các environment đã vượt TTL nhưng chưa bị đánh
	// dấu xoá — dùng bởi background worker quét mỗi N phút.
	FindExpired(ctx context.Context, now time.Time) ([]*PreviewEnvironment, error)

	// FindMarkedForDeletion trả về các environment đang ở trạng thái chờ
	// xoá, để worker kiểm tra xem đã hết grace period chưa.
	FindMarkedForDeletion(ctx context.Context, now time.Time) ([]*PreviewEnvironment, error)

	// TryAcquireLock hiện thực hoá tính Idempotency ở tầng lưu trữ.
	// Với Postgres, đây thường là pg_advisory_xact_lock hoặc một bảng
	// idempotency_keys với ràng buộc UNIQUE + INSERT ... ON CONFLICT DO NOTHING.
	// Trả về true nếu lock được cấp (tức request này được phép xử lý),
	// false nếu đã có tiến trình khác đang giữ khóa cho key này.
	TryAcquireLock(ctx context.Context, key string) (bool, error)

	// Reserve atomically checks quota and records the idempotency key + env.
	// A quota rejection must not consume the key; duplicates return false, nil.
	Reserve(ctx context.Context, env *PreviewEnvironment, quota Quota) (bool, error)
	CancelDeletion(ctx context.Context, key string, now time.Time, grace time.Duration) (*PreviewEnvironment, error)
	// RecordDeletionNotice updates only the matching, still-pending mark. A
	// concurrent cancellation must never be overwritten by notification delivery.
	RecordDeletionNotice(ctx context.Context, key string, markedAt, notifiedAt time.Time) (*PreviewEnvironment, error)
	ClaimDestruction(ctx context.Context, key string, now time.Time, grace time.Duration) (*PreviewEnvironment, error)
	FindProvisioning(ctx context.Context) ([]*PreviewEnvironment, error)
	RecoverDestructions(ctx context.Context) error
	// Pending completion notices survive restarts without holding quota.
	FindPendingDestructionNotices(ctx context.Context) ([]*PreviewEnvironment, error)
	ConfirmDestructionNotice(ctx context.Context, key string) error
}
