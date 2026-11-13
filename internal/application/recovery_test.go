package application_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

// The real application service runs against durable-state test doubles. These
// tests do not restart a process, connect to PostgreSQL or create Docker assets.
type recoveryDocker struct {
	mu           sync.Mutex
	events       []string
	requests     []application.ProvisionRequest
	destroyErr   error
	provisionErr error
	result       application.ProvisionResult
	started      chan struct{}
	wait         bool
}

func (d *recoveryDocker) Destroy(_ context.Context, target string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, "destroy:"+target)
	return d.destroyErr
}

func (d *recoveryDocker) Provision(ctx context.Context, req application.ProvisionRequest) (application.ProvisionResult, error) {
	d.mu.Lock()
	d.events = append(d.events, "provision:"+req.CommitSHA)
	d.requests = append(d.requests, req)
	result, err, started, wait := d.result, d.provisionErr, d.started, d.wait
	d.mu.Unlock()
	if started != nil {
		close(started)
	}
	if wait {
		<-ctx.Done()
		return application.ProvisionResult{}, ctx.Err()
	}
	return result, err
}

type recoveryFixture struct {
	t    *testing.T
	prs  *memory.PullRequestRepository
	envs *memory.EnvironmentRepository
	pr   *domain.PullRequest
	env  *domain.PreviewEnvironment
	dock *recoveryDocker
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := &recoveryFixture{t: t, prs: memory.NewPullRequestRepository(), envs: memory.NewEnvironmentRepository(),
		pr:   &domain.PullRequest{ID: "PR_recover", Repo: "org/app", Number: 17, CommitHash: strings.Repeat("a", 40)},
		dock: &recoveryDocker{result: application.ProvisionResult{ContainerID: "app-id", ResourceGroup: "owned-group", URL: "http://localhost:32123"}}}
	f.env = &domain.PreviewEnvironment{ID: f.pr.IdempotencyKey(), IdempotencyKey: f.pr.IdempotencyKey(),
		PullRequestID: f.pr.ID, Repo: f.pr.Repo, CommitSHA: f.pr.CommitHash, Status: domain.EnvProvisioning,
		CreatedAt: time.Now().Add(-time.Hour), TTL: domain.TTL{Duration: 10 * time.Minute}}
	if err := f.prs.Save(t.Context(), f.pr); err != nil {
		t.Fatal(err)
	}
	if err := f.envs.Save(t.Context(), f.env); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *recoveryFixture) service(prs domain.PullRequestRepository, envs domain.EnvironmentRepository) *application.WebhookService {
	return application.NewWebhookService(prs, envs, f.dock, silentNotifier{},
		domain.Quota{MaxEnvironments: 1}, domain.TTL{Duration: 10 * time.Minute}, "https://preview.example")
}

func (f *recoveryFixture) stored() *domain.PreviewEnvironment {
	f.t.Helper()
	env, err := f.envs.FindByIdempotencyKey(f.t.Context(), f.env.IdempotencyKey)
	if err != nil || env == nil {
		f.t.Fatalf("missing environment: %v", err)
	}
	return env
}

type recoveryPRLookup struct {
	domain.PullRequestRepository
	latest *domain.PullRequest
	err    error
}

func (r recoveryPRLookup) FindByIdempotencyKey(context.Context, string) (*domain.PullRequest, error) {
	if r.latest == nil {
		return nil, r.err
	}
	copy := *r.latest
	return &copy, r.err
}

func TestRecoveryRebuildsOriginalCommitAfterCleaningStaleResources(t *testing.T) {
	f := newRecoveryFixture(t)
	// PostgreSQL's PR lookup can return the latest head for an older pending
	// environment. Recovery must use the SHA from that environment's reservation.
	latest := *f.pr
	latest.CommitHash = strings.Repeat("b", 40)
	svc := f.service(recoveryPRLookup{PullRequestRepository: f.prs, latest: &latest}, f.envs)
	before := time.Now()
	if err := svc.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if len(f.dock.events) != 2 || f.dock.events[0] != "destroy:"+f.env.ID || f.dock.events[1] != "provision:"+f.env.CommitSHA {
		t.Fatalf("must clean before rebuilding the original commit: %v", f.dock.events)
	}
	if len(f.dock.requests) != 1 || f.dock.requests[0].Repo != f.pr.Repo || f.dock.requests[0].EnvID != f.env.ID {
		t.Fatal("recovery changed the reservation identity")
	}
	got := f.stored()
	if got.Status != domain.EnvRunning || got.CommitSHA != f.env.CommitSHA || got.ResourceGroup != "owned-group" ||
		got.ContainerID != "app-id" || got.URL != f.dock.result.URL || !got.CreatedAt.Equal(f.env.CreatedAt) {
		t.Fatal("recovered preview must persist readiness and keep its original identity")
	}
	if got.ExpiresAt == nil || got.ExpiresAt.Before(before.Add(got.TTL.Duration)) || got.IsExpired(time.Now()) {
		t.Fatal("an old reservation must get its TTL from successful recovery readiness")
	}
	if latest.CommitHash != strings.Repeat("b", 40) {
		t.Fatal("recovery must not mutate the repository's latest PR head")
	}
}

type failedRecoveryListing struct {
	domain.EnvironmentRepository
}

func (failedRecoveryListing) FindProvisioning(context.Context) ([]*domain.PreviewEnvironment, error) {
	return nil, errors.New("storage unavailable")
}

func TestRecoveryFailsClosedBeforeProvisioning(t *testing.T) {
	for _, name := range []string{"environment lookup", "missing PR", "PR lookup", "stale cleanup"} {
		t.Run(name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			var prs domain.PullRequestRepository = f.prs
			var envs domain.EnvironmentRepository = f.envs
			switch name {
			case "environment lookup":
				envs = failedRecoveryListing{EnvironmentRepository: f.envs}
			case "missing PR":
				prs = recoveryPRLookup{PullRequestRepository: f.prs}
			case "PR lookup":
				prs = recoveryPRLookup{PullRequestRepository: f.prs, err: errors.New("storage unavailable")}
			case "stale cleanup":
				f.dock.destroyErr = errors.New("Docker unavailable")
			}
			svc := f.service(prs, envs)
			if err := svc.Recover(t.Context()); err == nil {
				t.Fatal("must report recovery failure")
			}
			svc.Wait()
			if len(f.dock.requests) != 0 || f.stored().Status != domain.EnvProvisioning {
				t.Fatal("must not rebuild or release an unresolved reservation")
			}
			if count, err := f.envs.CountActiveByRepo(t.Context(), f.pr.Repo); err != nil || count != 1 {
				t.Fatal("unresolved reservation must retain its quota slot")
			}
		})
	}
}

func TestRestartedServiceKeepsRunningPreviewAndExpiryOnDuplicate(t *testing.T) {
	f := newRecoveryFixture(t)
	expires := time.Now().Add(4 * time.Minute)
	f.env.Status, f.env.ExpiresAt = domain.EnvRunning, &expires
	f.env.ContainerID, f.env.ResourceGroup, f.env.URL = "existing-app", "existing-group", "http://localhost:32777"
	if err := f.envs.Save(t.Context(), f.env); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		svc := f.service(f.prs, f.envs) // a new service sharing persisted state
		if err := svc.Recover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := svc.HandlePullRequestEvent(t.Context(), f.pr); err != nil {
			t.Fatal(err)
		}
		svc.Wait()
	}
	got := f.stored()
	if len(f.dock.events) != 0 || got.Status != domain.EnvRunning || got.URL != f.env.URL ||
		got.ContainerID != f.env.ContainerID || got.ResourceGroup != f.env.ResourceGroup ||
		got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Fatal("restart/duplicate must not rebuild, destroy, change URL or reset TTL")
	}
}

func TestRecoveryFailureWithLeftoversRetainsQuotaAndCannotBeExtended(t *testing.T) {
	f := newRecoveryFixture(t)
	f.dock.provisionErr = errors.New("build failed and rollback left resources")
	svc := f.service(f.prs, f.envs)
	if err := svc.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	got := f.stored()
	if got.Status != domain.EnvMarkedForDeletion || got.LastError == "" || got.ResourceGroup != "owned-group" || got.MarkedForDeletionAt == nil {
		t.Fatal("leftovers must persist a cleanup handle and enter the grace lifecycle")
	}
	other := &domain.PreviewEnvironment{ID: "other", IdempotencyKey: "other", Repo: f.pr.Repo, Status: domain.EnvProvisioning}
	if ok, err := f.envs.Reserve(t.Context(), other, domain.Quota{MaxEnvironments: 1}); ok || !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatal("leftover resources must retain quota")
	}
	if _, err := f.envs.CancelDeletion(t.Context(), got.IdempotencyKey, time.Now(), time.Minute); !errors.Is(err, domain.ErrCannotCancelDeletion) {
		t.Fatal("a failed preview must not be extended")
	}
}

func TestWebhookDoesNotBuildBeforeFailedPreviousCleanupIsResolved(t *testing.T) {
	f := newRecoveryFixture(t)
	f.env.Status = domain.EnvFailed
	if err := f.envs.Save(t.Context(), f.env); err != nil {
		t.Fatal(err)
	}
	f.dock.destroyErr = errors.New("previous resources are still present")
	svc := f.service(f.prs, f.envs)
	if err := svc.HandlePullRequestEvent(t.Context(), f.pr); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	got := f.stored()
	if len(f.dock.requests) != 0 || got.Status != domain.EnvMarkedForDeletion || got.ContainerID != got.ID || got.LastError == "" {
		t.Fatal("failed previous cleanup must retain its handle and block a new build")
	}
}

func TestRecoveryProvisioningHonorsShutdown(t *testing.T) {
	f := newRecoveryFixture(t)
	f.dock.started, f.dock.wait = make(chan struct{}), true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	svc := f.service(f.prs, f.envs)
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.dock.started:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not start")
	}
	cancel()
	done := make(chan struct{})
	go func() { svc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery ignored shutdown or leaked a goroutine")
	}
	if got := f.stored(); got.Status != domain.EnvFailed || !strings.Contains(got.LastError, "canceled") {
		t.Fatal("cancelled recovery must persist a failed outcome")
	}
}
