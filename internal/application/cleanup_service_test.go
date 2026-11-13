package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"preview-env-orchestrator/internal/domain"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

type cleanupNotifier struct {
	messages []string
	fail     bool
	onSend   func()
}

func (n *cleanupNotifier) CommentOnPR(_ context.Context, repo string, number int, message string) error {
	if repo != "org/app" || number != 42 {
		return errors.New("incorrect PR destination")
	}
	n.messages = append(n.messages, message)
	if n.onSend != nil {
		n.onSend()
	}
	if n.fail {
		return errors.New("GitHub unavailable")
	}
	return nil
}

type cleanupDocker struct {
	destroyed []string
	err       error
}

func (*cleanupDocker) Provision(context.Context, ProvisionRequest) (ProvisionResult, error) {
	return ProvisionResult{}, errors.New("unexpected provision")
}

func (d *cleanupDocker) Destroy(_ context.Context, target string) error {
	d.destroyed = append(d.destroyed, target)
	return d.err
}

type cleanupFixture struct {
	t        *testing.T
	now      time.Time
	envs     *memory.EnvironmentRepository
	prs      *memory.PullRequestRepository
	notifier *cleanupNotifier
	docker   *cleanupDocker
	svc      *CleanupService
	key      string
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	t.Helper()
	f := &cleanupFixture{t: t, now: time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC),
		envs: memory.NewEnvironmentRepository(), prs: memory.NewPullRequestRepository(),
		notifier: &cleanupNotifier{}, docker: &cleanupDocker{}}
	pr := &domain.PullRequest{ID: "PR_42", Repo: "org/app", Number: 42, CommitHash: strings.Repeat("a", 40)}
	if err := f.prs.Save(t.Context(), pr); err != nil {
		t.Fatal(err)
	}
	f.key = pr.IdempotencyKey()
	f.save(&domain.PreviewEnvironment{ID: f.key, IdempotencyKey: f.key, PullRequestID: pr.ID,
		Repo: pr.Repo, CommitSHA: pr.CommitHash, Status: domain.EnvRunning,
		URL: "http://localhost:32773", ResourceGroup: "owned-group", ContainerID: "app-id",
		CreatedAt: f.now.Add(-2 * time.Hour), TTL: domain.TTL{Duration: time.Hour}})
	f.svc = f.service()
	return f
}

func (f *cleanupFixture) service() *CleanupService {
	svc := NewCleanupService(f.envs, f.prs, f.docker, f.notifier, "https://preview.example")
	svc.Configure(time.Minute, "test-signing-key")
	svc.now = func() time.Time { return f.now }
	return svc
}

func (f *cleanupFixture) save(env *domain.PreviewEnvironment) {
	f.t.Helper()
	if err := f.envs.Save(f.t.Context(), env); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cleanupFixture) env() *domain.PreviewEnvironment {
	f.t.Helper()
	env, err := f.envs.FindByIdempotencyKey(f.t.Context(), f.key)
	if err != nil || env == nil {
		f.t.Fatalf("missing environment: %v", err)
	}
	return env
}

func TestCleanupWarnsOnceAcrossSweepsAndRestartThenDestroys(t *testing.T) {
	f := newCleanupFixture(t)
	f.svc.Run(t.Context())
	env := f.env()
	if env.Status != domain.EnvMarkedForDeletion || env.DeletionNotifiedAt == nil || len(f.docker.destroyed) != 0 {
		t.Fatal("first sweep must mark and warn without destroying")
	}
	if len(f.notifier.messages) != 1 {
		t.Fatal("expected one warning")
	}
	for _, want := range []string{env.CommitSHA, env.URL, "1m0s", CancelDeletionURL(f.svc.baseURL, env.ID, f.svc.cancelSigningKey), "2 minutes"} {
		if !strings.Contains(f.notifier.messages[0], want) {
			t.Errorf("warning missing %q", want)
		}
	}
	f.svc.Run(t.Context())
	f.svc = f.service() // persisted notice survives a worker restart
	f.svc.Run(t.Context())
	if len(f.notifier.messages) != 1 {
		t.Fatal("confirmed warning must not be sent again")
	}
	f.now = f.now.Add(61 * time.Second)
	f.svc.Run(t.Context())
	if f.env().Status != domain.EnvDestroyed || len(f.docker.destroyed) != 1 || f.docker.destroyed[0] != "owned-group" {
		t.Fatal("must destroy the full resource group after grace")
	}
	if len(f.notifier.messages) != 2 || f.env().DestructionNoticePending {
		t.Fatal("successful destruction must publish and confirm a completion notice")
	}
	for _, want := range []string{"DESTROYED", env.CommitSHA, env.URL, "quota", "released"} {
		if !strings.Contains(f.notifier.messages[1], want) {
			t.Errorf("completion notice missing %q", want)
		}
	}
	f.svc = f.service()
	f.svc.Run(t.Context())
	if len(f.notifier.messages) != 2 || len(f.docker.destroyed) != 1 {
		t.Fatal("restart must not repeat confirmed notices or resource cleanup")
	}
}

func TestCleanupRetriesFailedWarningAndStartsFreshGrace(t *testing.T) {
	f := newCleanupFixture(t)
	env := f.env()
	env.MarkForDeletion(f.now.Add(-time.Hour))
	f.save(env)
	f.notifier.fail = true
	f.svc.Run(t.Context())
	if f.env().DeletionNotifiedAt != nil || len(f.docker.destroyed) != 0 {
		t.Fatal("failed warning must block destruction even after the old deadline")
	}
	f.now = f.now.Add(10 * time.Minute)
	f.notifier.fail = false
	f.svc.Run(t.Context())
	if env := f.env(); env.DeletionNotifiedAt == nil || !env.DeletionNotifiedAt.Equal(f.now) {
		t.Fatal("successful retry must persist its delivery time")
	}
	f.now = f.now.Add(59 * time.Second)
	f.svc.Run(t.Context())
	if len(f.docker.destroyed) != 0 {
		t.Fatal("late delivery must grant a full grace period")
	}
	f.now = f.now.Add(2 * time.Second)
	f.svc.Run(t.Context())
	if len(f.docker.destroyed) != 1 || len(f.notifier.messages) != 3 {
		t.Fatal("expected two warning attempts, one destruction and one completion notice")
	}
}

type unavailablePRs struct {
	domain.PullRequestRepository
	err error
}

func (r unavailablePRs) FindByIdempotencyKey(context.Context, string) (*domain.PullRequest, error) {
	return nil, r.err
}

func TestCleanupRetriesMissingOrUnavailablePR(t *testing.T) {
	for _, err := range []error{nil, errors.New("database unavailable")} {
		t.Run("lookup-"+statusOfError(err), func(t *testing.T) {
			f := newCleanupFixture(t)
			f.svc.prRepo = unavailablePRs{PullRequestRepository: f.prs, err: err}
			f.svc.Run(t.Context())
			f.now = f.now.Add(time.Hour)
			f.svc.Run(t.Context())
			if len(f.notifier.messages) != 0 || len(f.docker.destroyed) != 0 {
				t.Fatal("missing PR must leave warning pending and block destruction")
			}
			f.svc.prRepo = f.prs
			f.svc.Run(t.Context())
			if len(f.notifier.messages) != 1 || f.env().DeletionNotifiedAt == nil {
				t.Fatal("must resume notification after lookup recovers")
			}
		})
	}
}

func statusOfError(err error) string {
	if err == nil {
		return "missing"
	}
	return "error"
}

func TestCleanupNoticeCannotUndoConcurrentCancellation(t *testing.T) {
	f := newCleanupFixture(t)
	f.notifier.onSend = func() {
		if _, err := f.envs.CancelDeletion(t.Context(), f.key, f.now, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	f.svc.Run(t.Context())
	env := f.env()
	if env.Status != domain.EnvRunning || env.DeletionNotifiedAt != nil || env.DeletionCancelCount != 1 {
		t.Fatal("recording delivery must not overwrite cancellation")
	}
}

func TestCleanupWarnsAgainWithoutCancelLinkAfterExtension(t *testing.T) {
	f := newCleanupFixture(t)
	f.svc.Run(t.Context())
	f.now = f.now.Add(30 * time.Second)
	if _, err := f.envs.CancelDeletion(t.Context(), f.key, f.now, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2*time.Minute + time.Second)
	f.svc.Run(t.Context())
	if len(f.notifier.messages) != 2 || f.env().DeletionNotifiedAt == nil || len(f.docker.destroyed) != 0 {
		t.Fatal("second expiry must receive its own warning and grace")
	}
	message := f.notifier.messages[1]
	if strings.Contains(message, "/cancel-deletion") || !strings.Contains(message, "Cannot renew") ||
		!strings.Contains(message, "deletion cancel count > 0") {
		t.Fatal("second warning must explain that cancellation is unavailable")
	}
	if _, err := f.envs.CancelDeletion(t.Context(), f.key, f.now, time.Minute); !errors.Is(err, domain.ErrCannotCancelDeletion) {
		t.Fatal("a second cancellation must remain rejected")
	}
	f.now = f.now.Add(61 * time.Second)
	f.svc.Run(t.Context())
	if f.env().Status != domain.EnvDestroyed {
		t.Fatal("extended environment must still be cleaned up after its second grace")
	}
}

func TestCleanupWarningForFailedProvisioningDoesNotOfferCancellation(t *testing.T) {
	f := newCleanupFixture(t)
	env := f.env()
	env.LastError = "failed provisioning with leftover resources"
	env.MarkForDeletion(f.now.Add(-time.Hour))
	f.save(env)
	f.svc.Run(t.Context())
	if len(f.notifier.messages) != 1 || strings.Contains(f.notifier.messages[0], "/cancel-deletion") ||
		!strings.Contains(f.notifier.messages[0], "Cannot cancel deletion") ||
		!strings.Contains(f.notifier.messages[0], "failed to initialize") {
		t.Fatal("failed preview warning must not offer a cancellation link")
	}
	f.now = f.now.Add(61 * time.Second)
	f.svc.Run(t.Context())
	if f.env().Status != domain.EnvDestroyed {
		t.Fatal("leftover resources must be destroyed after warning and grace")
	}
}

type failingNoticeRepository struct {
	domain.EnvironmentRepository
	fail bool
}

func (r *failingNoticeRepository) RecordDeletionNotice(ctx context.Context, key string, markedAt, notifiedAt time.Time) (*domain.PreviewEnvironment, error) {
	if r.fail {
		return nil, errors.New("database unavailable")
	}
	return r.EnvironmentRepository.RecordDeletionNotice(ctx, key, markedAt, notifiedAt)
}

func TestCleanupRetriesUnconfirmedDeliveryAfterStorageFailure(t *testing.T) {
	f := newCleanupFixture(t)
	repo := &failingNoticeRepository{EnvironmentRepository: f.envs, fail: true}
	f.svc.envRepo = repo
	f.svc.Run(t.Context())
	f.now = f.now.Add(time.Hour)
	if f.env().DeletionNotifiedAt != nil || len(f.docker.destroyed) != 0 {
		t.Fatal("unconfirmed delivery must not enable deletion")
	}
	repo.fail = false
	f.svc.Run(t.Context())
	if len(f.notifier.messages) != 2 || f.env().DeletionNotifiedAt == nil || len(f.docker.destroyed) != 0 {
		t.Fatal("retry may duplicate the comment but must start a fresh grace period")
	}
}

func TestCompletionNoticeFailureRetriesAfterRestartWithoutHoldingQuota(t *testing.T) {
	f := newCleanupFixture(t)
	f.svc.Run(t.Context())
	f.now = f.now.Add(61 * time.Second)
	f.notifier.fail = true
	f.notifier.onSend = func() {
		if f.env().Status != domain.EnvDestroyed {
			t.Fatal("must persist completed destruction before publishing the notice")
		}
		if active, err := f.envs.CountActiveByRepo(t.Context(), "org/app"); err != nil || active != 0 {
			t.Fatal("quota must already be released when sending the completion notice")
		}
	}
	f.svc.Run(t.Context())
	if !f.env().DestructionNoticePending || f.env().Status != domain.EnvDestroyed {
		t.Fatal("failed delivery must stay pending without reversing destruction")
	}
	// A new commit can reserve the released slot before GitHub recovers.
	newEnv := &domain.PreviewEnvironment{ID: "new", IdempotencyKey: "new", Repo: "org/app", Status: domain.EnvProvisioning}
	if ok, err := f.envs.Reserve(t.Context(), newEnv, domain.Quota{MaxEnvironments: 1}); err != nil || !ok {
		t.Fatal("undelivered completion notice must not block a new reservation")
	}
	f.notifier.fail, f.notifier.onSend = false, nil
	f.svc = f.service()
	f.svc.Run(t.Context())
	if f.env().DestructionNoticePending || len(f.notifier.messages) != 3 || len(f.docker.destroyed) != 1 {
		t.Fatal("restart must retry the notice without destroying resources again")
	}
}

func TestCleanupDoesNotAnnounceCompletionWhenDockerFails(t *testing.T) {
	f := newCleanupFixture(t)
	f.svc.Run(t.Context())
	f.now = f.now.Add(61 * time.Second)
	f.docker.err = errors.New("Docker unavailable")
	f.svc.Run(t.Context())
	if f.env().Status != domain.EnvMarkedForDeletion || f.env().DestructionNoticePending || len(f.notifier.messages) != 1 {
		t.Fatal("failed cleanup must not enqueue or send a completion notice")
	}
	f.docker.err = nil
	f.svc.Run(t.Context())
	if f.env().Status != domain.EnvDestroyed || len(f.notifier.messages) != 2 {
		t.Fatal("successful cleanup retry must announce completion")
	}
}

type failingCompletionRepository struct {
	domain.EnvironmentRepository
	failSave    bool
	failConfirm bool
}

func (r *failingCompletionRepository) Save(ctx context.Context, env *domain.PreviewEnvironment) error {
	if r.failSave && env.Status == domain.EnvDestroyed {
		return errors.New("database unavailable")
	}
	return r.EnvironmentRepository.Save(ctx, env)
}

func (r *failingCompletionRepository) ConfirmDestructionNotice(ctx context.Context, key string) error {
	if r.failConfirm {
		return errors.New("database unavailable")
	}
	return r.EnvironmentRepository.ConfirmDestructionNotice(ctx, key)
}

func TestCleanupDoesNotAnnounceCompletionBeforeStateIsPersisted(t *testing.T) {
	f := newCleanupFixture(t)
	f.svc.Run(t.Context())
	f.svc.envRepo = &failingCompletionRepository{EnvironmentRepository: f.envs, failSave: true}
	f.now = f.now.Add(61 * time.Second)
	f.svc.Run(t.Context())
	if f.env().Status != domain.EnvDestroying || len(f.notifier.messages) != 1 {
		t.Fatal("failed state persistence must not publish completion")
	}
}

func TestCompletionNoticeRetriesUnconfirmedDelivery(t *testing.T) {
	f := newCleanupFixture(t)
	f.svc.Run(t.Context())
	repo := &failingCompletionRepository{EnvironmentRepository: f.envs, failConfirm: true}
	f.svc.envRepo = repo
	f.now = f.now.Add(61 * time.Second)
	f.svc.Run(t.Context())
	if !f.env().DestructionNoticePending || len(f.notifier.messages) != 2 {
		t.Fatal("failed confirmation must retain the pending notice")
	}
	repo.failConfirm = false
	f.svc.Run(t.Context())
	if f.env().DestructionNoticePending || len(f.notifier.messages) != 3 || len(f.docker.destroyed) != 1 {
		t.Fatal("unconfirmed delivery may repeat its comment but must not repeat cleanup")
	}
}

func TestCompletionNoticeRetriesPRLookupAfterDestruction(t *testing.T) {
	for _, err := range []error{nil, errors.New("database unavailable")} {
		t.Run(statusOfError(err), func(t *testing.T) {
			f := newCleanupFixture(t)
			f.svc.Run(t.Context())
			f.svc.prRepo = unavailablePRs{PullRequestRepository: f.prs, err: err}
			f.now = f.now.Add(61 * time.Second)
			f.svc.Run(t.Context())
			if f.env().Status != domain.EnvDestroyed || !f.env().DestructionNoticePending || len(f.notifier.messages) != 1 {
				t.Fatal("missing PR must leave completion pending while preserving destruction")
			}
			f.svc.prRepo = f.prs
			f.svc.Run(t.Context())
			if f.env().DestructionNoticePending || len(f.notifier.messages) != 2 {
				t.Fatal("completion delivery must resume after PR lookup recovers")
			}
		})
	}
}

func TestCleanupDoesNotAnnounceHistoricalDestructions(t *testing.T) {
	f := newCleanupFixture(t)
	env := f.env()
	env.Status = domain.EnvDestroyed // no pending notice in pre-upgrade history
	f.save(env)
	f.svc.Run(t.Context())
	if len(f.notifier.messages) != 0 || len(f.docker.destroyed) != 0 {
		t.Fatal("historical destroyed environments must not be replayed")
	}
}

func TestCleanupResumesInterruptedDestructionWithoutRepeatingWarning(t *testing.T) {
	f := newCleanupFixture(t)
	f.svc.Run(t.Context())
	f.now = f.now.Add(61 * time.Second)
	claimed, err := f.envs.ClaimDestruction(t.Context(), f.key, f.now, time.Minute)
	if err != nil || claimed == nil || claimed.Status != domain.EnvDestroying {
		t.Fatal("must claim destruction before simulating an interruption")
	}
	if err := f.envs.RecoverDestructions(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.svc = f.service()
	f.svc.Run(t.Context())
	if f.env().Status != domain.EnvDestroyed || len(f.docker.destroyed) != 1 || len(f.notifier.messages) != 2 {
		t.Fatal("recovery must resume cleanup and send completion without repeating its warning")
	}
}
