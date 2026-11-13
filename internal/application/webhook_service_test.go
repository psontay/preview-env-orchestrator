package application_test

import (
	"context"
	"testing"
	"time"

	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
	dockerinfra "preview-env-orchestrator/internal/infrastructure/docker"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

func TestHandlePullRequestEvent_IdempotentOnDuplicateWebhook(t *testing.T) {
	prRepo := memory.NewPullRequestRepository()
	envRepo := memory.NewEnvironmentRepository()
	dockerSvc := dockerinfra.NewMockService()
	notifier := memory.NewGitHubNotifier()
	quota := domain.Quota{MaxEnvironments: 5}
	ttl := domain.TTL{Duration: time.Hour}

	svc := application.NewWebhookService(prRepo, envRepo, dockerSvc, notifier, quota, ttl, "http://localhost:8080")

	pr := &domain.PullRequest{ID: "PR_1", Repo: "org/app", Number: 42, Author: "alice", CommitHash: "abc123"}

	// Gửi webhook 2 lần liên tiếp cho cùng 1 commit (mô phỏng GitHub bắn trùng).
	if err := svc.HandlePullRequestEvent(context.Background(), pr); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if err := svc.HandlePullRequestEvent(context.Background(), pr); err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}

	// Đợi provisioning bất đồng bộ hoàn tất.
	svc.Wait()

	env, err := envRepo.FindByIdempotencyKey(context.Background(), pr.IdempotencyKey())
	if err != nil {
		t.Fatalf("lookup error: %v", err)
	}
	if env == nil {
		t.Fatal("expected an environment to exist")
	}
	if env.Status != domain.EnvRunning {
		t.Fatalf("expected status RUNNING, got %s", env.Status)
	}
}

func TestHandlePullRequestEvent_QuotaExceeded(t *testing.T) {
	prRepo := memory.NewPullRequestRepository()
	envRepo := memory.NewEnvironmentRepository()
	dockerSvc := dockerinfra.NewMockService()
	notifier := memory.NewGitHubNotifier()
	quota := domain.Quota{MaxEnvironments: 1}
	ttl := domain.TTL{Duration: time.Hour}

	svc := application.NewWebhookService(prRepo, envRepo, dockerSvc, notifier, quota, ttl, "http://localhost:8080")

	first := &domain.PullRequest{ID: "PR_1", Repo: "org/app", Number: 1, CommitHash: "sha1"}
	second := &domain.PullRequest{ID: "PR_2", Repo: "org/app", Number: 2, CommitHash: "sha2"}

	if err := svc.HandlePullRequestEvent(context.Background(), first); err != nil {
		t.Fatalf("first PR should succeed: %v", err)
	}
	svc.Wait()

	err := svc.HandlePullRequestEvent(context.Background(), second)
	if err != application.ErrQuotaExceeded {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
}

func TestCleanupService_MarksExpiredThenDestroysAfterGracePeriod(t *testing.T) {
	envRepo := memory.NewEnvironmentRepository()
	prRepo := memory.NewPullRequestRepository()
	dockerSvc := dockerinfra.NewMockService()
	notifier := memory.NewGitHubNotifier()

	cleanup := application.NewCleanupService(envRepo, prRepo, dockerSvc, notifier, "http://localhost:8080")

	// Tạo thẳng 1 environment đã "hết hạn từ lâu" để kiểm tra luồng TTL.
	expiredEnv := &domain.PreviewEnvironment{
		ID:             "env-1",
		IdempotencyKey: "key-1",
		Status:         domain.EnvRunning,
		CreatedAt:      time.Now().Add(-48 * time.Hour),
		TTL:            domain.TTL{Duration: time.Hour},
		ContainerID:    "container-1",
	}
	if err := envRepo.Save(context.Background(), expiredEnv); err != nil {
		t.Fatalf("save error: %v", err)
	}

	cleanup.Run(context.Background())

	got, _ := envRepo.FindByIdempotencyKey(context.Background(), "key-1")
	if got.Status != domain.EnvMarkedForDeletion {
		t.Fatalf("expected MARK_FOR_DELETION right after first sweep, got %s", got.Status)
	}

	// Chưa đủ grace period (10 phút) -> lần quét thứ 2 ngay sau đó vẫn CHƯA bị huỷ thật.
	cleanup.Run(context.Background())
	got, _ = envRepo.FindByIdempotencyKey(context.Background(), "key-1")
	if got.Status != domain.EnvMarkedForDeletion {
		t.Fatalf("expected still MARK_FOR_DELETION before grace period elapses, got %s", got.Status)
	}
}
