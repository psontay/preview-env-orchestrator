package domain_test

import (
	"testing"
	"time"

	"preview-env-orchestrator/internal/domain"
)

// newEnv dựng một PreviewEnvironment tối thiểu để test, tạo lúc createdAt
// với TTL cho trước.
func newEnv(createdAt time.Time, ttl time.Duration) *domain.PreviewEnvironment {
	return &domain.PreviewEnvironment{
		ID:             "env_1",
		PullRequestID:  "PR_1",
		IdempotencyKey: "org/app:PR_1:abc123",
		Status:         domain.EnvRunning,
		CreatedAt:      createdAt,
		TTL:            domain.TTL{Duration: ttl},
	}
}

// TestPreviewEnvironment_IsExpired: chưa tới hạn thì false, quá hạn thì true,
// đúng ngay thời điểm hết hạn thì chưa coi là expired (dùng After, không phải !Before).
func TestPreviewEnvironment_IsExpired(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newEnv(created, time.Hour)

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"còn trong hạn", created.Add(30 * time.Minute), false},
		{"đúng thời điểm hết hạn", created.Add(time.Hour), false},
		{"đã quá hạn", created.Add(time.Hour + time.Nanosecond), true},
		{"quá hạn nhiều", created.Add(3 * time.Hour), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := env.IsExpired(tc.now); got != tc.want {
				t.Errorf("IsExpired(%v) = %v, muốn %v", tc.now, got, tc.want)
			}
		})
	}
}

// TestPreviewEnvironment_MarkForDeletion: chuyển trạng thái sang MARK_FOR_DELETION
// và ghi lại mốc thời gian đánh dấu.
func TestPreviewEnvironment_MarkForDeletion(t *testing.T) {
	env := newEnv(time.Now(), time.Hour)
	now := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)

	env.MarkForDeletion(now)

	if env.Status != domain.EnvMarkedForDeletion {
		t.Errorf("Status = %q, muốn %q", env.Status, domain.EnvMarkedForDeletion)
	}
	if env.MarkedForDeletionAt == nil {
		t.Fatal("MarkedForDeletionAt phải được set, đang nil")
	}
	if !env.MarkedForDeletionAt.Equal(now) {
		t.Errorf("MarkedForDeletionAt = %v, muốn %v", *env.MarkedForDeletionAt, now)
	}
}

// TestPreviewEnvironment_CancelDeletion: huỷ lệnh xoá đưa env về RUNNING và
// xoá mốc đánh dấu — sau đó ReadyToDestroy phải trả về false.
func TestPreviewEnvironment_CancelDeletion(t *testing.T) {
	env := newEnv(time.Now(), time.Hour)
	markAt := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	env.MarkForDeletion(markAt)

	env.CancelDeletion()

	if env.Status != domain.EnvRunning {
		t.Errorf("Status = %q, muốn %q", env.Status, domain.EnvRunning)
	}
	if env.MarkedForDeletionAt != nil {
		t.Errorf("MarkedForDeletionAt phải nil sau CancelDeletion, đang %v", *env.MarkedForDeletionAt)
	}

	// Dù thời gian trôi qua bao lâu, env đã huỷ lệnh xoá không được destroy.
	if env.ReadyToDestroy(markAt.Add(24*time.Hour), time.Minute) {
		t.Errorf("sau CancelDeletion, ReadyToDestroy phải trả về false")
	}
}

// TestPreviewEnvironment_ReadyToDestroy phủ đủ các nhánh của quy tắc
// "grace period trước khi phá huỷ".
func TestPreviewEnvironment_ReadyToDestroy(t *testing.T) {
	markAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	const grace = 10 * time.Minute

	t.Run("chưa bị đánh dấu xoá (RUNNING)", func(t *testing.T) {
		env := newEnv(time.Now(), time.Hour)
		if env.ReadyToDestroy(markAt.Add(time.Hour), grace) {
			t.Error("env RUNNING không bao giờ ReadyToDestroy")
		}
	})

	t.Run("đã đánh dấu nhưng chưa hết grace period", func(t *testing.T) {
		env := newEnv(time.Now(), time.Hour)
		env.MarkForDeletion(markAt)
		if env.ReadyToDestroy(markAt.Add(5*time.Minute), grace) {
			t.Error("chưa hết grace period thì chưa được destroy")
		}
	})

	t.Run("đúng thời điểm hết grace period (chưa quá)", func(t *testing.T) {
		env := newEnv(time.Now(), time.Hour)
		env.MarkForDeletion(markAt)
		if env.ReadyToDestroy(markAt.Add(grace), grace) {
			t.Error("đúng mốc hết hạn dùng After nên chưa được destroy")
		}
	})

	t.Run("đã qua grace period", func(t *testing.T) {
		env := newEnv(time.Now(), time.Hour)
		env.MarkForDeletion(markAt)
		if !env.ReadyToDestroy(markAt.Add(grace+time.Nanosecond), grace) {
			t.Error("đã qua grace period thì phải ReadyToDestroy")
		}
	})

	t.Run("status MARK_FOR_DELETION nhưng thiếu mốc thời gian", func(t *testing.T) {
		env := newEnv(time.Now(), time.Hour)
		env.Status = domain.EnvMarkedForDeletion
		env.MarkedForDeletionAt = nil
		if env.ReadyToDestroy(markAt.Add(24*time.Hour), grace) {
			t.Error("thiếu MarkedForDeletionAt thì không được destroy")
		}
	})

	t.Run("gọi CancelDeletion giữa chừng rồi mới quét", func(t *testing.T) {
		env := newEnv(time.Now(), time.Hour)
		env.MarkForDeletion(markAt)
		env.CancelDeletion()
		if env.ReadyToDestroy(markAt.Add(grace+time.Hour), grace) {
			t.Error("sau CancelDeletion thì không được destroy dù đã quá grace period")
		}
	})
}
