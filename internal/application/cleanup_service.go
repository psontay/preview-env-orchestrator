package application

import (
	"context"
	"fmt"
	"log"
	"time"

	"preview-env-orchestrator/internal/domain"
)

// gracePeriod là khoảng thời gian ân hạn giữa lúc đánh dấu xoá và lúc
// thực sự gọi Docker huỷ container — đúng theo yêu cầu "Destructive Safety"
// trong roadmap (Giai đoạn 3).
const gracePeriod = 10 * time.Minute

// CleanupService là background worker, dự kiến được gọi định kỳ (mỗi 5
// phút) bởi một ticker trong main.go hoặc một cronjob bên ngoài.
type CleanupService struct {
	envRepo  domain.EnvironmentRepository
	prRepo   domain.PullRequestRepository
	docker   DockerOrchestrationService
	notifier GitHubNotifier
	// baseURL dùng để dựng link "Hủy lệnh xoá" gửi vào comment PR khi một
	// environment vừa bị đánh dấu MARK_FOR_DELETION.
	baseURL string
}

func NewCleanupService(
	envRepo domain.EnvironmentRepository,
	prRepo domain.PullRequestRepository,
	docker DockerOrchestrationService,
	notifier GitHubNotifier,
	baseURL string,
) *CleanupService {
	return &CleanupService{
		envRepo:  envRepo,
		prRepo:   prRepo,
		docker:   docker,
		notifier: notifier,
		baseURL:  baseURL,
	}
}

// Run thực hiện một lượt quét: (1) đánh dấu các env đã hết TTL, (2) huỷ
// thật các env đã qua grace period kể từ lúc được đánh dấu.
func (s *CleanupService) Run(ctx context.Context) {
	now := time.Now()
	s.markExpired(ctx, now)
	s.destroyReady(ctx, now)
}

func (s *CleanupService) markExpired(ctx context.Context, now time.Time) {
	expired, err := s.envRepo.FindExpired(ctx, now)
	if err != nil {
		log.Printf("[cleanup] lỗi tìm environment hết hạn: %v", err)
		return
	}
	for _, env := range expired {
		env.MarkForDeletion(now)
		if err := s.envRepo.Save(ctx, env); err != nil {
			log.Printf("[cleanup] lỗi đánh dấu xoá env=%s: %v", env.ID, err)
			continue
		}
		log.Printf("[cleanup] env=%s hết TTL, đã đánh dấu MARK_FOR_DELETION (sẽ xoá thật sau %s)", env.ID, gracePeriod)
		s.notifyMarkedForDeletion(ctx, env)
	}
}

// notifyMarkedForDeletion gửi comment vào PR báo environment sắp bị xoá,
// kèm link tới endpoint hủy lệnh xoá. Không tìm được PR tương ứng thì bỏ qua
// (comment là "best effort", không được làm hỏng lượt quét).
func (s *CleanupService) notifyMarkedForDeletion(ctx context.Context, env *domain.PreviewEnvironment) {
	pr, err := s.prRepo.FindByIdempotencyKey(ctx, env.IdempotencyKey)
	if err != nil || pr == nil {
		return
	}
	msg := fmt.Sprintf(
		"⚠️ Môi trường preview đã hết TTL và sẽ bị xoá sau %s.\n"+
			"Nếu muốn giữ lại (ví dụ PR vừa có commit mới), hủy lệnh xoá tại: %s",
		gracePeriod, cancelDeletionURL(s.baseURL, env))
	_ = s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number, msg)
}

func (s *CleanupService) destroyReady(ctx context.Context, now time.Time) {
	marked, err := s.envRepo.FindMarkedForDeletion(ctx, now)
	if err != nil {
		log.Printf("[cleanup] lỗi tìm environment chờ xoá: %v", err)
		return
	}
	for _, env := range marked {
		if !env.ReadyToDestroy(now, gracePeriod) {
			continue
		}
		if err := s.docker.Destroy(ctx, env.ContainerID); err != nil {
			log.Printf("[cleanup] lỗi destroy container cho env=%s: %v", env.ID, err)
			continue
		}
		env.Status = domain.EnvDestroyed
		if err := s.envRepo.Save(ctx, env); err != nil {
			log.Printf("[cleanup] lỗi lưu trạng thái DESTROYED cho env=%s: %v", env.ID, err)
			continue
		}
		log.Printf("[cleanup] env=%s đã bị huỷ hoàn toàn", env.ID)
	}
}
