package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"preview-env-orchestrator/internal/api/handlers"
	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

// newCancelMux dựng đúng route như trong cmd/api/main.go để test đi qua
// cả cơ chế PathValue của http.ServeMux (Go 1.22).
func newCancelMux(envRepo *memory.EnvironmentRepository) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /environments/{id}/cancel-deletion", handlers.NewCancelDeletionHandler(envRepo))
	return mux
}

func postCancel(t *testing.T, mux *http.ServeMux, id string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/environments/"+url.PathEscape(id)+"/cancel-deletion", nil)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCancelDeletion_MarkedEnvGoesBackToRunning(t *testing.T) {
	ctx := context.Background()
	envRepo := memory.NewEnvironmentRepository()

	env := &domain.PreviewEnvironment{
		ID:             "env-1",
		IdempotencyKey: "env-1",
		Status:         domain.EnvRunning,
		CreatedAt:      time.Now(),
		TTL:            domain.TTL{Duration: time.Hour},
	}
	env.MarkForDeletion(time.Now())
	if err := envRepo.Save(ctx, env); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := postCancel(t, newCancelMux(envRepo), "env-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	got, _ := envRepo.FindByIdempotencyKey(ctx, "env-1")
	if got.Status != domain.EnvRunning {
		t.Fatalf("want status RUNNING, got %s", got.Status)
	}
	if got.MarkedForDeletionAt != nil {
		t.Fatalf("want MarkedForDeletionAt cleared after cancel")
	}
}

func TestCancelDeletion_HandlesSlashInID(t *testing.T) {
	ctx := context.Background()
	envRepo := memory.NewEnvironmentRepository()

	// idempotency key thật có dạng "owner/repo:pr_id:commit" (chứa "/").
	id := "org/app:PR_9:deadbeef"
	env := &domain.PreviewEnvironment{
		ID:             id,
		IdempotencyKey: id,
		Status:         domain.EnvRunning,
		CreatedAt:      time.Now(),
		TTL:            domain.TTL{Duration: time.Hour},
	}
	env.MarkForDeletion(time.Now())
	if err := envRepo.Save(ctx, env); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := postCancel(t, newCancelMux(envRepo), id)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for percent-encoded id, got %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := envRepo.FindByIdempotencyKey(ctx, id)
	if got.Status != domain.EnvRunning {
		t.Fatalf("want RUNNING, got %s", got.Status)
	}
}

func TestCancelDeletion_UnknownEnvReturns404(t *testing.T) {
	rec := postCancel(t, newCancelMux(memory.NewEnvironmentRepository()), "does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestCancelDeletion_RunningEnvIsIdempotent(t *testing.T) {
	ctx := context.Background()
	envRepo := memory.NewEnvironmentRepository()
	env := &domain.PreviewEnvironment{
		ID: "env-run", IdempotencyKey: "env-run",
		Status: domain.EnvRunning, CreatedAt: time.Now(), TTL: domain.TTL{Duration: time.Hour},
	}
	if err := envRepo.Save(ctx, env); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := postCancel(t, newCancelMux(envRepo), "env-run")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (no-op), got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCancelDeletion_DestroyedEnvReturns409(t *testing.T) {
	ctx := context.Background()
	envRepo := memory.NewEnvironmentRepository()
	env := &domain.PreviewEnvironment{
		ID: "env-gone", IdempotencyKey: "env-gone",
		Status: domain.EnvDestroyed, CreatedAt: time.Now(), TTL: domain.TTL{Duration: time.Hour},
	}
	if err := envRepo.Save(ctx, env); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := postCancel(t, newCancelMux(envRepo), "env-gone")
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.Code)
	}
}

func TestCancelDeletion_MethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/environments/env-1/cancel-deletion", nil)
	newCancelMux(memory.NewEnvironmentRepository()).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

// destroySpy là một test double của application.DockerOrchestrationService
// chỉ để ghi lại các lần Destroy được gọi.
type destroySpy struct{ destroyed []string }

func (d *destroySpy) Provision(context.Context, application.ProvisionRequest) (application.ProvisionResult, error) {
	return application.ProvisionResult{}, nil
}

func (d *destroySpy) Destroy(_ context.Context, id string) error {
	d.destroyed = append(d.destroyed, id)
	return nil
}

// TestCancelDeletion_PreventsCleanupFromDestroying là luồng đầy đủ theo Prompt 1:
// env hết TTL -> CleanupService đánh dấu MARK_FOR_DELETION -> gọi endpoint hủy
// -> env quay lại RUNNING -> lượt quét kế tiếp KHÔNG destroy container.
func TestCancelDeletion_PreventsCleanupFromDestroying(t *testing.T) {
	ctx := context.Background()
	envRepo := memory.NewEnvironmentRepository()
	prRepo := memory.NewPullRequestRepository()
	spy := &destroySpy{}
	cleanup := application.NewCleanupService(envRepo, prRepo, spy, memory.NewGitHubNotifier(), "http://localhost:8080")

	pr := &domain.PullRequest{ID: "PR_1", Repo: "org-app", Number: 7, CommitHash: "abc123"}
	if err := prRepo.Save(ctx, pr); err != nil {
		t.Fatalf("save pr: %v", err)
	}

	key := pr.IdempotencyKey() // "org-app:PR_1:abc123" — không chứa "/"
	env := &domain.PreviewEnvironment{
		ID:             key,
		IdempotencyKey: key,
		PullRequestID:  pr.ID,
		Status:         domain.EnvRunning,
		CreatedAt:      time.Now().Add(-48 * time.Hour), // đã quá TTL từ lâu
		TTL:            domain.TTL{Duration: time.Hour},
		ContainerID:    "container-1",
	}
	if err := envRepo.Save(ctx, env); err != nil {
		t.Fatalf("save env: %v", err)
	}

	// Lượt quét 1: đánh dấu MARK_FOR_DELETION.
	cleanup.Run(ctx)
	got, _ := envRepo.FindByIdempotencyKey(ctx, key)
	if got.Status != domain.EnvMarkedForDeletion {
		t.Fatalf("after sweep 1 want MARK_FOR_DELETION, got %s", got.Status)
	}

	// Người dùng bấm "Hủy lệnh xoá".
	rec := postCancel(t, newCancelMux(envRepo), key)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ = envRepo.FindByIdempotencyKey(ctx, key)
	if got.Status != domain.EnvRunning || got.MarkedForDeletionAt != nil {
		t.Fatalf("after cancel want RUNNING & no mark, got %s / %v", got.Status, got.MarkedForDeletionAt)
	}

	// Lượt quét 2 ngay sau đó: KHÔNG được destroy (grace period đã reset).
	cleanup.Run(ctx)
	if len(spy.destroyed) != 0 {
		t.Fatalf("cleanup không được xoá env đã hủy lệnh, nhưng đã destroy: %v", spy.destroyed)
	}
	got, _ = envRepo.FindByIdempotencyKey(ctx, key)
	if got.Status == domain.EnvDestroyed {
		t.Fatalf("env không được ở trạng thái DESTROYED sau khi đã hủy lệnh")
	}
}
