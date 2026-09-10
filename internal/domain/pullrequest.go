package domain

// PRStatus mô tả trạng thái của một Pull Request trên GitHub.
type PRStatus string

const (
	PRStatusOpen   PRStatus = "OPEN"
	PRStatusClosed PRStatus = "CLOSED"
	PRStatusMerged PRStatus = "MERGED"
)

// PullRequest là Aggregate Root chứa thông tin cốt lõi của một PR.
// Nó không biết gì về Docker hay HTTP — chỉ chứa dữ liệu + quy tắc nghiệp vụ
// liên quan trực tiếp tới chính nó.
type PullRequest struct {
	ID         string // ID nội bộ của GitHub PR (node_id) hoặc repo+number
	Repo       string // ví dụ: "org/service-a"
	Number     int
	Author     string
	CommitHash string
	Status     PRStatus
}

// IdempotencyKey sinh khóa duy nhất cho một sự kiện (Repo + PR + Commit).
// Đây chính là "chìa khóa" để chống xử lý trùng lặp khi GitHub gửi
// webhook nhiều lần cho cùng một commit.
func (pr *PullRequest) IdempotencyKey() string {
	return pr.Repo + ":" + pr.ID + ":" + pr.CommitHash
}
