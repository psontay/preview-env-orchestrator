# Tích hợp OpenTelemetry

Mục tiêu: trace 1 request webhook xuyên suốt 3 bước — (1) nhận + parse
payload, (2) ghi DB, (3) gọi Docker — để khi provisioning lỗi, nhìn vào
trace biết ngay chết ở bước nào.

## Cài dependency

```bash
go get go.opentelemetry.io/otel
go get go.opentelemetry.io/otel/sdk
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
```

## Khởi tạo TracerProvider (thêm vào `cmd/api/main.go`)

```go
func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint("localhost:4317"), // Jaeger/Collector
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("preview-env-orchestrator"),
		)),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}
```

## Gắn span vào 3 bước chính

Trong `WebhookHandler.ServeHTTP` và `WebhookService.HandlePullRequestEvent`,
mở span con cho từng bước, giữ nguyên context để propagate trace ID:

```go
var tracer = otel.Tracer("preview-env-orchestrator")

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "webhook.receive_and_parse")
	defer span.End()
	// ... decode payload (đã có sẵn trong handler) ...
	// truyền ctx (không phải r.Context() nữa) xuống service:
	err := h.service.HandlePullRequestEvent(ctx, pr)
	// ...
}
```

```go
func (s *WebhookService) HandlePullRequestEvent(ctx context.Context, pr *domain.PullRequest) error {
	ctx, dbSpan := tracer.Start(ctx, "webhook.persist_state")
	// ... TryAcquireLock, Save PR, Save env ...
	dbSpan.End()

	ctx, dockerSpan := tracer.Start(ctx, "webhook.provision_docker")
	go func() {
		defer dockerSpan.End()
		result, err := s.docker.Provision(ctx, req)
		if err != nil {
			dockerSpan.RecordError(err)
			dockerSpan.SetStatus(codes.Error, err.Error())
		}
		// ...
	}()
	return nil
}
```

## Xuất trace

- **Local dev**: chạy Jaeger all-in-one bằng Docker (`jaegertracing/all-in-one`),
  UI tại `http://localhost:16686`, OTLP gRPC nhận ở cổng `4317`.
- **Production**: trỏ `otlptracegrpc.WithEndpoint` tới OpenTelemetry Collector
  nội bộ, Collector export tiếp sang backend (Jaeger, Tempo, Datadog, ...).

## Lưu ý

- Vì `Provision` chạy trong goroutine tách biệt khỏi request HTTP gốc
  (không block webhook handler), phải **truyền `ctx` mang trace ID theo
  goroutine đó** — không dùng `context.Background()` mới, nếu không span
  provisioning sẽ bị "đứt" khỏi trace gốc.
- Luôn `defer span.End()` ngay sau khi `Start`, kể cả trong nhánh lỗi.
