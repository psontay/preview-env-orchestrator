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

type pipelineDocker struct {
	mu       sync.Mutex
	requests []application.ProvisionRequest
	fail     bool
	wait     bool
}

func (d *pipelineDocker) Provision(ctx context.Context, req application.ProvisionRequest) (application.ProvisionResult, error) {
	d.mu.Lock()
	d.requests = append(d.requests, req)
	fail, wait := d.fail, d.wait
	d.mu.Unlock()
	if wait {
		<-ctx.Done()
		return application.ProvisionResult{}, ctx.Err()
	}
	if fail {
		return application.ProvisionResult{}, errors.New("test build failure")
	}
	return application.ProvisionResult{ContainerID: "owned-id", URL: "http://localhost:32123"}, nil
}
func (*pipelineDocker) Destroy(context.Context, string) error { return nil }

type silentNotifier struct{}

func (silentNotifier) CommentOnPR(context.Context, string, int, string) error { return nil }

func TestPipelinePreservesSHAAndRetriesFailedBuild(t *testing.T) {
	ctx := context.Background()
	prRepo := memory.NewPullRequestRepository()
	envRepo := memory.NewEnvironmentRepository()
	docker := &pipelineDocker{fail: true}
	svc := application.NewWebhookService(prRepo, envRepo, docker, silentNotifier{}, domain.Quota{MaxEnvironments: 1}, domain.TTL{Duration: time.Minute}, "https://preview.example")
	pr := &domain.PullRequest{ID: "PR_1", Repo: "org/app", Number: 1, CommitHash: strings.Repeat("a", 40)}
	if err := svc.HandlePullRequestEvent(ctx, pr); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	env, _ := envRepo.FindByIdempotencyKey(ctx, pr.IdempotencyKey())
	if env.Status != domain.EnvFailed {
		t.Fatal("must persist FAILED")
	}
	docker.fail = false
	if err := svc.HandlePullRequestEvent(ctx, pr); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	env, _ = envRepo.FindByIdempotencyKey(ctx, pr.IdempotencyKey())
	if env.Status != domain.EnvRunning || env.ExpiresAt == nil || !env.ExpiresAt.After(time.Now()) {
		t.Fatal("must persist ready URL and TTL starting at readiness")
	}
	if len(docker.requests) != 2 || docker.requests[1].CommitSHA != pr.CommitHash || docker.requests[1].Repo != pr.Repo || docker.requests[1].Image != "" {
		t.Fatal("must build event SHA, never shared latest image")
	}
	if err := svc.HandlePullRequestEvent(ctx, pr); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if len(docker.requests) != 2 {
		t.Fatal("successful duplicate must not build again")
	}
}

func TestPipelineCancelsOnApplicationShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	envRepo := memory.NewEnvironmentRepository()
	svc := application.NewWebhookService(memory.NewPullRequestRepository(), envRepo, &pipelineDocker{wait: true}, silentNotifier{}, domain.Quota{MaxEnvironments: 1}, domain.TTL{Duration: time.Hour}, "")
	svc.ConfigureRuntime(ctx, time.Minute, strings.Repeat("s", 64))
	pr := &domain.PullRequest{ID: "PR", Repo: "org/app", Number: 1, CommitHash: "abc"}
	if err := svc.HandlePullRequestEvent(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() { svc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown leaked a provisioning goroutine")
	}
	env, _ := envRepo.FindByIdempotencyKey(context.Background(), pr.IdempotencyKey())
	if env.Status != domain.EnvFailed {
		t.Fatal("cancelled work must not remain silently PROVISIONING")
	}
}
