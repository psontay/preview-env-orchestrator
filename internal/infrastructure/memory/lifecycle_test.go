package memory_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"preview-env-orchestrator/internal/domain"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

func reservation(repo, key string) *domain.PreviewEnvironment {
	return &domain.PreviewEnvironment{ID: key, IdempotencyKey: key, Repo: repo, Status: domain.EnvProvisioning, CreatedAt: time.Now(), TTL: domain.TTL{Duration: time.Hour}}
}

func TestConcurrentQuotaIsPerRepository(t *testing.T) {
	repo := memory.NewEnvironmentRepository()
	ctx := context.Background()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := repo.Reserve(ctx, reservation("org/app", fmt.Sprint(i)), domain.Quota{MaxEnvironments: 3})
			if err != nil && !errors.Is(err, domain.ErrQuotaExceeded) {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("accepted %d, want 3", accepted.Load())
	}
	if ok, err := repo.Reserve(ctx, reservation("other/app", "other"), domain.Quota{MaxEnvironments: 1}); !ok || err != nil {
		t.Fatal("another repo must have its own quota")
	}
	if n, _ := repo.CountActiveByRepo(ctx, "org/app"); n != 3 {
		t.Fatalf("repo count=%d", n)
	}
}

func TestRejectedQuotaDoesNotPoisonKey(t *testing.T) {
	repo := memory.NewEnvironmentRepository()
	ctx := context.Background()
	q := domain.Quota{MaxEnvironments: 1}
	a, b := reservation("org/app", "a"), reservation("org/app", "b")
	_, _ = repo.Reserve(ctx, a, q)
	if _, err := repo.Reserve(ctx, b, q); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatal("want quota rejection")
	}
	a.Status = domain.EnvDestroyed
	_ = repo.Save(ctx, a)
	if ok, err := repo.Reserve(ctx, b, q); !ok || err != nil {
		t.Fatal("same rejected event must succeed after slot is available")
	}
	if ok, err := repo.Reserve(ctx, b, q); ok || err != nil {
		t.Fatal("accepted duplicate must be a no-op")
	}
	got, _ := repo.FindByIdempotencyKey(ctx, "b")
	got.Status = domain.EnvDestroyed
	stored, _ := repo.FindByIdempotencyKey(ctx, "b")
	if stored.Status != domain.EnvProvisioning {
		t.Fatal("repository must not expose mutable stored pointers")
	}
}

func TestCancelAndDestructionCannotBothWin(t *testing.T) {
	for i := 0; i < 100; i++ {
		repo := memory.NewEnvironmentRepository()
		ctx := context.Background()
		mark := time.Now()
		env := reservation("org/app", "key")
		env.MarkForDeletion(mark)
		env.DeletionNotifiedAt = &mark
		_ = repo.Save(ctx, env)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var cancelled, destroyed atomic.Bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.CancelDeletion(ctx, "key", mark.Add(59*time.Second), time.Minute)
			cancelled.Store(err == nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			claimed, err := repo.ClaimDestruction(ctx, "key", mark.Add(61*time.Second), time.Minute)
			if err != nil {
				t.Error(err)
			}
			destroyed.Store(claimed != nil)
		}()
		close(start)
		wg.Wait()
		if cancelled.Load() == destroyed.Load() {
			t.Fatal("exactly one atomic transition must win")
		}
	}
}

func TestDeletionNoticeConfirmationMatchesOnlyCurrentMark(t *testing.T) {
	ctx := t.Context()
	repo := memory.NewEnvironmentRepository()
	mark := time.Now()
	env := reservation("org/app", "key")
	env.MarkForDeletion(mark)
	if err := repo.Save(ctx, env); err != nil {
		t.Fatal(err)
	}
	if stale, err := repo.RecordDeletionNotice(ctx, "key", mark.Add(-time.Minute), mark); err != nil || stale != nil {
		t.Fatal("stale mark must not confirm delivery")
	}
	confirmed, err := repo.RecordDeletionNotice(ctx, "key", mark, mark.Add(time.Second))
	if err != nil || confirmed == nil || confirmed.DeletionNotifiedAt == nil {
		t.Fatal("current mark must accept delivery confirmation")
	}
	// Returned timestamps must not expose the repository's mutable storage.
	*confirmed.DeletionNotifiedAt = mark.Add(-time.Hour)
	stored, _ := repo.FindByIdempotencyKey(ctx, "key")
	if !stored.DeletionNotifiedAt.Equal(mark.Add(time.Second)) {
		t.Fatal("notification timestamp leaked a mutable reference")
	}
	if duplicate, err := repo.RecordDeletionNotice(ctx, "key", mark, mark.Add(time.Hour)); err != nil || duplicate != nil {
		t.Fatal("repeated confirmation must not extend grace")
	}
	if _, err := repo.CancelDeletion(ctx, "key", mark.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if late, err := repo.RecordDeletionNotice(ctx, "key", mark, mark.Add(31*time.Second)); err != nil || late != nil {
		t.Fatal("late delivery must not overwrite cancellation")
	}
}
