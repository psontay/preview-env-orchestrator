package domain_test

import (
	"testing"
	"time"

	"preview-env-orchestrator/internal/domain"
)

// TestQuota_IsExceeded kiểm tra ranh giới của Quota: dưới hạn thì cho phép,
// bằng hạn hoặc vượt hạn thì chặn (>= chứ không phải >).
func TestQuota_IsExceeded(t *testing.T) {
	q := domain.Quota{MaxEnvironments: 3}

	cases := []struct {
		name    string
		current int
		want    bool
	}{
		{"chưa có environment nào", 0, false},
		{"còn slot trống", 2, false},
		{"đúng bằng giới hạn", 3, true},
		{"đã vượt giới hạn", 4, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := q.IsExceeded(tc.current); got != tc.want {
				t.Errorf("IsExceeded(%d) = %v, muốn %v", tc.current, got, tc.want)
			}
		})
	}
}

// TestQuota_IsExceeded_ZeroLimit: quota = 0 nghĩa là không cho tạo environment nào.
func TestQuota_IsExceeded_ZeroLimit(t *testing.T) {
	q := domain.Quota{MaxEnvironments: 0}
	if !q.IsExceeded(0) {
		t.Errorf("với MaxEnvironments=0, IsExceeded(0) phải trả về true")
	}
}

// TestTTL_ExpiresAt kiểm tra thời điểm hết hạn = thời điểm bắt đầu + Duration.
func TestTTL_ExpiresAt(t *testing.T) {
	from := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		ttl  domain.TTL
		want time.Time
	}{
		{"TTL 1 giờ", domain.TTL{Duration: time.Hour}, from.Add(time.Hour)},
		{"TTL 30 phút", domain.TTL{Duration: 30 * time.Minute}, from.Add(30 * time.Minute)},
		{"TTL 0 = hết hạn ngay", domain.TTL{Duration: 0}, from},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ttl.ExpiresAt(from); !got.Equal(tc.want) {
				t.Errorf("ExpiresAt(%v) = %v, muốn %v", from, got, tc.want)
			}
		})
	}
}
