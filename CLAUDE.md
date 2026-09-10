# CLAUDE.md — Ngữ cảnh dự án cho Agent

> File này dành cho AI agent (Claude Code hoặc tương đương) đọc trước khi
> chỉnh sửa bất kỳ thứ gì trong repo. Mục tiêu: biến khung sườn kiến trúc
> hiện có thành một đồ án môn học "Phát triển phần mềm mã nguồn mở" hoàn
> chỉnh, đúng rubric, có thể publish lên GitHub như một open-source project
> thật sự (không chỉ là bài tập chạy local).

## 1. Đề bài (chép nguyên văn từ đề cương môn học)

**Tên đề tài:** Cổng điều phối môi trường preview theo pull request

**Mô tả:** Thiết kế, hiện thực và kiểm chứng một hệ thống hướng đối tượng
cho bài toán "cổng điều phối môi trường preview theo pull request". Hệ
thống phải tạo môi trường tạm, seed dữ liệu giả, thu URL, kiểm thử và dọn
dẹp. Đồ án nhằm đánh giá khả năng lập trình hướng đối tượng, mô hình hóa
miền, phân bổ trách nhiệm cho lớp/đối tượng, thiết kế kiến trúc, kiểm thử
và triển khai. **Điểm nhấn chấm điểm: quota, TTL, idempotency và xác nhận
trước thao tác phá hủy.**

**Tech stack yêu cầu:** Go (đã chọn, thay vì TypeScript/NestJS hoặc Java);
PostgreSQL; GitHub API; Docker; OpenTelemetry.

**Đây là môn "mã nguồn mở"** — ngoài phần kỹ thuật, bài chấm nhiều khả năng
còn xét tới: quy trình phát triển mở (git workflow, commit convention,
CONTRIBUTING, issue/PR template), CI khả dụng công khai, LICENSE, tài liệu
kiến trúc — không chỉ riêng code chạy được.

## 2. Trạng thái hiện tại của repo — ĐỌC KỸ TRƯỚC KHI SỬA

Repo đã có kiến trúc DDD 3 lớp, **build và test pass thật** (`go build ./...`,
`go test ./...`), nhưng phần lớn adapter đang là **bản giả lập để demo luồng
nghiệp vụ**, KHÔNG phải tích hợp thật. Agent không được nhầm lẫn phần nào
là thật, phần nào là mock — dưới đây liệt kê rõ ràng:

| Thành phần | Trạng thái | File |
|---|---|---|
| Domain model (`PullRequest`, `PreviewEnvironment`, `Quota`, `TTL`) | ✅ Thật, đã test | `internal/domain/*.go` |
| Idempotency logic (khóa theo key, chặn duplicate webhook) | ✅ Thật, đã test, nhưng lock hiện là `map + mutex` trong RAM | `internal/application/webhook_service.go`, `internal/infrastructure/memory/env_repository.go` |
| Quota check | ✅ Thật, đã test | `internal/application/webhook_service.go` |
| TTL sweep + destructive safety (đánh dấu trước, xoá sau grace period) | ✅ Logic thật, đã test | `internal/application/cleanup_service.go` |
| **Endpoint "Hủy lệnh xoá"** mà comment PR hứa hẹn | ❌ CHƯA CÓ — comment nói "Hủy lệnh tại [Link]" nhưng chưa có route nào gọi `env.CancelDeletion()` | cần thêm ở `internal/api/handlers/` |
| Lưu trữ dữ liệu | ❌ In-memory (`map`), mất khi restart | `internal/infrastructure/memory/*.go` — cần thay bằng Postgres, schema đã có sẵn ở `migrations/001_init.sql` |
| Docker orchestration | ❌ `MockService` giả lập, KHÔNG gọi Docker daemon thật | `internal/infrastructure/docker/mock_service.go` — hướng dẫn thay thế ở `docs/DOCKER_SDK_INTEGRATION.md` |
| GitHub API | ❌ Chỉ `fmt.Printf` ra console, không gọi GitHub thật, không verify webhook signature | `internal/infrastructure/memory/github_notifier.go` |
| OpenTelemetry | ❌ Chưa tích hợp gì cả, chỉ có tài liệu hướng dẫn | `docs/OPENTELEMETRY.md` |
| Webhook signature verification (bảo mật) | ❌ Chưa có — hiện ai gửi POST cũng được xử lý | `internal/api/handlers/webhook_handler.go` |
| Cấu hình qua biến môi trường | ❌ Chưa có, mọi thứ đang hardcode trong `main.go` | — |
| Dockerfile cho chính app | ❌ Chưa có | — |
| docker-compose (app + Postgres + Jaeger) | ❌ Chưa có | — |
| CI (GitHub Actions) | ❌ Chưa có | — |
| LICENSE / CONTRIBUTING.md / issue-PR templates | ❌ Chưa có | — |
| Unit test cho `domain/` (thuần, không cần mock) | ❌ Chưa có — hiện chỉ test qua `application/` | — |
| Kiểm thử tích hợp với Postgres/Docker thật (testcontainers) | ❌ Chưa có | — |

## 3. Nguyên tắc bắt buộc khi sửa (KHÔNG được vi phạm)

1. **Không phá vỡ ranh giới DDD đã có.** `internal/domain/` không được
   import bất kỳ package nào khác trong repo (không Postgres driver,
   không Docker SDK, không HTTP). Nếu cần thêm business rule, thêm vào
   domain dưới dạng method thuần, không side-effect.
2. **Không sửa chữ ký các port** (`domain.PullRequestRepository`,
   `domain.EnvironmentRepository`, `application.DockerOrchestrationService`,
   `application.GitHubNotifier`) trừ khi thực sự cần — các adapter mock
   hiện tại và adapter thật sắp thêm đều phải implement cùng interface.
3. **Giữ nguyên các adapter in-memory/mock hiện có**, không xoá — chuyển
   chúng thành "test doubles" dùng trong unit test, còn `main.go` sẽ wiring
   sang adapter thật qua biến môi trường (`ENV=dev` dùng mock, `ENV=prod`
   dùng thật) hoặc build tag.
4. **Mỗi lần thêm một adapter thật, phải viết kèm test** — domain/application
   test hiện có (`go test ./...`) phải luôn pass sau mỗi commit.
5. **Chạy đủ 3 lệnh sau trước khi coi một task là xong:**
   ```bash
   gofmt -l .   # phải rỗng
   go vet ./...
   go build ./... && go test ./...
   ```
6. **Idempotency, Quota, TTL, Destructive-safety là 4 điểm nhấn chấm
   điểm** — bất kỳ thay đổi nào cũng không được làm suy yếu 4 cơ chế này.
   Đặc biệt: khi chuyển sang Postgres, `TryAcquireLock` phải giữ đúng ngữ
   nghĩa "chỉ 1 tiến trình được xử lý 1 key", dùng `INSERT ... ON CONFLICT
   DO NOTHING` hoặc `pg_advisory_xact_lock` — không dùng `SELECT` rồi
   `INSERT` riêng lẻ (race condition).
7. **Quy ước commit:** dùng Conventional Commits (`feat:`, `fix:`, `docs:`,
   `test:`, `chore:`) vì đây là môn mã nguồn mở — lịch sử commit sạch cũng
   được xem là một phần của sản phẩm.
8. **Không commit secret.** Thông tin kết nối Postgres, GitHub webhook
   secret phải đọc từ biến môi trường (`.env` liệt kê trong `.env.example`,
   file `.env` thật phải nằm trong `.gitignore`).

## 4. Quy trình làm việc bắt buộc (áp dụng cho MỌI phiên, kể cả sau /clear)

File này được Claude Code tự động nạp lại ở đầu mỗi phiên mới, nên các
quy tắc dưới đây LUÔN có hiệu lực dù người dùng có gõ `/clear` giữa chừng
hay không — không cần người dùng nhắc lại.

1. **Nguồn việc duy nhất là `prompts.md`.** Nó chứa bộ prompt đã sắp theo
   thứ tự ưu tiên (Prompt 0 → 9, có thêm Prompt 5b), tương ứng 4 giai đoạn
   gốc + phần bổ sung cho tiêu chí "mã nguồn mở". **Prompt 5b là bắt buộc,
   không phải tuỳ chọn** — người dùng chưa học OpenTelemetry trước khi làm
   đồ án này, nên sau khi hoàn thành Prompt 5 PHẢI tự động chạy tiếp Prompt
   5b (không cần người dùng nhắc) trước khi báo cáo là "xong phần OTel".
2. **Trước khi làm bất cứ việc gì, mở `prompts.md` và tìm dòng
   `> TRẠNG THÁI:`** ở đầu file — đó là con trỏ tiến độ. Nó ghi prompt nào
   đã xong (✅), đang làm dở (🔶), hay chưa bắt đầu (⬜). Luôn tiếp tục từ
   prompt đầu tiên CHƯA có ✅, không làm lại prompt đã ✅, không nhảy cóc.
3. **Chỉ làm ĐÚNG MỘT prompt mỗi lượt**, không gộp. Sau khi làm xong một
   prompt:
   - Chạy: `gofmt -l . && go vet ./... && go build ./... && go test ./...`
   - Nếu pass: sửa dòng trạng thái của prompt đó trong `prompts.md` thành
     ✅, ghi thêm 1 dòng note ngắn (file nào đã thêm/sửa).
   - Nếu fail: giữ nguyên 🔶, không được tự ý đánh dấu ✅.
   - Báo cáo ngắn gọn cho người dùng: đã sửa/thêm file gì, kết quả
     build/test, rồi DỪNG LẠI chờ xác nhận — không tự động chạy tiếp
     sang prompt kế.
4. **Ưu tiên dùng MCP server khả dụng** (context7, github, postgres, ...)
   để tra API/docs mới nhất hoặc kiểm tra trực tiếp thay vì đoán theo trí
   nhớ training — đặc biệt quan trọng ở các prompt liên quan Docker SDK,
   pgx, OpenTelemetry, GitHub API (dễ hallucinate tên method/field vì các
   SDK này đổi version thường xuyên).
5. **Nếu người dùng chỉ gõ một câu ngắn kiểu "tiếp tục" sau khi mở phiên
   mới**, hiểu đó là: đọc con trỏ trạng thái trong `prompts.md`, làm đúng
   một prompt tiếp theo, tuân theo toàn bộ quy trình ở mục này.

## 5. Lệnh tham khảo nhanh

```bash
# build + test toàn bộ
go build ./... && go test ./... -v

# format + vet
gofmt -l . && go vet ./...

# chạy server (hiện đang dùng mock/in-memory)
go run ./cmd/api

# gửi thử webhook giả lập GitHub
curl -X POST http://localhost:8080/webhooks/github \
  -H "Content-Type: application/json" \
  -d '{"action":"opened","pull_request":{"node_id":"PR_1","number":1,"user":{"login":"alice"},"head":{"sha":"abc123"}},"repository":{"full_name":"org/app"}}'
```
