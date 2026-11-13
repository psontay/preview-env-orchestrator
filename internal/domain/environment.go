package domain

import (
	"errors"
	"time"
)

var (
	ErrQuotaExceeded        = errors.New("quota exceeded for this repository")
	ErrEnvironmentNotFound  = errors.New("environment not found")
	ErrCannotCancelDeletion = errors.New("deletion cannot be cancelled")
)

// EnvStatus mô tả các trạng thái hợp lệ trong vòng đời của một
// PreviewEnvironment. Việc dùng type riêng (thay vì string trần) giúp
// compiler bắt lỗi nếu gán nhầm giá trị không hợp lệ.
type EnvStatus string

const (
	EnvProvisioning      EnvStatus = "PROVISIONING"
	EnvRunning           EnvStatus = "RUNNING"
	EnvMarkedForDeletion EnvStatus = "MARK_FOR_DELETION"
	EnvDestroyed         EnvStatus = "DESTROYED"
	EnvFailed            EnvStatus = "FAILED"
	EnvDestroying        EnvStatus = "DESTROYING"
)

// PreviewEnvironment là Aggregate Root trung tâm của hệ thống.
// Toàn bộ logic "khi nào được xoá, khi nào hết hạn" nằm NGAY TẠI ĐÂY
// (trong domain), chứ không nằm rải rác ở tầng service hay handler —
// đây là nguyên tắc cốt lõi của DDD: domain tự bảo vệ tính bất biến của nó.
type PreviewEnvironment struct {
	ID                       string
	PullRequestID            string
	IdempotencyKey           string
	Status                   EnvStatus
	URL                      string
	ContainerID              string
	ResourceGroup            string
	CreatedAt                time.Time
	TTL                      TTL
	MarkedForDeletionAt      *time.Time
	DeletionNotifiedAt       *time.Time
	DestructionNoticePending bool
	Repo                     string
	CommitSHA                string
	ExpiresAt                *time.Time
	DeletionCancelCount      int
	LastError                string
}

// IsExpired kiểm tra xem environment đã vượt quá TTL cho phép chưa.
func (e *PreviewEnvironment) IsExpired(now time.Time) bool {
	if e.ExpiresAt != nil {
		return now.After(*e.ExpiresAt)
	}
	return now.After(e.TTL.ExpiresAt(e.CreatedAt))
}

// ExtendOnce cancels deletion only before the grace deadline. The second TTL
// expiry follows the same mark/grace/destroy lifecycle, without another cancel.
func (e *PreviewEnvironment) ExtendOnce(now time.Time, grace time.Duration) error {
	deadline := e.DeletionDeadline(grace)
	if e.Status != EnvMarkedForDeletion || e.MarkedForDeletionAt == nil ||
		e.DeletionCancelCount != 0 || e.LastError != "" || deadline != nil && !now.Before(*deadline) {
		return ErrCannotCancelDeletion
	}
	e.CancelDeletion()
	expires := now.Add(2 * time.Minute)
	e.ExpiresAt = &expires
	e.DeletionCancelCount++
	return nil
}

func (e *PreviewEnvironment) CountsTowardsQuota() bool {
	return e.Status == EnvProvisioning || e.Status == EnvRunning ||
		e.Status == EnvMarkedForDeletion || e.Status == EnvDestroying
}

// MarkForDeletion chuyển trạng thái sang "chờ xoá" thay vì xoá ngay lập tức.
// Đây là bước bắt buộc cho yêu cầu "Destructive Safety": luôn có một
// khoảng thời gian ân hạn (grace period) trước khi phá huỷ tài nguyên thật.
func (e *PreviewEnvironment) MarkForDeletion(now time.Time) {
	e.Status = EnvMarkedForDeletion
	t := now
	e.MarkedForDeletionAt = &t
	e.DeletionNotifiedAt = nil
}

// CancelDeletion cho phép huỷ lệnh xoá nếu người dùng bấm "Hủy lệnh"
// trong khoảng grace period (ví dụ: PR có commit mới).
func (e *PreviewEnvironment) CancelDeletion() {
	e.Status = EnvRunning
	e.MarkedForDeletionAt = nil
	e.DeletionNotifiedAt = nil
}

// DeletionDeadline starts grace only after the warning has been delivered.
// While delivery is pending, deletion is blocked and cancellation stays open.
func (e *PreviewEnvironment) DeletionDeadline(grace time.Duration) *time.Time {
	if e.MarkedForDeletionAt == nil || e.DeletionNotifiedAt == nil {
		return nil
	}
	start := *e.MarkedForDeletionAt
	if e.DeletionNotifiedAt.After(start) {
		start = *e.DeletionNotifiedAt
	}
	deadline := start.Add(grace)
	return &deadline
}

// ReadyToDestroy requires a delivered warning and a full grace period.
func (e *PreviewEnvironment) ReadyToDestroy(now time.Time, gracePeriod time.Duration) bool {
	deadline := e.DeletionDeadline(gracePeriod)
	if e.Status != EnvMarkedForDeletion || deadline == nil {
		return false
	}
	return now.After(*deadline)
}
