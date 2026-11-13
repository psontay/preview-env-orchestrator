package application

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"preview-env-orchestrator/internal/domain"
)

// tracer là handle OpenTelemetry dùng chung cho package application. Tên
// "preview-env-orchestrator" là instrumentation scope — nó xuất hiện trên
// mỗi span để biết span do phần code nào tạo ra.
var tracer = otel.Tracer("preview-env-orchestrator")

// endSpanErr đánh dấu span là lỗi (ghi lại error + set status Error) rồi
// đóng span. Gom lại một chỗ để các nhánh return sớm bên dưới gọn hơn.
func endSpanErr(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.End()
}

// ErrQuotaExceeded được trả về khi repo đã đạt giới hạn số environment
// đang hoạt động đồng thời.
var ErrQuotaExceeded = domain.ErrQuotaExceeded

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
	// baseURL là địa chỉ công khai của chính service này, dùng để dựng
	// link "Hủy lệnh xoá" gửi kèm comment PR.
	baseURL          string
	runtimeCtx       context.Context
	provisionTimeout time.Duration
	cancelSigningKey string
	jobs             sync.WaitGroup
}

func NewWebhookService(
	prRepo domain.PullRequestRepository,
	envRepo domain.EnvironmentRepository,
	docker DockerOrchestrationService,
	notifier GitHubNotifier,
	quota domain.Quota,
	defaultTTL domain.TTL,
	baseURL string,
) *WebhookService {
	return &WebhookService{
		prRepo:           prRepo,
		envRepo:          envRepo,
		docker:           docker,
		notifier:         notifier,
		quota:            quota,
		defaultTTL:       defaultTTL,
		baseURL:          baseURL,
		runtimeCtx:       context.Background(),
		provisionTimeout: 20 * time.Minute,
	}
}

// ConfigureRuntime is called once at the composition root before serving.
func (s *WebhookService) ConfigureRuntime(ctx context.Context, timeout time.Duration, cancelKey string) {
	s.runtimeCtx, s.provisionTimeout, s.cancelSigningKey = ctx, timeout, cancelKey
}

func (s *WebhookService) Wait() { s.jobs.Wait() }

// HandlePullRequestEvent xử lý một sự kiện webhook từ GitHub.
// Luồng xử lý bám sát Giai đoạn 2 + 3 trong roadmap:
//  1. Idempotency lock (chống xử lý trùng khi webhook bắn 2 lần liên tiếp)
//  2. Kiểm tra đã tồn tại environment cho commit này chưa
//  3. Kiểm tra Quota
//  4. Lưu trạng thái PROVISIONING
//  5. Provisioning bất đồng bộ (không block HTTP handler)
func (s *WebhookService) HandlePullRequestEvent(ctx context.Context, pr *domain.PullRequest) error {
	// parentCtx giữ span cha (webhook.receive_and_parse do handler mở) để
	// cả span "ghi DB" lẫn span "provisioning" bên dưới đều treo trực tiếp
	// vào nó — chúng là hai bước song song về mặt ngữ nghĩa, không phải
	// bước con lồng nhau.
	parentCtx := ctx
	key := pr.IdempotencyKey()

	// ---- Span 2: ghi trạng thái vào DB (idempotency lock + quota + save) ----
	ctx, dbSpan := tracer.Start(ctx, "webhook.persist_state")
	dbSpan.SetAttributes(attribute.String("idempotency.key", key))

	if err := s.prRepo.Save(ctx, pr); err != nil {
		endSpanErr(dbSpan, err)
		return err
	}

	env := &domain.PreviewEnvironment{
		ID:             key,
		PullRequestID:  pr.ID,
		IdempotencyKey: key,
		Status:         domain.EnvProvisioning,
		CreatedAt:      time.Now(),
		TTL:            s.defaultTTL,
		Repo:           pr.Repo,
		CommitSHA:      pr.CommitHash,
	}
	acquired, err := s.envRepo.Reserve(ctx, env, s.quota)
	if err != nil {
		endSpanErr(dbSpan, err)
		return err
	}
	dbSpan.End()
	if !acquired {
		return nil
	}

	// ---- Span 3: provisioning Docker (sống trong goroutine riêng) ----
	// context.WithoutCancel: lấy lại toàn bộ value của parentCtx (trong đó
	// có trace ID) nhưng BỎ tín hiệu hủy. Cần thiết vì HTTP handler sắp
	// return 202 và context của request bị cancel ngay sau đó, trong khi
	// provisioning còn chạy tiếp vài chục giây. Nếu ở đây dùng
	// context.Background() thì span provisioning sẽ mất liên kết với trace
	// gốc — nhìn trên Jaeger nó thành một trace rời, không biết thuộc
	// webhook nào.
	provisionCtx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), s.provisionTimeout)
	stop := context.AfterFunc(s.runtimeCtx, cancel)
	provisionCtx, dockerSpan := tracer.Start(provisionCtx, "webhook.provision_docker")
	dockerSpan.SetAttributes(attribute.String("idempotency.key", key))

	// Provisioning chạy trong goroutine riêng — HTTP handler trả về
	// 202 Accepted ngay lập tức, không chờ Docker dựng xong.
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer cancel()
		defer stop()
		defer dockerSpan.End()
		if cleanupErr := s.docker.Destroy(provisionCtx, env.ID); cleanupErr != nil {
			s.completeProvision(
				provisionCtx,
				dockerSpan,
				pr,
				env,
				provisionOutcome{
					result: ProvisionResult{ContainerID: env.ID},
					err:    fmt.Errorf("clean previous failed preview: %w", cleanupErr),
				},
			)
			return
		}
		s.comment(provisionCtx, pr, "Building `"+pr.CommitHash+"`.")
		result, provisionErr := s.docker.Provision(provisionCtx, req(pr, env))
		s.completeProvision(provisionCtx, dockerSpan, pr, env, provisionOutcome{result, provisionErr})
	}()

	return nil
}

type provisionOutcome struct {
	result ProvisionResult
	err    error
}

func req(pr *domain.PullRequest, env *domain.PreviewEnvironment) ProvisionRequest {
	return ProvisionRequest{
		EnvID:     env.ID,
		Repo:      pr.Repo,
		CommitSHA: pr.CommitHash,
	}
}

func (s *WebhookService) completeProvision(
	ctx context.Context,
	span trace.Span,
	pr *domain.PullRequest,
	env *domain.PreviewEnvironment,
	outcome provisionOutcome,
) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if outcome.err != nil {
		span.RecordError(outcome.err)
		span.SetStatus(codes.Error, outcome.err.Error())
		log.Printf("[webhook] provisioning thất bại cho key=%s: %v", env.IdempotencyKey, outcome.err)
		env.Status = domain.EnvFailed
		if outcome.result.ContainerID != "" || outcome.result.ResourceGroup != "" {
			env.ContainerID = outcome.result.ContainerID
			env.ResourceGroup = outcome.result.ResourceGroup
			env.MarkForDeletion(time.Now())
		}
		env.LastError = outcome.err.Error()
		if err := s.envRepo.Save(persistCtx, env); err != nil {
			log.Printf("[webhook] save FAILED: %v", err)
		}
		s.comment(
			persistCtx,
			pr,
			"❌ Tạo preview thất bại cho commit `"+pr.CommitHash+"`. Xem log orchestrator để biết bước gặp lỗi.",
		)
		return
	}

	span.SetAttributes(attribute.String("preview.url", outcome.result.URL))
	env.Status = domain.EnvRunning
	env.URL = outcome.result.URL
	env.ContainerID = outcome.result.ContainerID
	env.ResourceGroup = outcome.result.ResourceGroup
	expires := time.Now().Add(env.TTL.Duration)
	env.ExpiresAt = &expires
	if err := s.envRepo.Save(persistCtx, env); err != nil {
		log.Printf("[webhook] save RUNNING failed; removing owned preview: %v", err)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cleanupCancel()
		target := env.ResourceGroup
		if target == "" {
			target = env.ContainerID
		}
		if cleanupErr := s.docker.Destroy(cleanupCtx, target); cleanupErr != nil {
			log.Printf("[webhook] compensation failed: %v", cleanupErr)
		}
		return
	}
	s.comment(
		persistCtx,
		pr,
		fmt.Sprintf(
			"Preview commit `%s` ready: %s\n\nHealth: %s/actuator/health\nTTL: %s, since backend running.\n Cancel for deletion and renew for 2 mins (only grace period): %s\nURL localhost on machine running docker.",
			pr.CommitHash,
			env.URL,
			env.URL,
			env.TTL.Duration,
			CancelDeletionURL(s.baseURL, env.ID, s.cancelSigningKey),
		),
	)
}

func (s *WebhookService) comment(ctx context.Context, pr *domain.PullRequest, message string) {
	if err := s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number, message); err != nil {
		log.Printf("[webhook] GitHub comment failed for %s#%d: %v", pr.Repo, pr.Number, err)
	}
}

// Recover removes partially provisioned resources before replaying durable
// reservations after a single-instance restart.
func (s *WebhookService) Recover(ctx context.Context) error {
	envs, err := s.envRepo.FindProvisioning(ctx)
	if err != nil {
		return err
	}
	for _, env := range envs {
		pr, err := s.prRepo.FindByIdempotencyKey(ctx, env.IdempotencyKey)
		if err != nil {
			return err
		}
		if pr == nil {
			return fmt.Errorf("missing PR for pending environment %s", env.ID)
		}
		pr.CommitHash = env.CommitSHA
		// Fail closed: do not start a new build before stale resources are gone.
		if err := s.docker.Destroy(ctx, env.ID); err != nil {
			return fmt.Errorf("recover preview: %w", err)
		}
		jobCtx, cancel := context.WithTimeout(ctx, s.provisionTimeout)
		jobCtx, span := tracer.Start(jobCtx, "webhook.recover_provisioning")
		s.jobs.Add(1)
		go func(pr *domain.PullRequest, env *domain.PreviewEnvironment) {
			defer s.jobs.Done()
			defer cancel()
			defer span.End()
			result, err := s.docker.Provision(jobCtx, req(pr, env))
			s.completeProvision(jobCtx, span, pr, env, provisionOutcome{result, err})
		}(pr, env)
	}
	return nil
}
