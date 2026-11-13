package handlers_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"preview-env-orchestrator/internal/api/handlers"
	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

type webhookDocker struct{ calls atomic.Int32 }

func (d *webhookDocker) Provision(_ context.Context, req application.ProvisionRequest) (application.ProvisionResult, error) {
	d.calls.Add(1)
	return application.ProvisionResult{ContainerID: "owned", URL: "http://localhost:32123"}, nil
}
func (*webhookDocker) Destroy(context.Context, string) error { return nil }

type webhookNotifier struct{}

func (webhookNotifier) CommentOnPR(context.Context, string, int, string) error { return nil }

func TestSignedWebhookValidatesSourceAndDeduplicates(t *testing.T) {
	repo := memory.NewEnvironmentRepository()
	docker := &webhookDocker{}
	svc := application.NewWebhookService(memory.NewPullRequestRepository(), repo, docker, webhookNotifier{}, domain.Quota{MaxEnvironments: 1}, domain.TTL{Duration: time.Minute}, "https://demo.example")
	h := handlers.NewWebhookHandler(svc)
	h.ConfigureRepository("org/app", "alice")
	secret := strings.Repeat("s", 64)
	wrapped := handlers.VerifyGitHubSignature(secret, h)
	sha := strings.Repeat("a", 40)
	payload := func(head, actor, commit string) string {
		return fmt.Sprintf(`{"action":"opened","pull_request":{"node_id":"PR_1","number":1,"head":{"sha":%q,"repo":{"full_name":%q}}},"repository":{"full_name":"org/app"},"sender":{"login":%q}}`, commit, head, actor)
	}
	send := func(body string, valid bool) int {
		r := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", "pull_request")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		signature := hex.EncodeToString(mac.Sum(nil))
		if !valid {
			signature = strings.Repeat("0", 64)
		}
		r.Header.Set("X-Hub-Signature-256", "sha256="+signature)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct {
		head, actor, sha string
		signature        bool
		status           int
	}{
		{"org/app", "alice", sha, false, 401}, {"fork/app", "alice", sha, true, 403}, {"org/app", "mallory", sha, true, 403}, {"org/app", "alice", "master", true, 400},
		{"org/app", "alice", sha, true, 202}, {"org/app", "alice", sha, true, 202},
	} {
		if got := send(payload(tc.head, tc.actor, tc.sha), tc.signature); got != tc.status {
			t.Fatalf("want %d got %d", tc.status, got)
		}
	}
	svc.Wait()
	if docker.calls.Load() != 1 {
		t.Fatal("duplicate delivery caused a second build")
	}
	env, err := repo.FindByIdempotencyKey(context.Background(), "org/app:PR_1:"+sha)
	if err != nil || env == nil || env.Status != domain.EnvRunning || env.CommitSHA != sha {
		t.Fatal("signed HTTP webhook did not persist correct ready preview")
	}
}

func signedDelivery(handler http.Handler, method, event, body, signature string) *httptest.ResponseRecorder {
	const secret = "webhook-unit-test-key"
	r := httptest.NewRequest(method, "/webhooks/github", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	if signature != "missing" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		value := hex.EncodeToString(mac.Sum(nil))
		if signature == "invalid" {
			value = strings.Repeat("0", 64)
		}
		r.Header.Set("X-Hub-Signature-256", "sha256="+value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func webhookTestPayload(nodeID, sha string) string {
	return fmt.Sprintf(`{"action":"opened","pull_request":{"node_id":%q,"number":1,"head":{"sha":%q,"repo":{"full_name":"org/app"}}},"repository":{"full_name":"org/app"},"sender":{"login":"alice"}}`, nodeID, sha)
}

func TestRejectedAndIgnoredWebhooksReserveNoResources(t *testing.T) {
	sha := strings.Repeat("a", 40)
	body := webhookTestPayload("PR_1", sha)
	for _, tc := range []struct {
		name, method, event, body, signature string
		status                               int
	}{
		{"missing signature", "POST", "pull_request", body, "missing", 401},
		{"wrong signature", "POST", "pull_request", body, "invalid", 401},
		{"wrong method", "GET", "pull_request", body, "valid", 405},
		{"ping", "POST", "ping", "not-json", "valid", 200},
		{"irrelevant event", "POST", "push", "not-json", "valid", 202},
		{"closed PR", "POST", "pull_request", strings.Replace(body, `"opened"`, `"closed"`, 1), "valid", 202},
		{"malformed JSON", "POST", "pull_request", "{", "valid", 400},
		{"missing PR id", "POST", "pull_request", webhookTestPayload("", sha), "valid", 400},
		{"invalid PR number", "POST", "pull_request", strings.Replace(body, `"number":1`, `"number":0`, 1), "valid", 400},
		{"missing SHA", "POST", "pull_request", webhookTestPayload("PR_1", ""), "valid", 400},
		{"nonimmutable revision", "POST", "pull_request", webhookTestPayload("PR_1", "main"), "valid", 400},
		{"missing repository", "POST", "pull_request", strings.Replace(body, `"repository":{"full_name":"org/app"}`, `"repository":{"full_name":""}`, 1), "valid", 400},
		{"wrong repository", "POST", "pull_request", strings.Replace(body, `"repository":{"full_name":"org/app"}`, `"repository":{"full_name":"other/app"}`, 1), "valid", 403},
		{"fork", "POST", "pull_request", strings.Replace(body, `"repo":{"full_name":"org/app"}`, `"repo":{"full_name":"fork/app"}`, 1), "valid", 403},
		{"wrong actor", "POST", "pull_request", strings.Replace(body, `"login":"alice"`, `"login":"mallory"`, 1), "valid", 403},
		{"oversized body", "POST", "pull_request", strings.Repeat("x", (1<<20)+1), "valid", 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envs := memory.NewEnvironmentRepository()
			docker := &webhookDocker{}
			svc := application.NewWebhookService(memory.NewPullRequestRepository(), envs, docker, webhookNotifier{},
				domain.Quota{MaxEnvironments: 1}, domain.TTL{Duration: time.Minute}, "https://demo.example")
			h := handlers.NewWebhookHandler(svc)
			h.ConfigureRepository("org/app", "alice")
			wrapped := handlers.VerifyGitHubSignature("webhook-unit-test-key", h)
			w := signedDelivery(wrapped, tc.method, tc.event, tc.body, tc.signature)
			svc.Wait()
			if w.Code != tc.status {
				t.Fatalf("want HTTP %d, got %d", tc.status, w.Code)
			}
			if docker.calls.Load() != 0 {
				t.Fatal("rejected/ignored event must not start a build")
			}
			for _, repo := range []string{"org/app", "other/app"} {
				if active, err := envs.CountActiveByRepo(t.Context(), repo); err != nil || active != 0 {
					t.Fatal("rejected/ignored event must not consume quota")
				}
			}
		})
	}
}

func TestSignedHTTPQuotaRejectionCanBeRetriedAfterCleanup(t *testing.T) {
	ctx := t.Context()
	prs, envs := memory.NewPullRequestRepository(), memory.NewEnvironmentRepository()
	docker, notifier := &webhookDocker{}, webhookNotifier{}
	svc := application.NewWebhookService(prs, envs, docker, notifier,
		domain.Quota{MaxEnvironments: 1}, domain.TTL{Duration: time.Minute}, "https://demo.example")
	h := handlers.NewWebhookHandler(svc)
	h.ConfigureRepository("org/app", "alice")
	wrapped := handlers.VerifyGitHubSignature("webhook-unit-test-key", h)
	shaA, shaB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	keyA, keyB := "org/app:PR_1:"+shaA, "org/app:PR_1:"+shaB
	send := func(sha string) *httptest.ResponseRecorder {
		return signedDelivery(wrapped, "POST", "pull_request", webhookTestPayload("PR_1", sha), "valid")
	}
	if w := send(shaA); w.Code != 202 {
		t.Fatal("first preview must be accepted")
	}
	svc.Wait()
	if w := send(shaB); w.Code != 429 || !strings.Contains(w.Body.String(), "quota exceeded") {
		t.Fatal("second preview must be rejected at quota")
	}
	if got, err := envs.FindByIdempotencyKey(ctx, keyB); err != nil || got != nil {
		t.Fatal("rejection must not persist an environment for B")
	}
	if w := send(shaA); w.Code != 202 {
		t.Fatal("accepted duplicate must remain a no-op even at quota")
	}
	a, err := envs.FindByIdempotencyKey(ctx, keyA)
	if err != nil || a == nil {
		t.Fatal("first preview missing")
	}
	// Model an elapsed, already-notified grace period without sleeping. Run
	// the real cleanup application service against the Docker test double.
	marked := time.Now().Add(-2 * time.Minute)
	a.MarkForDeletion(marked)
	a.DeletionNotifiedAt = &marked
	if err := envs.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	if w := send(shaB); w.Code != 429 {
		t.Fatal("marked environment must retain quota until cleanup completes")
	}
	cleanup := application.NewCleanupService(envs, prs, docker, notifier, "https://demo.example")
	cleanup.Configure(time.Minute, "webhook-unit-test-key")
	cleanup.Run(ctx)
	if got, err := envs.FindByIdempotencyKey(ctx, keyA); err != nil || got.Status != domain.EnvDestroyed {
		t.Fatal("cleanup must persist DESTROYED before releasing the slot")
	}
	if w := send(shaB); w.Code != 202 {
		t.Fatal("the same rejected B delivery must now be accepted")
	}
	svc.Wait()
	if got, err := envs.FindByIdempotencyKey(ctx, keyB); err != nil || got == nil || got.Status != domain.EnvRunning {
		t.Fatal("B must become RUNNING after retry")
	}
	if docker.calls.Load() != 2 {
		t.Fatal("only A and the successful B retry may build")
	}
}
