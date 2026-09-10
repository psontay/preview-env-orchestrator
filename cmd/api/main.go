// Command api khởi động HTTP server nhận webhook GitHub và chạy
// background worker dọn dẹp environment hết hạn.
//
// Bản này dùng adapter in-memory + mock Docker để chạy được ngay tại
// chỗ (go run ./cmd/api) không cần Postgres/Docker daemon thật — phù hợp
// để demo luồng nghiệp vụ. Khi lên production, thay các adapter trong
// phần "Wiring" bên dưới bằng bản Postgres/Docker SDK thật (xem thư mục docs/).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"preview-env-orchestrator/internal/api/handlers"
	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
	dockerinfra "preview-env-orchestrator/internal/infrastructure/docker"
	"preview-env-orchestrator/internal/infrastructure/memory"
)

func main() {
	// ---- Wiring (Composition Root) ----
	// Đây là NƠI DUY NHẤT trong toàn bộ codebase biết về các implementation
	// cụ thể. Domain và Application layer chỉ biết tới interface.
	prRepo := memory.NewPullRequestRepository()
	envRepo := memory.NewEnvironmentRepository()
	dockerSvc := dockerinfra.NewMockService()
	notifier := memory.NewGitHubNotifier()

	quota := domain.Quota{MaxEnvironments: 5}
	defaultTTL := domain.TTL{Duration: 24 * time.Hour}

	// baseURL là địa chỉ công khai mà GitHub/người dùng dùng để gọi ngược
	// lại service này (ví dụ endpoint "Hủy lệnh xoá"). Đọc từ biến môi
	// trường để không hardcode domain thật vào comment PR.
	baseURL := getenv("PUBLIC_BASE_URL", "http://localhost:8080")

	webhookService := application.NewWebhookService(prRepo, envRepo, dockerSvc, notifier, quota, defaultTTL, baseURL)
	cleanupService := application.NewCleanupService(envRepo, prRepo, dockerSvc, notifier, baseURL)

	// ---- Background worker: quét TTL mỗi 5 phút ----
	go runCleanupLoop(cleanupService, 5*time.Minute)

	// ---- HTTP server ----
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handlers.NewWebhookHandler(webhookService))
	mux.Handle("POST /environments/{id}/cancel-deletion", handlers.NewCancelDeletionHandler(envRepo))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":8080"
	log.Printf("preview-env-orchestrator đang lắng nghe tại %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server lỗi: %v", err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runCleanupLoop(svc *application.CleanupService, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		svc.Run(context.Background())
	}
}
