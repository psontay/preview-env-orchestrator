# preview-env-orchestrator

[![CI](https://github.com/psontay/preview-env-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/psontay/preview-env-orchestrator/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Cổng điều phối môi trường preview theo Pull Request, thiết kế theo
Domain-Driven Design. Nhận webhook GitHub → kiểm tra idempotency & quota →
dựng môi trường (Docker) bất đồng bộ → comment link vào PR → tự động dọn
dẹp theo TTL với cơ chế "destructive safety" (đánh dấu trước, xoá sau).

Toàn bộ code trong repo này **build và chạy được ngay** (`go build ./...`
và `go test ./...` đều pass) bằng các adapter in-memory + Docker giả lập,
để bạn thấy được luồng nghiệp vụ chạy thật trước khi cắm Postgres/Docker
SDK thật vào.

## Kiến trúc (DDD, 3 lớp)

```
cmd/api/main.go              # Composition Root — nơi DUY NHẤT wiring implementation cụ thể
internal/
  domain/                    # Entities, Value Objects, Repository interfaces (KHÔNG phụ thuộc gì bên ngoài)
    pullrequest.go
    environment.go
    valueobjects.go          # Quota, TTL
    repository.go            # PullRequestRepository, EnvironmentRepository (ports)
  application/                # Domain Services điều phối use case, phụ thuộc domain + port
    ports.go                  # DockerOrchestrationService, GitHubNotifier (ports)
    webhook_service.go        # Use case chính: nhận PR event -> idempotency -> quota -> provision
    cleanup_service.go        # Background worker: TTL sweep + destructive safety
  infrastructure/             # Adapter — implement các port ở trên
    memory/                   # In-memory (demo/test): PR repo, Env repo, GitHub notifier giả
    docker/                   # MockService (giả lập docker); xem docs/ để thay bằng Docker SDK thật
  api/handlers/                # HTTP <-> domain translation, KHÔNG chứa business logic
docs/                          # Hướng dẫn tích hợp Docker SDK thật, Postgres, OpenTelemetry
migrations/001_init.sql        # Schema Postgres khớp với domain model
```

**Nguyên tắc phân bổ trách nhiệm (SOLID/DDD):**
- `domain/` không import bất kỳ package nào khác trong repo — nó là lõi bất biến.
- `application/` chỉ phụ thuộc `domain` + các port do chính nó định nghĩa
  (`DockerOrchestrationService`, `GitHubNotifier`) — không biết Docker SDK
  hay Postgres là gì.
- `infrastructure/` implement các port đó — đây là nơi duy nhất "bẩn" với
  chi tiết công nghệ cụ thể.
- Idempotency là trách nhiệm của `EnvironmentRepository.TryAcquireLock`
  (một hành vi lưu trữ), còn Quota là trách nhiệm của `domain.Quota`
  (một business rule thuần) — được `WebhookService` phối hợp gọi theo
  đúng thứ tự, không lẫn logic vào nhau.

### Sơ đồ lớp — Aggregate, Value Object, Service, Port/Adapter

```mermaid
classDiagram
    direction LR

    class PullRequest {
        <<Aggregate Root>>
        +string ID
        +string Repo
        +int Number
        +string CommitHash
        +PRStatus Status
        +IdempotencyKey() string
    }

    class PreviewEnvironment {
        <<Aggregate Root>>
        +string ID
        +string PullRequestID
        +EnvStatus Status
        +string URL
        +string ContainerID
        +TTL TTL
        +time MarkedForDeletionAt
        +IsExpired(now) bool
        +MarkForDeletion(now)
        +CancelDeletion()
        +ReadyToDestroy(now, grace) bool
    }

    class Quota {
        <<Value Object>>
        +int MaxEnvironments
        +IsExceeded(current) bool
    }

    class TTL {
        <<Value Object>>
        +Duration Duration
        +ExpiresAt(from) time
    }

    class PullRequestRepository {
        <<port>>
        +FindByIdempotencyKey(key)
        +Save(pr)
    }
    class EnvironmentRepository {
        <<port>>
        +TryAcquireLock(key) bool
        +CountActiveByRepo(repo) int
        +FindExpired(now)
        +FindMarkedForDeletion(now)
        +Save(env)
    }
    class DockerOrchestrationService {
        <<port>>
        +Provision(req) ProvisionResult
        +Destroy(containerID)
    }
    class GitHubNotifier {
        <<port>>
        +CommentOnPR(repo, num, msg)
    }

    class WebhookService {
        <<Domain Service>>
        +HandlePullRequestEvent(evt)
    }
    class CleanupService {
        <<Domain Service>>
        +SweepOnce(now)
    }

    class PostgresEnvRepository {
        <<adapter>>
    }
    class MemoryEnvRepository {
        <<adapter / test double>>
    }
    class DockerRealService {
        <<adapter>>
    }
    class DockerMockService {
        <<adapter / test double>>
    }
    class GitHubRESTNotifier {
        <<adapter>>
    }

    PreviewEnvironment *-- TTL : composition
    PullRequest ..> PreviewEnvironment : sinh ra (idempotency key)
    WebhookService --> Quota : kiểm tra business rule
    WebhookService ..> PullRequestRepository
    WebhookService ..> EnvironmentRepository
    WebhookService ..> DockerOrchestrationService
    WebhookService ..> GitHubNotifier
    CleanupService ..> EnvironmentRepository
    CleanupService ..> DockerOrchestrationService
    CleanupService ..> GitHubNotifier

    EnvironmentRepository <|.. PostgresEnvRepository
    EnvironmentRepository <|.. MemoryEnvRepository
    DockerOrchestrationService <|.. DockerRealService
    DockerOrchestrationService <|.. DockerMockService
    GitHubNotifier <|.. GitHubRESTNotifier
```

### Luồng nghiệp vụ — từ webhook tới cleanup

```mermaid
sequenceDiagram
    autonumber
    participant GH as GitHub
    participant H as webhook_handler
    participant WS as WebhookService
    participant ER as EnvironmentRepository
    participant Q as domain.Quota
    participant DK as DockerOrchestrationService
    participant GN as GitHubNotifier
    participant CS as CleanupService (worker)

    GH->>H: POST /webhooks/github (+ chữ ký HMAC-SHA256)
    H->>H: VerifyGitHubSignature
    H->>WS: HandlePullRequestEvent
    WS->>ER: TryAcquireLock(idempotencyKey)
    alt key đã tồn tại
        ER-->>WS: false → dừng (idempotent, không tạo trùng)
    else lock được cấp
        WS->>ER: CountActiveByRepo(repo)
        WS->>Q: IsExceeded(current)?
        alt vượt quota
            Q-->>WS: true → ErrQuotaExceeded
        else còn quota
            WS->>DK: Provision(image, seedSQL) [async]
            DK-->>WS: ProvisionResult{containerID, URL}
            WS->>ER: Save(env RUNNING)
            WS->>GN: CommentOnPR("URL sẵn sàng + link Hủy lệnh xoá")
        end
    end

    loop mỗi N phút
        CS->>ER: FindExpired(now)
        CS->>ER: Save(env MARK_FOR_DELETION)  %% đánh dấu, chưa xoá
        CS->>GN: CommentOnPR("sẽ xoá sau grace period — Hủy tại /cancel-deletion")
        CS->>ER: FindMarkedForDeletion(now)
        alt ReadyToDestroy(now, grace)
            CS->>DK: Destroy(containerID)
            CS->>ER: Save(env DESTROYED)
        else vừa CancelDeletion()
            CS-->>CS: bỏ qua, env đã quay lại RUNNING
        end
    end
```

## Chạy thử

```bash
go run ./cmd/api
# server lắng nghe tại :8080

curl -X POST http://localhost:8080/webhooks/github \
  -H "Content-Type: application/json" \
  -d '{
    "action": "opened",
    "pull_request": {
      "node_id": "PR_kwDOtest",
      "number": 7,
      "user": {"login": "alice"},
      "head": {"sha": "deadbeef"}
    },
    "repository": {"full_name": "org/app"}
  }'
# -> 202 Accepted, log server sẽ in comment "Môi trường preview đã sẵn sàng..."
```

## Test

```bash
go test ./... -v
```

Bộ test (`internal/application/webhook_service_test.go`) đã verify:
1. Gửi trùng webhook cho cùng 1 commit → chỉ tạo 1 environment (idempotency).
2. Vượt quota → trả về `ErrQuotaExceeded`.
3. TTL sweep → environment hết hạn được đánh dấu `MARK_FOR_DELETION` trước,
   chỉ bị huỷ thật sau khi qua grace period (destructive safety).

## Lên production: còn thiếu gì?

Repo này cố tình dùng adapter in-memory/mock để bạn chạy được ngay không
cần hạ tầng. Trước khi deploy thật, xem 3 tài liệu trong `docs/`:

| Việc cần làm | Tài liệu |
|---|---|
| Thay `MockService` bằng Docker Engine SDK thật (không dùng CLI) | `docs/DOCKER_SDK_INTEGRATION.md` |
| Thay in-memory repo bằng Postgres (dùng schema có sẵn) | `migrations/001_init.sql` |
| Gắn OpenTelemetry trace 3 bước: nhận webhook → ghi DB → gọi Docker | `docs/OPENTELEMETRY.md` |
| Gọi GitHub REST/GraphQL API thật thay vì in ra console | Dùng `google/go-github` hoặc gọi REST trực tiếp trong `GitHubNotifier.CommentOnPR` |

Tất cả các thay đổi trên chỉ cần sửa ở `internal/infrastructure/` và dây
lại ở `cmd/api/main.go` — không cần đụng vào `domain/` hay `application/`.

## Đóng góp

Dự án hoan nghênh đóng góp. Quy trình fork → nhánh → Conventional Commits →
pull request, cùng các lệnh kiểm tra bắt buộc trước khi mở PR, được mô tả
đầy đủ trong [CONTRIBUTING.md](CONTRIBUTING.md).

- Báo lỗi: dùng template **Bug report** trong `.github/ISSUE_TEMPLATE/`.
- Đề xuất tính năng: dùng template **Feature request**.
- Mọi PR cần điền [PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md)
  và giữ nguyên 4 điểm nhấn: Idempotency, Quota, TTL, Destructive-safety.

## License

Phát hành theo giấy phép [MIT](LICENSE).
