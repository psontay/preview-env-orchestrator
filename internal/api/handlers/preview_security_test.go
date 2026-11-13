package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"preview-env-orchestrator/internal/api/handlers"
	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

func TestSignedCancelPageDoesNotMutateAndOnlyExtendsOnce(t *testing.T) {
	ctx := context.Background()
	repo := memory.NewEnvironmentRepository()
	secret := strings.Repeat("s", 64)
	id := "org/app:PR_1:sha"
	env := &domain.PreviewEnvironment{ID: id, IdempotencyKey: id, Status: domain.EnvRunning, CreatedAt: time.Now().Add(-time.Hour), TTL: domain.TTL{Duration: time.Minute}}
	env.MarkForDeletion(time.Now())
	_ = repo.Save(ctx, env)
	h := handlers.NewCancelDeletionHandler(repo)
	h.Configure(time.Minute, secret)
	mux := http.NewServeMux()
	mux.Handle("GET /environments/{id}/cancel-deletion", h)
	mux.Handle("POST /environments/{id}/cancel-deletion", h)
	endpoint := application.CancelDeletionURL("", id, secret)
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", endpoint, 200}, {"POST", "/environments/" + url.PathEscape(id) + "/cancel-deletion", 403}, {"POST", endpoint, 200}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s want %d, got %d", tc.method, tc.status, w.Code)
		}
		if tc.method == "GET" {
			got, _ := repo.FindByIdempotencyKey(ctx, id)
			if got.Status != domain.EnvMarkedForDeletion {
				t.Fatal("GET changed lifecycle")
			}
		}
	}
	got, _ := repo.FindByIdempotencyKey(ctx, id)
	if got.DeletionCancelCount != 1 || got.ExpiresAt == nil {
		t.Fatal("extension not persisted")
	}
	got.MarkForDeletion(time.Now())
	_ = repo.Save(ctx, got)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", endpoint, nil))
	if w.Code != 409 {
		t.Fatal("must reject second extension")
	}
}

func TestPreviewCredentialsRequireBearerAuthentication(t *testing.T) {
	repo := memory.NewEnvironmentRepository()
	secret := strings.Repeat("s", 64)
	id := "org/app:PR_1:sha"
	_ = repo.Save(context.Background(), &domain.PreviewEnvironment{ID: id, IdempotencyKey: id, Status: domain.EnvRunning})
	mux := http.NewServeMux()
	mux.Handle("GET /environments/{id}", handlers.NewEnvironmentHandler(repo, secret))
	for _, token := range []string{"", "wrong", secret} {
		r := httptest.NewRequest("GET", "/environments/"+url.PathEscape(id), nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if token != secret {
			if w.Code != 401 || strings.Contains(w.Body.String(), "admin_password") {
				t.Fatal("unauthenticated credential disclosure")
			}
		} else if w.Code != 200 || !strings.Contains(w.Body.String(), "admin_password") {
			t.Fatal("authenticated status must return Postman credentials")
		}
	}
}
