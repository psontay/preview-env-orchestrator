package application

import (
	"context"
	"errors"
	"log"
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
	// baseURL là địa chỉ công khai của chính service này, dùng để dựng
	// link "Hủy lệnh xoá" gửi kèm comment PR.
	baseURL string
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
		prRepo:     prRepo,
		envRepo:    envRepo,
		docker:     docker,
		notifier:   notifier,
		quota:      quota,
		defaultTTL: defaultTTL,
		baseURL:    baseURL,
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
	// parentCtx giữ span cha (webhook.receive_and_parse do handler mở) để
	// cả span "ghi DB" lẫn span "provisioning" bên dưới đều treo trực tiếp
	// vào nó — chúng là hai bước song song về mặt ngữ nghĩa, không phải
	// bước con lồng nhau.
	parentCtx := ctx
	key := pr.IdempotencyKey()

	// ---- Span 2: ghi trạng thái vào DB (idempotency lock + quota + save) ----
	ctx, dbSpan := tracer.Start(ctx, "webhook.persist_state")
	dbSpan.SetAttributes(attribute.String("idempotency.key", key))

	acquired, err := s.envRepo.TryAcquireLock(ctx, key)
	if err != nil {
		endSpanErr(dbSpan, err)
		return err
	}
	if !acquired {
		log.Printf("[webhook] key=%s đang được xử lý bởi tiến trình khác, bỏ qua", key)
		dbSpan.AddEvent("idempotency: key đang được xử lý bởi tiến trình khác")
		dbSpan.End()
		return nil
	}

	existing, err := s.envRepo.FindByIdempotencyKey(ctx, key)
	if err != nil {
		endSpanErr(dbSpan, err)
		return err
	}
	if existing != nil {
		log.Printf("[webhook] key=%s đã có environment, bỏ qua (duplicate webhook)", key)
		dbSpan.AddEvent("idempotency: environment đã tồn tại, bỏ qua duplicate webhook")
		dbSpan.End()
		return nil
	}

	active, err := s.envRepo.CountActiveByRepo(ctx, pr.Repo)
	if err != nil {
		endSpanErr(dbSpan, err)
		return err
	}
	if s.quota.IsExceeded(active) {
		endSpanErr(dbSpan, ErrQuotaExceeded)
		return ErrQuotaExceeded
	}

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
	}
	if err := s.envRepo.Save(ctx, env); err != nil {
		endSpanErr(dbSpan, err)
		return err
	}
	dbSpan.End()

	// ---- Span 3: provisioning Docker (sống trong goroutine riêng) ----
	// context.WithoutCancel: lấy lại toàn bộ value của parentCtx (trong đó
	// có trace ID) nhưng BỎ tín hiệu hủy. Cần thiết vì HTTP handler sắp
	// return 202 và context của request bị cancel ngay sau đó, trong khi
	// provisioning còn chạy tiếp vài chục giây. Nếu ở đây dùng
	// context.Background() thì span provisioning sẽ mất liên kết với trace
	// gốc — nhìn trên Jaeger nó thành một trace rời, không biết thuộc
	// webhook nào.
	provisionCtx := context.WithoutCancel(parentCtx)
	provisionCtx, dockerSpan := tracer.Start(provisionCtx, "webhook.provision_docker")
	dockerSpan.SetAttributes(attribute.String("idempotency.key", key))

	// Provisioning chạy trong goroutine riêng — HTTP handler trả về
	// 202 Accepted ngay lập tức, không chờ Docker dựng xong. Kết quả được
	// truyền về qua channel để cập nhật DB và comment vào PR khi hoàn tất.
	resultCh := make(chan provisionOutcome, 1)
	go s.provisionAsync(provisionCtx, req(pr, env), resultCh)
	go s.awaitProvisionResult(provisionCtx, dockerSpan, pr, env, resultCh)

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

func (s *WebhookService) awaitProvisionResult(ctx context.Context, span trace.Span, pr *domain.PullRequest, env *domain.PreviewEnvironment, resultCh <-chan provisionOutcome) {
	// span chính là dockerSpan mở ở HandlePullRequestEvent. Đóng nó ở đây
	// — trong goroutine — vì đây mới là lúc bước provisioning thật sự
	// kết thúc, chứ không phải lúc HTTP handler return.
	defer span.End()
	outcome := <-resultCh

	if outcome.err != nil {
		span.RecordError(outcome.err)
		span.SetStatus(codes.Error, outcome.err.Error())
		log.Printf("[webhook] provisioning thất bại cho key=%s: %v", env.IdempotencyKey, outcome.err)
		env.Status = domain.EnvFailed
		_ = s.envRepo.Save(ctx, env)
		_ = s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number, "❌ Tạo môi trường preview thất bại, vui lòng xem log.")
		return
	}

	span.SetAttributes(attribute.String("preview.url", outcome.result.URL))
	env.Status = domain.EnvRunning
	env.URL = outcome.result.URL
	env.ContainerID = outcome.result.ContainerID
	_ = s.envRepo.Save(ctx, env)
	_ = s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number,
		"✅ Môi trường preview của bạn đã sẵn sàng tại: "+outcome.result.URL+
			"\n\nMôi trường sẽ tự động bị xoá khi hết TTL. Nếu muốn giữ lại, "+
			"hủy lệnh xoá tại: "+cancelDeletionURL(s.baseURL, env))
}
