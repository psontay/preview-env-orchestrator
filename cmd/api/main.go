// Command api khởi động HTTP server nhận webhook GitHub và chạy
// background worker dọn dẹp environment hết hạn.
//
// Adapter lưu trữ/Docker/GitHub được chọn qua biến môi trường
// (STORAGE_DRIVER, DOCKER_DRIVER, GITHUB_DRIVER — xem newRepositories,
// newDockerService, newGitHubNotifier bên dưới). Mặc định dùng bản
// in-memory + mock để chạy ngay tại chỗ (go run ./cmd/api) không cần
// Postgres/Docker daemon thật; đặt =postgres/real để dùng adapter thật.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"preview-env-orchestrator/internal/api/handlers"
	"preview-env-orchestrator/internal/application"
	"preview-env-orchestrator/internal/domain"
	dockerinfra "preview-env-orchestrator/internal/infrastructure/docker"
	"preview-env-orchestrator/internal/infrastructure/github"
	"preview-env-orchestrator/internal/infrastructure/memory"
	"preview-env-orchestrator/internal/infrastructure/postgres"
)

func main() {
	// ---- OpenTelemetry: khởi tạo TracerProvider trước mọi thứ khác ----
	// Nếu collector chưa chạy, exporter vẫn tạo được (nó kết nối lười) —
	// span sẽ được buffer và thử gửi lại, nên service không bị chặn.
	shutdownTracing, err := setupTracing(context.Background())
	if err != nil {
		log.Printf("otel: không khởi tạo được tracing (%v) — chạy tiếp không có trace", err)
	} else {
		defer func() { _ = shutdownTracing(context.Background()) }()
	}

	// ---- Wiring (Composition Root) ----
	// Đây là NƠI DUY NHẤT trong toàn bộ codebase biết về các implementation
	// cụ thể. Domain và Application layer chỉ biết tới interface.
	prRepo, envRepo := newRepositories()
	dockerSvc := newDockerService()
	notifier := newGitHubNotifier()

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
	// Webhook được bọc bởi middleware xác thực chữ ký HMAC-SHA256 của
	// GitHub (header X-Hub-Signature-256). Secret đọc từ GITHUB_WEBHOOK_SECRET.
	webhookHandler := handlers.VerifyGitHubSignature(
		os.Getenv("GITHUB_WEBHOOK_SECRET"),
		handlers.NewWebhookHandler(webhookService),
	)
	mux.Handle("/webhooks/github", webhookHandler)
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

// setupTracing khởi tạo OpenTelemetry TracerProvider và export span qua
// OTLP/gRPC tới một collector (Jaeger all-in-one khi chạy docker-compose,
// hoặc OpenTelemetry Collector khi lên production).
//
// Endpoint đọc từ biến môi trường OTEL_EXPORTER_OTLP_ENDPOINT, mặc định
// "localhost:4317" (cổng OTLP/gRPC chuẩn). Hàm trả về callback shutdown
// để flush nốt các span còn nằm trong batch buffer trước khi tiến trình
// thoát.
func setupTracing(ctx context.Context) (func(context.Context) error, error) {
	endpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(), // dev: không TLS giữa app và collector
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		// WithBatcher: gom span lại thành lô rồi gửi nền, không chặn
		// đường xử lý request.
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("preview-env-orchestrator"),
		)),
	)
	// SetTracerProvider đăng ký tp làm provider toàn cục — otel.Tracer(...)
	// gọi ở các package khác sẽ lấy đúng provider này.
	otel.SetTracerProvider(tp)
	log.Printf("otel: tracing bật, export OTLP/gRPC -> %s", endpoint)
	return tp.Shutdown, nil
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

// newDockerService chọn adapter điều phối container theo biến môi trường
// DOCKER_DRIVER:
//
//	mock (mặc định) — MockService, giả lập, không cần Docker daemon
//	real            — RealService, gọi Docker Engine SDK thật
func newDockerService() application.DockerOrchestrationService {
	switch driver := getenv("DOCKER_DRIVER", "mock"); driver {
	case "real":
		svc, err := dockerinfra.NewRealService()
		if err != nil {
			log.Fatalf("docker=real: %v", err)
		}
		log.Printf("docker: real (Docker Engine SDK)")
		return svc
	case "mock":
		log.Printf("docker: mock (giả lập)")
		return dockerinfra.NewMockService()
	default:
		log.Fatalf("DOCKER_DRIVER không hợp lệ: %q (chấp nhận: mock | real)", driver)
		return nil
	}
}

// newGitHubNotifier chọn adapter thông báo GitHub theo biến môi trường
// GITHUB_DRIVER:
//
//	mock (mặc định) — in ra console, không gọi mạng
//	real            — gọi GitHub REST API thật, cần GITHUB_TOKEN
func newGitHubNotifier() application.GitHubNotifier {
	switch driver := getenv("GITHUB_DRIVER", "mock"); driver {
	case "real":
		n, err := github.NewNotifierFromEnv()
		if err != nil {
			log.Fatalf("github=real: %v", err)
		}
		log.Printf("github: real (REST API)")
		return n
	case "mock":
		log.Printf("github: mock (in ra console)")
		return memory.NewGitHubNotifier()
	default:
		log.Fatalf("GITHUB_DRIVER không hợp lệ: %q (chấp nhận: mock | real)", driver)
		return nil
	}
}

func runCleanupLoop(svc *application.CleanupService, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		svc.Run(context.Background())
	}
}
