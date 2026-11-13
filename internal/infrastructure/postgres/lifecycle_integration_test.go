//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"preview-env-orchestrator/internal/domain"
	pgadapter "preview-env-orchestrator/internal/infrastructure/postgres"
)

func TestAtomicQuotaRetryAndLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	prs := pgadapter.NewPullRequestRepository(pool)
	envs := pgadapter.NewEnvironmentRepository(pool)
	pr := &domain.PullRequest{ID: "PR_lifecycle", Repo: "org/app", Number: 7, CommitHash: "first", Status: domain.PRStatusOpen}
	if err := prs.Save(ctx, pr); err != nil {
		t.Fatal(err)
	}
	makeEnv := func(n int) *domain.PreviewEnvironment {
		key := fmt.Sprintf("org/app:PR_lifecycle:%d", n)
		return &domain.PreviewEnvironment{ID: key, IdempotencyKey: key, PullRequestID: pr.ID, Repo: pr.Repo, CommitSHA: fmt.Sprint(n), Status: domain.EnvProvisioning, CreatedAt: time.Now(), TTL: domain.TTL{Duration: time.Hour}}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted, rejected []*domain.PreviewEnvironment
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := makeEnv(i)
			ok, err := envs.Reserve(ctx, env, domain.Quota{MaxEnvironments: 3})
			if err != nil && !errors.Is(err, domain.ErrQuotaExceeded) {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ok {
				accepted = append(accepted, env)
			} else {
				rejected = append(rejected, env)
			}
		}(i)
	}
	wg.Wait()
	if len(accepted) != 3 {
		t.Fatalf("accepted %d, want 3", len(accepted))
	}
	if ok, err := envs.Reserve(ctx, accepted[0], domain.Quota{MaxEnvironments: 3}); ok || err != nil {
		t.Fatalf("duplicate: %v %v", ok, err)
	}
	accepted[0].Status = domain.EnvDestroyed
	if err := envs.Save(ctx, accepted[0]); err != nil {
		t.Fatal(err)
	}
	if ok, err := envs.Reserve(ctx, rejected[0], domain.Quota{MaxEnvironments: 3}); !ok || err != nil {
		t.Fatalf("quota rejection poisoned key: %v", err)
	}
	env := accepted[1]
	mark := time.Now()
	env.MarkForDeletion(mark)
	if err := envs.Save(ctx, env); err != nil {
		t.Fatal(err)
	}
	marked, err := envs.FindByIdempotencyKey(ctx, env.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed, err := envs.RecordDeletionNotice(ctx, env.IdempotencyKey, *marked.MarkedForDeletionAt, mark); err != nil || confirmed == nil {
		t.Fatal("initial notification was not confirmed")
	}
	extended, err := envs.CancelDeletion(ctx, env.IdempotencyKey, mark.Add(10*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if extended.DeletionCancelCount != 1 || extended.ExpiresAt == nil || !extended.ExpiresAt.Equal(mark.Add(130*time.Second)) {
		t.Fatal("extension did not round-trip")
	}
	if extended.DeletionNotifiedAt != nil {
		t.Fatal("cancellation must clear notification confirmation")
	}
	if late, err := envs.RecordDeletionNotice(ctx, env.IdempotencyKey, *marked.MarkedForDeletionAt, mark.Add(11*time.Second)); err != nil || late != nil {
		t.Fatal("late confirmation must not overwrite cancellation")
	}
	if claimed, err := envs.ClaimDestruction(ctx, env.IdempotencyKey, mark.Add(2*time.Minute), time.Minute); err != nil || claimed != nil {
		t.Fatal("cancelled env must not be destroyed")
	}
	extended.MarkForDeletion(mark.Add(3 * time.Minute))
	if err := envs.Save(ctx, extended); err != nil {
		t.Fatal(err)
	}
	if _, err := envs.CancelDeletion(ctx, env.IdempotencyKey, mark.Add(3*time.Minute+time.Second), time.Minute); !errors.Is(err, domain.ErrCannotCancelDeletion) {
		t.Fatal("second cancellation accepted")
	}
	if claimed, err := envs.ClaimDestruction(ctx, env.IdempotencyKey, mark.Add(5*time.Minute), time.Minute); err != nil || claimed != nil {
		t.Fatal("unnotified deletion must not be claimed")
	}
	// Use the stored timestamp because PostgreSQL truncates to microseconds.
	pending, err := envs.FindByIdempotencyKey(ctx, env.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := envs.RecordDeletionNotice(ctx, env.IdempotencyKey, mark, mark); err != nil || stale != nil {
		t.Fatal("notice from the previous mark must not apply to the new cycle")
	}
	notified, err := envs.RecordDeletionNotice(ctx, env.IdempotencyKey, *pending.MarkedForDeletionAt, mark.Add(4*time.Minute))
	if err != nil || notified == nil || notified.DeletionNotifiedAt == nil {
		t.Fatal("notification confirmation did not round-trip")
	}
	if again, err := envs.RecordDeletionNotice(ctx, env.IdempotencyKey, *pending.MarkedForDeletionAt, mark.Add(10*time.Minute)); err != nil || again != nil {
		t.Fatal("notice confirmation must not restart grace")
	}
	if tooEarly, err := envs.ClaimDestruction(ctx, env.IdempotencyKey, mark.Add(4*time.Minute+30*time.Second), time.Minute); err != nil || tooEarly != nil {
		t.Fatal("must wait for grace after notification")
	}
	claimed, err := envs.ClaimDestruction(ctx, env.IdempotencyKey, mark.Add(6*time.Minute), time.Minute)
	if err != nil || claimed == nil || claimed.Status != domain.EnvDestroying {
		t.Fatal("ready env not claimed")
	}
	if again, err := envs.ClaimDestruction(ctx, env.IdempotencyKey, mark.Add(6*time.Minute), time.Minute); err != nil || again != nil {
		t.Fatal("destruction claimed twice")
	}
	if err := envs.RecoverDestructions(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := envs.FindByIdempotencyKey(ctx, env.IdempotencyKey)
	if err != nil || got.Status != domain.EnvMarkedForDeletion {
		t.Fatal("restart failed to recover interrupted destruction")
	}
	// Old commit keys must still resolve the PR for cleanup notifications.
	pr.CommitHash = "second"
	if err := prs.Save(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if got, err := prs.FindByIdempotencyKey(ctx, env.IdempotencyKey); err != nil || got == nil || got.Number != 7 {
		t.Fatal("old preview lost its PR after synchronize")
	}
	// Migration reruns must preserve data and not replay CREATE/ALTER statements.
	if err := pgadapter.RunMigrations(ctx, pool, migrationsDir(t)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM preview_environments`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("migration rerun lost data: count=%d err=%v", count, err)
	}
}

func TestDestructionNoticesPersistAndConfirmIndependently(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	prs := pgadapter.NewPullRequestRepository(pool)
	envs := pgadapter.NewEnvironmentRepository(pool)
	pr := &domain.PullRequest{ID: "PR_notices", Repo: "org/notices", Number: 8, CommitHash: "head", Status: domain.PRStatusOpen}
	if err := prs.Save(ctx, pr); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key     string
		status  domain.EnvStatus
		pending bool
	}{
		{"historical", domain.EnvDestroyed, false},
		{"pending", domain.EnvDestroyed, true},
		{"active", domain.EnvRunning, true},
	} {
		env := &domain.PreviewEnvironment{ID: item.key, IdempotencyKey: item.key, PullRequestID: pr.ID, Repo: pr.Repo,
			Status: item.status, DestructionNoticePending: item.pending, CreatedAt: time.Now(), TTL: domain.TTL{Duration: time.Hour}}
		if err := envs.Save(ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	// Recreate the adapter to verify delivery work is persisted in PostgreSQL.
	envs = pgadapter.NewEnvironmentRepository(pool)
	pending, err := envs.FindPendingDestructionNotices(ctx)
	if err != nil || len(pending) != 1 || pending[0].IdempotencyKey != "pending" || !pending[0].DestructionNoticePending {
		t.Fatalf("pending notices did not round-trip: %v %v", pending, err)
	}
	if active, err := envs.CountActiveByRepo(ctx, pr.Repo); err != nil || active != 1 {
		t.Fatal("destroyed environments must release quota despite pending notices")
	}
	if err := envs.ConfirmDestructionNotice(ctx, "active"); err != nil {
		t.Fatal(err)
	}
	active, err := envs.FindByIdempotencyKey(ctx, "active")
	if err != nil || !active.DestructionNoticePending || active.Status != domain.EnvRunning {
		t.Fatal("confirmation must only affect destroyed environments")
	}
	for i := 0; i < 2; i++ {
		if err := envs.ConfirmDestructionNotice(ctx, "pending"); err != nil {
			t.Fatal(err)
		}
	}
	if pending, err := envs.FindPendingDestructionNotices(ctx); err != nil || len(pending) != 0 {
		t.Fatal("confirmed notice must not be returned again")
	}
	completed, err := envs.FindByIdempotencyKey(ctx, "pending")
	if err != nil || completed.DestructionNoticePending || completed.Status != domain.EnvDestroyed {
		t.Fatal("confirmation must preserve DESTROYED and clear the pending flag")
	}
}
