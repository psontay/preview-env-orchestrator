package domain

import "time"

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
)

// PreviewEnvironment là Aggregate Root trung tâm của hệ thống.
// Toàn bộ logic "khi nào được xoá, khi nào hết hạn" nằm NGAY TẠI ĐÂY
// (trong domain), chứ không nằm rải rác ở tầng service hay handler —
// đây là nguyên tắc cốt lõi của DDD: domain tự bảo vệ tính bất biến của nó.
type PreviewEnvironment struct {
	ID                  string
	PullRequestID       string
	IdempotencyKey      string
	Status              EnvStatus
	URL                 string
	ContainerID         string
	CreatedAt           time.Time
	TTL                 TTL
	MarkedForDeletionAt *time.Time
}

// IsExpired kiểm tra xem environment đã vượt quá TTL cho phép chưa.
func (e *PreviewEnvironment) IsExpired(now time.Time) bool {
	return now.After(e.TTL.ExpiresAt(e.CreatedAt))
}

// MarkForDeletion chuyển trạng thái sang "chờ xoá" thay vì xoá ngay lập tức.
// Đây là bước bắt buộc cho yêu cầu "Destructive Safety": luôn có một
// khoảng thời gian ân hạn (grace period) trước khi phá huỷ tài nguyên thật.
func (e *PreviewEnvironment) MarkForDeletion(now time.Time) {
	e.Status = EnvMarkedForDeletion
	t := now
	e.MarkedForDeletionAt = &t
}

// CancelDeletion cho phép huỷ lệnh xoá nếu người dùng bấm "Hủy lệnh"
// trong khoảng grace period (ví dụ: PR có commit mới).
func (e *PreviewEnvironment) CancelDeletion() {
	e.Status = EnvRunning
	e.MarkedForDeletionAt = nil
}

// ReadyToDestroy trả về true chỉ khi environment đang ở trạng thái
// MARK_FOR_DELETION và đã trôi qua đủ gracePeriod kể từ lúc đánh dấu.
func (e *PreviewEnvironment) ReadyToDestroy(now time.Time, gracePeriod time.Duration) bool {
	if e.Status != EnvMarkedForDeletion || e.MarkedForDeletionAt == nil {
		return false
	}
	return now.After(e.MarkedForDeletionAt.Add(gracePeriod))
}
