package domain_test

import (
	"testing"

	"preview-env-orchestrator/internal/domain"
)

// TestPullRequest_IdempotencyKey: khóa idempotency phải ổn định cho cùng
// (Repo, ID, CommitHash) và đổi khi commit đổi — đây là cơ sở chống xử lý
// trùng webhook.
func TestPullRequest_IdempotencyKey(t *testing.T) {
	base := domain.PullRequest{ID: "PR_1", Repo: "org/app", Number: 42, Author: "alice", CommitHash: "abc123"}

	if got, want := base.IdempotencyKey(), "org/app:PR_1:abc123"; got != want {
		t.Errorf("IdempotencyKey() = %q, muốn %q", got, want)
	}

	same := base
	if base.IdempotencyKey() != same.IdempotencyKey() {
		t.Errorf("cùng dữ liệu phải cho cùng key")
	}

	newCommit := base
	newCommit.CommitHash = "def456"
	if base.IdempotencyKey() == newCommit.IdempotencyKey() {
		t.Errorf("commit khác nhau phải cho key khác nhau")
	}

	newRepo := base
	newRepo.Repo = "org/other"
	if base.IdempotencyKey() == newRepo.IdempotencyKey() {
		t.Errorf("repo khác nhau phải cho key khác nhau")
	}
}
