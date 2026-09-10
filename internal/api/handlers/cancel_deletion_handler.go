package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"preview-env-orchestrator/internal/domain"
)

// environmentCanceler là tập con hành vi của domain.EnvironmentRepository mà
// handler này cần: tra cứu một environment và lưu lại sau khi đổi trạng thái.
// Khai báo interface hẹp ngay tại nơi tiêu thụ (Interface Segregation) giúp
// handler không phụ thuộc vào toàn bộ repository.
type environmentCanceler interface {
	FindByIdempotencyKey(ctx context.Context, key string) (*domain.PreviewEnvironment, error)
	Save(ctx context.Context, env *domain.PreviewEnvironment) error
}

// CancelDeletionHandler phục vụ POST /environments/{id}/cancel-deletion.
// Nó hiện thực lời hứa "Hủy lệnh xoá tại [Link]" mà WebhookService và
// CleanupService gửi vào comment PR: gọi PreviewEnvironment.CancelDeletion()
// (business rule đã nằm sẵn trong domain) rồi lưu lại, đưa environment từ
// MARK_FOR_DELETION quay về RUNNING trước khi hết grace period.
//
// Lưu ý: {id} trên route chính là idempotency key của environment — theo
// cách WebhookService dựng environment (xem webhook_service.go), ID và
// IdempotencyKey luôn bằng nhau, nên tra cứu qua FindByIdempotencyKey.
type CancelDeletionHandler struct {
	envRepo environmentCanceler
}

func NewCancelDeletionHandler(envRepo environmentCanceler) *CancelDeletionHandler {
	return &CancelDeletionHandler{envRepo: envRepo}
}

type cancelDeletionResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func (h *CancelDeletionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing environment id", http.StatusBadRequest)
		return
	}

	env, err := h.envRepo.FindByIdempotencyKey(r.Context(), id)
	if err != nil {
		log.Printf("[cancel-deletion] lỗi tra cứu env=%s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if env == nil {
		http.Error(w, "environment not found", http.StatusNotFound)
		return
	}

	switch env.Status {
	case domain.EnvMarkedForDeletion:
		env.CancelDeletion()
		if err := h.envRepo.Save(r.Context(), env); err != nil {
			log.Printf("[cancel-deletion] lỗi lưu env=%s: %v", id, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		log.Printf("[cancel-deletion] env=%s: đã hủy lệnh xoá, quay lại RUNNING", id)
		writeJSON(w, http.StatusOK, cancelDeletionResponse{
			ID:     env.ID,
			Status: string(env.Status),
			Detail: "đã hủy lệnh xoá, môi trường quay lại RUNNING",
		})

	case domain.EnvRunning:
		// Idempotent: gọi lại khi environment vốn đang RUNNING vẫn an toàn.
		writeJSON(w, http.StatusOK, cancelDeletionResponse{
			ID:     env.ID,
			Status: string(env.Status),
			Detail: "môi trường đang RUNNING, không có lệnh xoá nào để hủy",
		})

	default:
		// DESTROYED / FAILED / PROVISIONING: không có gì hợp lệ để hủy.
		writeJSON(w, http.StatusConflict, cancelDeletionResponse{
			ID:     env.ID,
			Status: string(env.Status),
			Detail: "không thể hủy lệnh xoá ở trạng thái hiện tại",
		})
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
