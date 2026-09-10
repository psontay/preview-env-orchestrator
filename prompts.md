# prompts.md — Bộ prompt hoàn thiện đồ án

> Đọc `CLAUDE.md` trước — mục 4 của file đó quy định quy trình bắt buộc
> khi làm việc với file này (một prompt một lượt, cập nhật trạng thái,
> ưu tiên dùng MCP...). Agent PHẢI đọc và cập nhật đúng bảng TRẠNG THÁI
> ngay dưới đây sau mỗi prompt — đây là "bộ nhớ tiến độ" duy nhất sống
> sót qua các lần `/clear`.

> TRẠNG THÁI:
>
> | Prompt | Trạng thái | Ghi chú |
> |---|---|---|
> | 0 — Khảo sát & đối chiếu rubric | ✅ | Báo cáo hiện trạng, không sửa code. build+test pass. |
> | 1 — Xác nhận trước thao tác phá huỷ | ✅ | Thêm handler cancel_deletion_handler.go + route POST /environments/{id}/cancel-deletion trong main.go; WebhookService/CleanupService nhúng link hủy (urls.go, baseURL qua PUBLIC_BASE_URL); test cancel_deletion_handler_test.go (7 case, gồm luồng mark→cancel→RUNNING→sweep không destroy). Không đụng domain. build+test pass. |
> | 2 — Tích hợp PostgreSQL thật | ✅ | Package internal/infrastructure/postgres/ (pool.go, migrate.go, pr_repository.go, env_repository.go) dùng pgx/v5 + pgxpool. TryAcquireLock = INSERT ... ON CONFLICT DO NOTHING 1 statement + RowsAffected trên bảng idempotency_keys mới (migrations/002). CountActiveByRepo dùng cột repo denormalized, không JOIN. Integration test testcontainers-go behind build tag `integration` (repository_integration_test.go) — đã chạy pass với Postgres thật. main.go chọn adapter qua STORAGE_DRIVER (memory mặc định). LƯU Ý: go.mod bị nâng lên go 1.25.0 do dep testcontainers-go v0.44.0 (Prompt 6/8 dùng 1.25 thay vì 1.22). build+test (không tag) pass. |
> | 3 — Tích hợp Docker Engine SDK thật | ⬜ | |
> | 4 — Tích hợp GitHub API thật + bảo mật webhook | ⬜ | |
> | 5 — Tích hợp OpenTelemetry thật | ⬜ | |
> | 6 — Đóng gói triển khai (Dockerfile, compose, Makefile) | ⬜ | |
> | 7 — Chuẩn hoá dự án mã nguồn mở | ⬜ | |
> | 8 — CI/CD bằng GitHub Actions | ⬜ | |
> | 9 — Nâng chất lượng kiểm thử domain | ⬜ | |
>
> Ký hiệu: ⬜ chưa bắt đầu · 🔶 đang làm dở / build-test đang fail · ✅ đã xong, build+test pass

---

## Prompt 0 — Khảo sát & đối chiếu rubric

> "Đọc toàn bộ repo này và file CLAUDE.md. Liệt kê lại cho tôi dưới dạng
> bảng: (1) phần nào của đề tài đã hiện thực thật, (2) phần nào đang là
> mock/in-memory, (3) phần nào hoàn toàn chưa có, đối chiếu với 4 điểm
> nhấn chấm điểm (quota, TTL, idempotency, xác nhận trước phá huỷ) và
> tech stack yêu cầu (PostgreSQL, GitHub API, Docker, OpenTelemetry).
> Đừng sửa code ở bước này, chỉ báo cáo hiện trạng."

---

## Prompt 1 — Hoàn thiện "xác nhận trước thao tác phá huỷ"

> "Endpoint hiện tại chưa cho phép hủy lệnh xoá dù comment PR có nhắc tới
> việc này. Hãy thêm một HTTP endpoint `POST /environments/{id}/cancel-deletion`
> gọi `PreviewEnvironment.CancelDeletion()` (đã có sẵn trong domain), cập
> nhật lại URL trong comment GitHub ở `WebhookService`/`CleanupService` để
> trỏ đúng tới endpoint này (ví dụ: `http://<host>/environments/{id}/cancel-deletion`).
> Viết test cho luồng: env bị đánh dấu MARK_FOR_DELETION → gọi cancel →
> env quay lại RUNNING → CleanupService lần quét sau không xoá nó nữa.
> Không sửa domain, chỉ thêm handler + wiring + test."

---

## Prompt 2 — Tích hợp PostgreSQL thật (dùng pgx)

> "Dùng schema có sẵn ở `migrations/001_init.sql`. Tạo package
> `internal/infrastructure/postgres/` implement đầy đủ
> `domain.PullRequestRepository` và `domain.EnvironmentRepository` bằng
> `github.com/jackc/pgx/v5`. Yêu cầu bắt buộc:
> - `TryAcquireLock` phải dùng `INSERT ... ON CONFLICT (idempotency_key) DO NOTHING`
>   trong 1 statement duy nhất, kiểm tra `RowsAffected()` — KHÔNG được
>   SELECT-rồi-INSERT riêng lẻ vì sẽ tạo race condition.
> - Dùng connection pool (`pgxpool`), đọc DSN từ biến môi trtrường `DATABASE_URL`.
> - `CountActiveByRepo` dùng cột `repo` đã denormalize sẵn trong bảng
>   `preview_environments`, có index — không JOIN nếu không cần.
> - Viết integration test dùng `testcontainers-go` để tự động bật một
>   Postgres tạm, chạy migration, test cả 2 repository. Test này tách
>   riêng bằng build tag `integration` để không chạy trong `go test ./...`
>   thông thường (tránh phụ thuộc Docker khi CI không có sẵn).
> Sau khi xong, sửa `cmd/api/main.go` để chọn adapter (memory hay postgres)
> dựa trên biến môi trường `STORAGE_DRIVER` (mặc định `memory` để dev
> nhanh, `postgres` cho production)."

---

## Prompt 3 — Tích hợp Docker Engine SDK thật

> "Theo đúng hướng dẫn ở `docs/DOCKER_SDK_INTEGRATION.md`, tạo
> `internal/infrastructure/docker/real_service.go` implement
> `application.DockerOrchestrationService` bằng `github.com/docker/docker/client`
> (không dùng os/exec gọi CLI docker). Container cần: (1) chạy image cấu
> hình sẵn, (2) copy + chạy file seed .sql qua CopyToContainer + ContainerExecCreate,
> (3) lấy port động vừa cấp phát để build URL preview. Thêm cờ môi trường
> `DOCKER_DRIVER=mock|real` trong `main.go` để chọn adapter. Giữ nguyên
> `MockService` để dùng trong unit test — không xoá."

---

## Prompt 4 — Tích hợp GitHub API thật + bảo mật webhook

> "Thay `internal/infrastructure/memory/github_notifier.go` bằng một
> adapter thật gọi GitHub REST API (`POST /repos/{owner}/{repo}/issues/{number}/comments`)
> dùng token đọc từ biến môi trường `GITHUB_TOKEN`. Đồng thời, thêm middleware
> xác thực chữ ký webhook: GitHub ký payload bằng HMAC-SHA256 trong header
> `X-Hub-Signature-256`, secret đọc từ biến môi trường `GITHUB_WEBHOOK_SECRET`.
> Nếu chữ ký không khớp, trả về 401 và KHÔNG xử lý payload. Viết test cho
> cả 2 trường hợp: chữ ký đúng thì xử lý, chữ ký sai thì bị từ chối."

---

## Prompt 5 — Tích hợp OpenTelemetry thật

> "Theo hướng dẫn ở `docs/OPENTELEMETRY.md`, tích hợp OpenTelemetry SDK
> thật vào `cmd/api/main.go` (TracerProvider export qua OTLP gRPC tới
> `localhost:4317`, đọc endpoint từ biến môi trường `OTEL_EXPORTER_OTLP_ENDPOINT`).
> Gắn 3 span theo đúng luồng: nhận+parse webhook, ghi DB (idempotency +
> quota + save), gọi Docker provisioning. Đảm bảo context mang trace ID
> được truyền đúng vào goroutine bất đồng bộ provisioning (không tạo
> context.Background() mới ở đó). Thêm `docker-compose.yml` chạy kèm
> Jaeger all-in-one để có thể xem trace ngay khi `docker compose up`."

---

## Prompt 6 — Đóng gói triển khai (Dockerfile, compose, Makefile, config)

> "Tạo: (1) `Dockerfile` multi-stage build cho chính ứng dụng Go (build
> stage dùng golang:1.22, run stage dùng distroless hoặc alpine tối giản);
> (2) `docker-compose.yml` chạy app + Postgres + Jaeger cùng lúc, có
> healthcheck cho Postgres trước khi app start; (3) `.env.example` liệt
> kê đầy đủ biến môi trường app cần (`DATABASE_URL`, `GITHUB_TOKEN`,
> `GITHUB_WEBHOOK_SECRET`, `STORAGE_DRIVER`, `DOCKER_DRIVER`,
> `OTEL_EXPORTER_OTLP_ENDPOINT`), thêm `.env` thật vào `.gitignore`;
> (4) `Makefile` với các target `run`, `test`, `build`, `lint`,
> `compose-up`, `migrate`."

---

## Prompt 7 — Chuẩn hoá dự án mã nguồn mở

> "Bổ sung các file chuẩn cho một dự án open-source: `LICENSE` (MIT),
> `CONTRIBUTING.md` (quy trình fork/branch/commit convention/PR), file
> `.github/ISSUE_TEMPLATE/bug_report.md` và `feature_request.md`,
> `.github/PULL_REQUEST_TEMPLATE.md`. Cập nhật `README.md` ở root: thêm
> badge (build status), mục 'Kiến trúc' kèm sơ đồ class/luồng dạng
> Mermaid (aggregate, value object, service, port/adapter), mục 'Đóng
> góp' trỏ tới CONTRIBUTING.md."

---

## Prompt 8 — CI/CD bằng GitHub Actions

> "Tạo `.github/workflows/ci.yml`: chạy trên mọi push/PR vào `main`,
> các bước: checkout → setup-go 1.22 → `gofmt -l .` (fail nếu có output)
> → `go vet ./...` → `go build ./...` → `go test ./... -race -cover`.
> Thêm bước riêng chạy test có build tag `integration` (Prompt 2) dùng
> service Postgres do GitHub Actions cung cấp sẵn (`services: postgres:`),
> chỉ chạy sau khi các bước trên pass."

---

## Prompt 9 — Nâng chất lượng kiểm thử domain (đúng trọng tâm 'hướng đối tượng')

> "Đề tài yêu cầu đánh giá khả năng mô hình hoá miền và phân bổ trách
> nhiệm cho lớp/đối tượng — hiện tại `internal/domain/` chưa có unit test
> riêng (chỉ được test gián tiếp qua application layer). Viết unit test
> thuần cho từng entity/value object trong domain, không cần mock:
> `Quota.IsExceeded`, `TTL.ExpiresAt`, `PreviewEnvironment.IsExpired`,
> `MarkForDeletion`, `CancelDeletion`, `ReadyToDestroy` (test đủ các
> nhánh: đúng hạn, chưa hạn, đang chờ xoá nhưng chưa đủ grace period, đã
> đủ grace period, gọi CancelDeletion giữa chừng). Đây là phần dễ ăn điểm
> nhất cho tiêu chí OOP/domain modeling vì logic đã có sẵn, chỉ thiếu test
> trực tiếp chứng minh."

---

## Gợi ý thứ tự ưu tiên nếu thiếu thời gian

Nếu không kịp làm hết, ưu tiên theo đúng trọng số "điểm nhấn cập nhật" của
đề bài: **Prompt 1 (destructive safety) → Prompt 9 (test domain) → Prompt 2
(Postgres, vì idempotency thật sự chỉ đáng tin khi có DB) → Prompt 4
(GitHub API + bảo mật webhook) → Prompt 3 (Docker SDK) → Prompt 5
(OpenTelemetry)**. Prompt 6, 7, 8 là phần "mã nguồn mở" cộng điểm hình
thức, làm sau cùng nếu còn thời gian.
