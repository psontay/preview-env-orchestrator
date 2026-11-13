package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
)

// tracer — instrumentation scope cho package handlers. Cùng tên với scope
// bên application để mọi span trong repo nằm chung một nhóm trên Jaeger.
var tracer = otel.Tracer("preview-env-orchestrator")

// githubPullRequestPayload là bản rút gọn của payload webhook GitHub gửi
// cho sự kiện "pull_request". Thực tế payload đầy đủ có rất nhiều field;
// ở đây chỉ trích các field hệ thống cần dùng.
type githubPullRequestPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		NodeID string `json:"node_id"`
		Number int    `json:"number"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Sha  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

type WebhookHandler struct {
	service      *application.WebhookService
	repository   string
	allowedActor string
}

func (h *WebhookHandler) ConfigureRepository(repo, actor string) {
	h.repository, h.allowedActor = repo, actor
}

var fullCommitSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

func NewWebhookHandler(service *application.WebhookService) *WebhookHandler {
	return &WebhookHandler{service: service}
}

// ServeHTTP nhận webhook, parse payload, map sang domain.PullRequest rồi
// giao cho application layer xử lý. Handler chỉ làm nhiệm vụ "dịch"
// HTTP <-> domain, không chứa business logic.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ---- Span 1: nhận + parse webhook ----
	// Đây là span gốc (root span) của cả trace cho một request webhook.
	// ctx trả về đã "mang" span này; mọi span con phải bắt nguồn từ ctx
	// đó, nên ta truyền ctx (không phải r.Context()) xuống service.
	ctx, span := tracer.Start(r.Context(), "webhook.receive_and_parse")
	defer span.End()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if event := r.Header.Get("X-GitHub-Event"); event == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	} else if event != "" && event != "pull_request" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var payload githubPullRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		span.RecordError(err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Chỉ xử lý các action liên quan tới việc PR có commit mới / được mở.
	if payload.Action != "opened" && payload.Action != "synchronize" && payload.Action != "reopened" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if payload.PullRequest.NodeID == "" || payload.Repository.FullName == "" || payload.PullRequest.Number <= 0 || payload.PullRequest.Head.Sha == "" {
		http.Error(w, "missing pull request fields", http.StatusBadRequest)
		return
	}
	if h.repository != "" {
		if payload.Repository.FullName != h.repository || payload.PullRequest.Head.Repo.FullName != h.repository || payload.Sender.Login != h.allowedActor {
			http.Error(w, "repository, fork or sender is not allowed for this local runner", http.StatusForbidden)
			return
		}
		if !fullCommitSHA.MatchString(payload.PullRequest.Head.Sha) {
			http.Error(w, "invalid commit SHA", http.StatusBadRequest)
			return
		}
	}

	pr := &domain.PullRequest{
		ID:         payload.PullRequest.NodeID,
		Repo:       payload.Repository.FullName,
		Number:     payload.PullRequest.Number,
		Author:     payload.PullRequest.User.Login,
		CommitHash: payload.PullRequest.Head.Sha,
		Status:     domain.PRStatusOpen,
	}
	span.SetAttributes(
		attribute.String("pr.repo", pr.Repo),
		attribute.Int("pr.number", pr.Number),
		attribute.String("pr.action", payload.Action),
	)

	if err := h.service.HandlePullRequestEvent(ctx, pr); err != nil {
		if errors.Is(err, application.ErrQuotaExceeded) {
			http.Error(w, "quota exceeded for this repository", http.StatusTooManyRequests)
			return
		}
		log.Printf("[webhook-handler] lỗi xử lý event: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 202 Accepted: provisioning đang chạy bất đồng bộ ở background,
	// client (GitHub) không cần chờ.
	w.WriteHeader(http.StatusAccepted)
}
