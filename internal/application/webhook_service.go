package application

import (
	"context"
	"errors"
	"log"
	"time"

	"preview-env-orchestrator/internal/domain"
)

// ErrQuotaExceeded được trả về khi repo đã đạt giới hạn số environment
// đang hoạt động đồng thời.
var ErrQuotaExceeded = errors.New("quota exceeded for this repository")

// WebhookService là một Domain/Application Service: nó điều phối các
// Aggregate (PullRequest, PreviewEnvironment) và các port bên ngoài
// (Docker, GitHub) để thực hiện một use case trọn vẹn — "xử lý sự kiện
// webhook PR". Bản thân nó không chứa business rule (rule nằm trong
// domain), chỉ điều phối luồng chạy (orchestration).
type WebhookService struct {
	prRepo     domain.PullRequestRepository
	envRepo    domain.EnvironmentRepository
	docker     DockerOrchestrationService
	notifier   GitHubNotifier
	quota      domain.Quota
	defaultTTL domain.TTL
}

func NewWebhookService(
	prRepo domain.PullRequestRepository,
	envRepo domain.EnvironmentRepository,
	docker DockerOrchestrationService,
	notifier GitHubNotifier,
	quota domain.Quota,
	defaultTTL domain.TTL,
) *WebhookService {
	return &WebhookService{
		prRepo:     prRepo,
		envRepo:    envRepo,
		docker:     docker,
		notifier:   notifier,
		quota:      quota,
		defaultTTL: defaultTTL,
	}
}

// HandlePullRequestEvent xử lý một sự kiện webhook từ GitHub.
// Luồng xử lý bám sát Giai đoạn 2 + 3 trong roadmap:
//  1. Idempotency lock (chống xử lý trùng khi webhook bắn 2 lần liên tiếp)
//  2. Kiểm tra đã tồn tại environment cho commit này chưa
//  3. Kiểm tra Quota
//  4. Lưu trạng thái PROVISIONING
//  5. Provisioning bất đồng bộ (không block HTTP handler)
func (s *WebhookService) HandlePullRequestEvent(ctx context.Context, pr *domain.PullRequest) error {
	key := pr.IdempotencyKey()

	acquired, err := s.envRepo.TryAcquireLock(ctx, key)
	if err != nil {
		return err
	}
	if !acquired {
		log.Printf("[webhook] key=%s đang được xử lý bởi tiến trình khác, bỏ qua", key)
		return nil
	}

	existing, err := s.envRepo.FindByIdempotencyKey(ctx, key)
	if err != nil {
		return err
	}
	if existing != nil {
		log.Printf("[webhook] key=%s đã có environment, bỏ qua (duplicate webhook)", key)
		return nil
	}

	active, err := s.envRepo.CountActiveByRepo(ctx, pr.Repo)
	if err != nil {
		return err
	}
	if s.quota.IsExceeded(active) {
		return ErrQuotaExceeded
	}

	if err := s.prRepo.Save(ctx, pr); err != nil {
		return err
	}

	env := &domain.PreviewEnvironment{
		ID:             key,
		PullRequestID:  pr.ID,
		IdempotencyKey: key,
		Status:         domain.EnvProvisioning,
		CreatedAt:      time.Now(),
		TTL:            s.defaultTTL,
	}
	if err := s.envRepo.Save(ctx, env); err != nil {
		return err
	}

	// Provisioning chạy trong goroutine riêng — HTTP handler trả về
	// 202 Accepted ngay lập tức, không chờ Docker dựng xong (có thể mất
	// vài chục giây). Kết quả được truyền về qua channel để cập nhật DB
	// và comment vào PR khi hoàn tất.
	resultCh := make(chan provisionOutcome, 1)
	go s.provisionAsync(context.Background(), req(pr, env), resultCh)
	go s.awaitProvisionResult(pr, env, resultCh)

	return nil
}

type provisionOutcome struct {
	result ProvisionResult
	err    error
}

func req(pr *domain.PullRequest, env *domain.PreviewEnvironment) ProvisionRequest {
	return ProvisionRequest{
		Image:       "myapp/preview:latest",
		SeedSQLPath: "/seed/init.sql",
		EnvID:       env.ID,
	}
}

func (s *WebhookService) provisionAsync(ctx context.Context, req ProvisionRequest, out chan<- provisionOutcome) {
	result, err := s.docker.Provision(ctx, req)
	out <- provisionOutcome{result: result, err: err}
}

func (s *WebhookService) awaitProvisionResult(pr *domain.PullRequest, env *domain.PreviewEnvironment, resultCh <-chan provisionOutcome) {
	ctx := context.Background()
	outcome := <-resultCh

	if outcome.err != nil {
		log.Printf("[webhook] provisioning thất bại cho key=%s: %v", env.IdempotencyKey, outcome.err)
		env.Status = domain.EnvFailed
		_ = s.envRepo.Save(ctx, env)
		_ = s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number, "❌ Tạo môi trường preview thất bại, vui lòng xem log.")
		return
	}

	env.Status = domain.EnvRunning
	env.URL = outcome.result.URL
	env.ContainerID = outcome.result.ContainerID
	_ = s.envRepo.Save(ctx, env)
	_ = s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number,
		"✅ Môi trường preview của bạn đã sẵn sàng tại: "+outcome.result.URL)
}
