package domain

import "time"

// Quota là một Value Object bất biến, mô tả giới hạn số lượng
// PreviewEnvironment đang hoạt động đồng thời cho một repository.
type Quota struct {
	MaxEnvironments int
}

// IsExceeded trả về true nếu số lượng environment đang hoạt động (current)
// đã đạt hoặc vượt giới hạn cho phép.
func (q Quota) IsExceeded(current int) bool {
	return current >= q.MaxEnvironments
}

// TTL (Time-To-Live) là một Value Object mô tả thời gian sống tối đa
// của một PreviewEnvironment kể từ lúc tạo.
type TTL struct {
	Duration time.Duration
}

// ExpiresAt tính thời điểm hết hạn dựa trên thời điểm bắt đầu (from).
func (t TTL) ExpiresAt(from time.Time) time.Time {
	return from.Add(t.Duration)
}
