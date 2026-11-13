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

// CleanupService is called periodically by the application lifecycle worker.
type CleanupService struct {
	envRepo  domain.EnvironmentRepository
	prRepo   domain.PullRequestRepository
	docker   DockerOrchestrationService
	notifier GitHubNotifier
	// baseURL dùng để dựng link "Hủy lệnh xoá" gửi vào comment PR khi một
	// environment vừa bị đánh dấu MARK_FOR_DELETION.
	baseURL          string
	grace            time.Duration
	cancelSigningKey string
	now              func() time.Time
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
		grace:    gracePeriod,
		now:      time.Now,
	}
}

func (s *CleanupService) Configure(grace time.Duration, cancelKey string) {
	s.grace, s.cancelSigningKey = grace, cancelKey
}

// Run thực hiện một lượt quét: (1) đánh dấu các env đã hết TTL, (2) huỷ
// thật các env đã được cảnh báo thành công và qua đủ grace period.
func (s *CleanupService) Run(ctx context.Context) {
	now := s.now()
	s.markExpired(ctx, now)
	s.destroyReady(ctx, now)
	s.notifyDestroyed(ctx)
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
		log.Printf("[cleanup] env=%s hết TTL, đã đánh dấu MARK_FOR_DELETION; đang chờ gửi cảnh báo", env.ID)
	}
}

// notifyMarkedForDeletion gửi comment vào PR báo environment sắp bị xoá,
// kèm link huỷ nếu còn lượt. Lỗi được thử lại ở lượt quét kế tiếp;
// chưa gửi thành công thì không bắt đầu grace period và không xoá tài nguyên.
func (s *CleanupService) notifyMarkedForDeletion(ctx context.Context, env *domain.PreviewEnvironment) {
	if env.MarkedForDeletionAt == nil {
		log.Printf("[cleanup] env=%s thiếu mốc đánh dấu xoá; chưa thể gửi cảnh báo", env.ID)
		return
	}
	pr, err := s.prRepo.FindByIdempotencyKey(ctx, env.IdempotencyKey)
	if err != nil {
		log.Printf("[cleanup] env=%s lỗi tìm PR để cảnh báo, sẽ thử lại: %v", env.ID, err)
		return
	}
	if pr == nil {
		log.Printf("[cleanup] env=%s chưa tìm thấy PR để cảnh báo, sẽ thử lại", env.ID)
		return
	}
	reason := "TTL run out"
	if env.LastError != "" {
		reason = "gặp lỗi khởi tạo và còn tài nguyên cần dọn"
	}
	msg := fmt.Sprintf(
		"Preview commit `%s` %s, mark for deletion.\n\n"+
			"Env: `%s`\nURL: %s\n"+
			"Delete after %s until this msg send success.\n\n",
		env.CommitSHA, reason, env.ID, env.URL, s.grace,
	)
	if env.LastError != "" {
		msg += "Cannot cancel deletion cause by preview env failed to initialize."
	} else if env.DeletionCancelCount > 0 {
		msg += "Cannot renew preview env cause by deletion cancel count > 0."
	} else {
		msg += "Cancel deletion and renew for 2 minutes: " + CancelDeletionURL(s.baseURL, env.ID, s.cancelSigningKey) +
			"\nOpen link while grace period."
	}
	if err := s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number, msg); err != nil {
		log.Printf("[cleanup] env=%s PR=%s#%d gửi cảnh báo thất bại, sẽ thử lại: %v", env.ID, pr.Repo, pr.Number, err)
		return
	}
	notified, err := s.envRepo.RecordDeletionNotice(ctx, env.IdempotencyKey, *env.MarkedForDeletionAt, s.now())
	if err != nil {
		log.Printf("[cleanup] env=%s lỗi lưu xác nhận cảnh báo, sẽ thử lại: %v", env.ID, err)
		return
	}
	if notified != nil {
		log.Printf(
			"[cleanup] env=%s đã gửi cảnh báo PR=%s#%d, hạn xoá=%s",
			env.ID,
			pr.Repo,
			pr.Number,
			notified.DeletionDeadline(s.grace).Format(time.RFC3339),
		)
	}
}

func (s *CleanupService) destroyReady(ctx context.Context, now time.Time) {
	marked, err := s.envRepo.FindMarkedForDeletion(ctx, now)
	if err != nil {
		log.Printf("[cleanup] lỗi tìm environment chờ xoá: %v", err)
		return
	}
	for _, env := range marked {
		if env.DeletionNotifiedAt == nil {
			s.notifyMarkedForDeletion(ctx, env)
			continue
		}
		claimed, err := s.envRepo.ClaimDestruction(ctx, env.IdempotencyKey, now, s.grace)
		if err != nil {
			log.Printf("[cleanup] claim failed: %v", err)
			continue
		}
		if claimed == nil {
			continue
		}
		env = claimed
		target := env.ResourceGroup
		if target == "" {
			target = env.ContainerID
		}
		if err := s.docker.Destroy(ctx, target); err != nil {
			log.Printf("[cleanup] lỗi destroy container cho env=%s: %v", env.ID, err)
			env.Status = domain.EnvMarkedForDeletion
			if saveErr := s.envRepo.Save(ctx, env); saveErr != nil {
				log.Printf("[cleanup] save retry state failed: %v", saveErr)
			}
			continue
		}
		env.Status = domain.EnvDestroyed
		env.DestructionNoticePending = true
		if err := s.envRepo.Save(ctx, env); err != nil {
			log.Printf("[cleanup] lỗi lưu trạng thái DESTROYED cho env=%s: %v", env.ID, err)
			continue
		}
		log.Printf("[cleanup] env=%s has been destroyed!", env.ID)
	}
}

// notifyDestroyed retries independently of resource cleanup. DESTROYED already
// releases quota, even if GitHub or delivery confirmation is temporarily down.
func (s *CleanupService) notifyDestroyed(ctx context.Context) {
	pending, err := s.envRepo.FindPendingDestructionNotices(ctx)
	if err != nil {
		log.Printf("[cleanup] lỗi tìm thông báo dọn xong đang chờ gửi: %v", err)
		return
	}
	for _, env := range pending {
		pr, err := s.prRepo.FindByIdempotencyKey(ctx, env.IdempotencyKey)
		if err != nil {
			log.Printf("[cleanup] env=%s lỗi tìm PR để báo dọn xong, sẽ thử lại: %v", env.ID, err)
			continue
		}
		if pr == nil {
			log.Printf("[cleanup] env=%s chưa tìm thấy PR để báo dọn xong, sẽ thử lại", env.ID)
			continue
		}
		msg := fmt.Sprintf(
			"Preview commit `%s` has been clean up (DESTROYED).\n\n"+
				"Env: `%s`\nold URL of preview has been stopped: `%s`\n\n"+
				"The quota used by this environment has been released. ",
			env.CommitSHA, env.ID, env.URL,
		)
		if err := s.notifier.CommentOnPR(ctx, pr.Repo, pr.Number, msg); err != nil {
			log.Printf(
				"[cleanup] env=%s PR=%s#%d báo dọn xong thất bại, sẽ thử lại: %v",
				env.ID,
				pr.Repo,
				pr.Number,
				err,
			)
			continue
		}
		if err := s.envRepo.ConfirmDestructionNotice(ctx, env.IdempotencyKey); err != nil {
			log.Printf("[cleanup] env=%s lỗi lưu xác nhận thông báo dọn xong, sẽ thử lại: %v", env.ID, err)
			continue
		}
		log.Printf("[cleanup] env=%s đã báo dọn xong PR=%s#%d", env.ID, pr.Repo, pr.Number)
	}
}
