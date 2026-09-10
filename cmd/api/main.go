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
	"preview-env-orchestrator/internal/infrastructure/postgres"
)

func main() {
	// ---- Wiring (Composition Root) ----
	// Đây là NƠI DUY NHẤT trong toàn bộ codebase biết về các implementation
	// cụ thể. Domain và Application layer chỉ biết tới interface.
	prRepo, envRepo := newRepositories()
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

// newRepositories chọn adapter lưu trữ theo biến môi trường STORAGE_DRIVER:
//
//	memory   (mặc định) — in-process, mất khi restart, tiện cho dev/demo
//	postgres            — dùng pgxpool, đọc DSN từ DATABASE_URL
//
// Cả hai đều implement cùng interface trong internal/domain nên phần còn lại
// của chương trình không đổi.
func newRepositories() (domain.PullRequestRepository, domain.EnvironmentRepository) {
	switch driver := getenv("STORAGE_DRIVER", "memory"); driver {
	case "postgres":
		pool, err := postgres.NewPool(context.Background())
		if err != nil {
			log.Fatalf("storage=postgres: %v", err)
		}
		log.Printf("storage: postgres")
		return postgres.NewPullRequestRepository(pool), postgres.NewEnvironmentRepository(pool)
	case "memory":
		log.Printf("storage: memory (in-process, không bền vững)")
		return memory.NewPullRequestRepository(), memory.NewEnvironmentRepository()
	default:
		log.Fatalf("STORAGE_DRIVER không hợp lệ: %q (chấp nhận: memory | postgres)", driver)
		return nil, nil
	}
}

func runCleanupLoop(svc *application.CleanupService, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		svc.Run(context.Background())
	}
}
