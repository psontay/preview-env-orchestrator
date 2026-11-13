package domain_test

import (
	"errors"
	"testing"
	"time"

	"preview-env-orchestrator/internal/domain"
)

func TestExtensionOnlyOnceAndKeepsGraceLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	env := newEnv(now.Add(-time.Hour), time.Minute)
	env.MarkForDeletion(now.Add(-30 * time.Second))
	if err := env.ExtendOnce(now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if env.DeletionCancelCount != 1 || env.ExpiresAt == nil || !env.ExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatal("extension must expire two minutes after cancellation")
	}
	if env.IsExpired(now.Add(time.Minute)) {
		t.Fatal("old creation TTL must not expire extended environment")
	}
	if !env.IsExpired(now.Add(2*time.Minute + time.Nanosecond)) {
		t.Fatal("extension must expire")
	}
	env.MarkForDeletion(now.Add(2*time.Minute + time.Second))
	notified := now.Add(2*time.Minute + time.Second)
	env.DeletionNotifiedAt = &notified
	if err := env.ExtendOnce(now.Add(2*time.Minute+2*time.Second), time.Minute); !errors.Is(err, domain.ErrCannotCancelDeletion) {
		t.Fatal("second cancellation must be rejected")
	}
	if env.ReadyToDestroy(now.Add(2*time.Minute+30*time.Second), time.Minute) {
		t.Fatal("second expiry must still wait for grace")
	}
	if !env.ReadyToDestroy(now.Add(4*time.Minute), time.Minute) {
		t.Fatal("second grace must eventually destroy")
	}
}

func TestExtensionRejectsElapsedGrace(t *testing.T) {
	now := time.Now()
	for _, elapsed := range []time.Duration{time.Minute, time.Minute + time.Nanosecond} {
		env := newEnv(now, time.Hour)
		env.MarkForDeletion(now)
		env.DeletionNotifiedAt = &now
		if !errors.Is(env.ExtendOnce(now.Add(elapsed), time.Minute), domain.ErrCannotCancelDeletion) {
			t.Fatal("must reject at and after grace deadline")
		}
		if env.Status != domain.EnvMarkedForDeletion || env.DeletionCancelCount != 0 {
			t.Fatal("rejection must not mutate lifecycle")
		}
	}
}

func TestDeletionWaitsForWarningAndUsesDeliveryDeadline(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	env := newEnv(now, time.Minute)
	env.MarkForDeletion(now)
	if env.ReadyToDestroy(now.Add(time.Hour), time.Minute) {
		t.Fatal("must not destroy without a delivered warning")
	}
	notified := now.Add(time.Hour)
	env.DeletionNotifiedAt = &notified
	if env.ReadyToDestroy(notified.Add(time.Minute), time.Minute) {
		t.Fatal("must wait a full grace period after late delivery")
	}
	if err := env.ExtendOnce(notified.Add(59*time.Second), time.Minute); err != nil {
		t.Fatalf("late notification must allow cancellation during its grace: %v", err)
	}
	if env.MarkedForDeletionAt != nil || env.DeletionNotifiedAt != nil {
		t.Fatal("cancellation must clear both deletion timestamps")
	}
	env.MarkForDeletion(notified.Add(3 * time.Minute))
	if env.DeletionNotifiedAt != nil || env.ReadyToDestroy(notified.Add(time.Hour), time.Minute) {
		t.Fatal("each deletion cycle needs its own warning")
	}
}

func TestCancellationWhileWarningIsPending(t *testing.T) {
	now := time.Now()
	env := newEnv(now, time.Minute)
	env.MarkForDeletion(now)
	if err := env.ExtendOnce(now.Add(time.Hour), time.Minute); err != nil {
		t.Fatalf("grace has not started while delivery is pending: %v", err)
	}
}
