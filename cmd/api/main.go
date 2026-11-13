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
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
	if err := run(); err != nil {
		log.Printf("startup/server error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// ---- OpenTelemetry: khởi tạo TracerProvider trước mọi thứ khác ----
	// Nếu collector chưa chạy, exporter vẫn tạo được (nó kết nối lười) —
	// span sẽ được buffer và thử gửi lại, nên service không bị chặn.
	shutdownTracing, err := setupTracing(ctx)
	if err != nil {
		log.Printf("otel: không khởi tạo được tracing (%v) — chạy tiếp không có trace", err)
	} else {
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTracing(flushCtx)
		}()
	}

	// ---- Wiring (Composition Root) ----
	// Đây là NƠI DUY NHẤT trong toàn bộ codebase biết về các implementation
	// cụ thể. Domain và Application layer chỉ biết tới interface.
	prRepo, envRepo, closeStorage, err := newRepositories(ctx)
	if err != nil {
		return err
	}
	defer closeStorage()
	dockerSvc, closeDocker, err := newDockerService(cfg)
	if err != nil {
		return err
	}
	defer closeDocker()
	notifier := newGitHubNotifier()

	quota := domain.Quota{MaxEnvironments: cfg.Quota}
	defaultTTL := domain.TTL{Duration: cfg.TTL}

	// baseURL là địa chỉ công khai mà GitHub/người dùng dùng để gọi ngược
	// lại service này (ví dụ endpoint "Hủy lệnh xoá"). Đọc từ biến môi
	// trường để không hardcode domain thật vào comment PR.
	baseURL := cfg.BaseURL

	webhookService := application.NewWebhookService(prRepo, envRepo, dockerSvc, notifier, quota, defaultTTL, baseURL)
	cleanupService := application.NewCleanupService(envRepo, prRepo, dockerSvc, notifier, baseURL)
	webhookService.ConfigureRuntime(ctx, cfg.ProvisionTimeout, cfg.Secret)
	cleanupService.Configure(cfg.Grace, cfg.Secret)
	defer func() { stop(); webhookService.Wait() }()
	if err := envRepo.RecoverDestructions(ctx); err != nil {
		return err
	}
	if err := webhookService.Recover(ctx); err != nil {
		return fmt.Errorf("recover pending builds: %w", err)
	}

	// ---- Background worker: sweep interval from configuration ----
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); runCleanupLoop(ctx, cleanupService, cfg.Sweep) }()

	// ---- HTTP server ----
	mux := http.NewServeMux()
	// Webhook được bọc bởi middleware xác thực chữ ký HMAC-SHA256 của
	// GitHub (header X-Hub-Signature-256). Secret đọc từ GITHUB_WEBHOOK_SECRET.
	webhook := handlers.NewWebhookHandler(webhookService)
	if getenv("DOCKER_DRIVER", "mock") == "real" {
		webhook.ConfigureRepository(cfg.Repository, cfg.Actor)
	}
	mux.Handle("POST /webhooks/github", handlers.VerifyGitHubSignature(cfg.Secret, webhook))
	canceler := handlers.NewCancelDeletionHandler(envRepo)
	canceler.Configure(cfg.Grace, cfg.Secret)
	mux.Handle("POST /environments/{id}/cancel-deletion", canceler)
	mux.Handle("GET /environments/{id}/cancel-deletion", canceler)
	mux.Handle("GET /environments/{id}", handlers.NewEnvironmentHandler(envRepo, cfg.Secret))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Preview environment orchestrator is running.\nHealth check: /healthz\nGitHub webhook: POST /webhooks/github\n"))
	})

	addr := getenv("HTTP_ADDR", ":8080")
	log.Printf("preview-env-orchestrator đang lắng nghe tại %s", addr)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-serverErrors:
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	stop()
	webhookService.Wait()
	<-workerDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
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
func newRepositories(ctx context.Context) (domain.PullRequestRepository, domain.EnvironmentRepository, func(), error) {
	switch driver := getenv("STORAGE_DRIVER", "memory"); driver {
	case "postgres":
		startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		pool, err := postgres.NewPool(startupCtx)
		if err != nil {
			return nil, nil, nil, errors.New("cannot connect to orchestrator PostgreSQL; check DATABASE_URL and database readiness")
		}
		if err := postgres.RunMigrations(startupCtx, pool, getenv("MIGRATIONS_DIR", "migrations")); err != nil {
			pool.Close()
			return nil, nil, nil, err
		}
		log.Printf("storage: postgres")
		return postgres.NewPullRequestRepository(pool), postgres.NewEnvironmentRepository(pool), pool.Close, nil
	case "memory":
		log.Printf("storage: memory (in-process, không bền vững)")
		return memory.NewPullRequestRepository(), memory.NewEnvironmentRepository(), func() {}, nil
	default:
		return nil, nil, nil, fmt.Errorf("invalid STORAGE_DRIVER: %q", driver)
	}
}

// newDockerService chọn adapter điều phối container theo biến môi trường
// DOCKER_DRIVER:
//
//	mock (mặc định) — MockService, giả lập, không cần Docker daemon
//	real            — RealService, gọi Docker Engine SDK thật
func newDockerService(cfg config) (application.DockerOrchestrationService, func(), error) {
	switch driver := getenv("DOCKER_DRIVER", "mock"); driver {
	case "real":
		svc, err := dockerinfra.NewRealService()
		if err != nil {
			return nil, nil, err
		}
		if err := svc.ConfigurePreview(github.NewSource(os.Getenv("GITHUB_TOKEN"), "", nil), dockerinfra.StackConfig{Repository: cfg.Repository, Secret: cfg.Secret, ControlContainer: os.Getenv("ORCHESTRATOR_CONTAINER"), ReadyTimeout: cfg.ReadyTimeout}); err != nil {
			_ = svc.Close()
			return nil, nil, err
		}
		log.Printf("docker: real (Docker Engine SDK)")
		return svc, func() { _ = svc.Close() }, nil
	case "mock":
		log.Printf("docker: mock (giả lập)")
		return dockerinfra.NewMockService(), func() {}, nil
	default:
		return nil, nil, fmt.Errorf("invalid DOCKER_DRIVER: %q", driver)
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

func runCleanupLoop(ctx context.Context, svc *application.CleanupService, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			svc.Run(ctx)
		}
	}
}
