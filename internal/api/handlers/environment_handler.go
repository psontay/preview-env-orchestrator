package handlers

import (
	"context"
	"crypto/hmac"
	"net/http"
	"strings"
	"time"

	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
)

type environmentReader interface {
	FindByIdempotencyKey(context.Context, string) (*domain.PreviewEnvironment, error)
}

// EnvironmentHandler is an authenticated diagnostics endpoint for Postman.
// Preview credentials never appear in public PR comments or logs.
type EnvironmentHandler struct {
	repo   environmentReader
	secret string
}

func NewEnvironmentHandler(repo environmentReader, secret string) *EnvironmentHandler {
	return &EnvironmentHandler{repo, secret}
}

func (h *EnvironmentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.secret == "" || !hmac.Equal([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(h.secret)) || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	env, err := h.repo.FindByIdempotencyKey(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	if env == nil {
		http.Error(w, "environment not found", http.StatusNotFound)
		return
	}
	var expires *time.Time = env.ExpiresAt
	if expires == nil {
		t := env.TTL.ExpiresAt(env.CreatedAt)
		expires = &t
	}
	response := map[string]any{"id": env.ID, "repo": env.Repo, "commit_sha": env.CommitSHA, "status": env.Status, "url": env.URL, "container_id": env.ContainerID, "expires_at": expires, "deletion_cancel_count": env.DeletionCancelCount, "marked_for_deletion_at": env.MarkedForDeletionAt, "last_error": env.LastError}
	response["resource_group"] = env.ResourceGroup
	response["deletion_notified_at"] = env.DeletionNotifiedAt
	response["destruction_notice_pending"] = env.DestructionNoticePending
	if env.Status == domain.EnvRunning {
		response["admin_username"] = "previewadmin"
		response["admin_password"] = application.PreviewSecret(h.secret, env.ID, "admin")[:32]
	}
	writeJSON(w, http.StatusOK, response)
}
