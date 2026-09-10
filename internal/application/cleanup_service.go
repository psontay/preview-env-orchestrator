package application

import (
	"context"
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
	docker   DockerOrchestrationService
	notifier GitHubNotifier
}

func NewCleanupService(envRepo domain.EnvironmentRepository, docker DockerOrchestrationService, notifier GitHubNotifier) *CleanupService {
	return &CleanupService{envRepo: envRepo, docker: docker, notifier: notifier}
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
	}
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
