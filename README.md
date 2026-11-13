# preview-env-orchestrator

[![CI](https://github.com/psontay/preview-env-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/psontay/preview-env-orchestrator/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Cổng điều phối môi trường preview theo Pull Request. Mỗi khi một PR được mở
hoặc có commit mới, hệ thống tự build đúng code của commit đó, dựng một môi
trường tạm bằng Docker, comment URL vào PR, rồi tự dọn khi hết hạn.

## Tính năng chính

- **Webhook GitHub** (`opened`, `synchronize`, `reopened`) có xác thực chữ ký HMAC-SHA256.
- **Idempotency**: cùng một event gửi lại không tạo thêm môi trường.
- **Quota**: giới hạn số preview đang chạy trên mỗi repository (mặc định 5).
- **TTL + grace**: hết hạn thì cảnh báo trên PR, chờ một khoảng ân hạn rồi mới xoá.
- **Huỷ xoá một lần**: người dùng bấm link trong comment để gia hạn thêm 2 phút.
- **Dọn tài nguyên**: xoá container, network, volume của preview và báo lại trên PR.
- **Quan sát**: trace OpenTelemetry gửi tới Jaeger.

Hiện stack preview được làm riêng cho app TodoList (Java): app + PostgreSQL +
Redis + RabbitMQ + Mailpit, mỗi preview một network riêng.

## Luồng hoạt động

```
GitHub PR event ──► POST /webhooks/github
                      │ kiểm tra chữ ký, idempotency, quota
                      ▼
              tải source đúng commit SHA ──► Docker build ──► chạy stack
                      │
                      ▼
              comment URL vào PR  (TTL bắt đầu khi app sẵn sàng)
                      │
          hết TTL ──► cảnh báo ──► grace ──► dọn dẹp ──► comment đã xoá
```

## Kiến trúc

Thiết kế theo DDD / ports & adapters:

```
cmd/api/                      # Composition root: config, wiring, HTTP server
internal/
  domain/                     # Entity, value object, repository ports
  application/                # Use case: webhook, provisioning, cleanup
  api/handlers/               # HTTP handlers
  infrastructure/
    postgres/                 # Repository thật + migrations
    memory/                   # Repository in-memory (dev/test)
    docker/                   # Docker SDK (và mock)
    github/                   # Comment PR, tải archive source
migrations/                   # Schema SQL
scripts/                      # Setup .env, tra trạng thái preview
docs/                         # Tài liệu chi tiết
```

## Yêu cầu

- Go 1.25+
- Docker và Docker Compose
- Node.js (cho các script trong `scripts/`)
- GitHub Personal Access Token (quyền đọc repo và comment PR)
- Một URL công khai để GitHub gọi webhook, ví dụ Cloudflare Quick Tunnel

## Cài đặt và chạy

### Cách 1: Chạy nhanh để thử (không cần Docker/GitHub thật)

Dùng bộ nhớ trong process và adapter giả lập:

```bash
git clone https://github.com/psontay/preview-env-orchestrator.git
cd preview-env-orchestrator
go run ./cmd/api
curl http://127.0.0.1:8080/healthz
```

Chạy test:

```bash
go test ./... -race
```

### Cách 2: Chạy đầy đủ bằng Docker Compose (Postgres, Docker và GitHub thật)

1. **Cấu hình `.env`**

   ```bash
   cp .env.example .env
   ```

   Điền ít nhất `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET`, `POSTGRES_PASSWORD`,
   `PREVIEW_REPOSITORY` (dạng `owner/repo`) và `PREVIEW_ALLOWED_ACTOR`.
   Hoặc để script tự thiết lập các giá trị cho chế độ thật:

   ```bash
   node scripts/setup-local.mjs --public-url https://YOUR-TUNNEL.trycloudflare.com
   ```

   Script ghi `.env` (quyền 0600), lấy `DOCKER_GID` và chuyển sang adapter thật.
   Không commit `.env`.

2. **Mở tunnel** để GitHub gọi được máy bạn:

   ```bash
   docker run --rm --network host cloudflare/cloudflared \
     tunnel --url http://127.0.0.1:18080
   ```

   Đặt URL nhận được vào `PUBLIC_BASE_URL` trong `.env`.

3. **Khởi động hệ thống**

   ```bash
   docker compose up -d --build
   curl --fail http://127.0.0.1:18080/healthz
   ```

4. **Cấu hình webhook trên GitHub** (Settings → Webhooks của repo):
   - Payload URL: `<PUBLIC_BASE_URL>/webhooks/github`
   - Content type: `application/json`
   - Secret: giá trị `GITHUB_WEBHOOK_SECRET`
   - Event: *Pull requests*

5. **Thử**: mở hoặc push commit vào một PR. Orchestrator sẽ comment trạng
   thái build và URL preview vào PR.

Dừng hệ thống (giữ dữ liệu):

```bash
docker compose down
```

### Địa chỉ hữu ích

| Dịch vụ | URL |
|---|---|
| Orchestrator | http://127.0.0.1:18080 |
| Jaeger UI | http://127.0.0.1:16686 |
| Postgres (DBeaver) | `127.0.0.1:15432` |

## Cấu hình chính

| Biến | Ý nghĩa | Mặc định |
|---|---|---|
| `STORAGE_DRIVER` | `memory` hoặc `postgres` | `memory` |
| `DOCKER_DRIVER` | `mock` hoặc `real` | `mock` |
| `GITHUB_DRIVER` | `mock` hoặc `real` | `mock` |
| `PREVIEW_TTL` | Thời gian sống của preview | 24h |
| `DELETION_GRACE_PERIOD` | Ân hạn sau cảnh báo | 10m |
| `CLEANUP_INTERVAL` | Chu kỳ quét dọn | — |
| `PREVIEW_QUOTA` | Số preview tối đa mỗi repo | 5 |
| `PUBLIC_BASE_URL` | Origin công khai, dùng cho webhook và link huỷ | — |

Danh sách đầy đủ có trong [.env.example](.env.example).

## Lệnh thường dùng

```bash
make run           # chạy API server
make build         # build binary vào ./bin/api
make test          # chạy unit test
make lint          # gofmt + go vet
make compose-up    # docker compose up -d --build
make compose-down  # gỡ stack, giữ volume
```

Xem trạng thái một preview:

```bash
node scripts/preview.mjs <PR_NUMBER> --safe
```

## Tài liệu thêm

- [Hướng dẫn chạy demo local](docs/LOCAL_DEMO.md)
- [Triển khai database](docs/DATABASE_IMPLEMENTATION.md)
- [Trạng thái kiểm thử](docs/TEST_STATUS.md)
- [Đóng góp](CONTRIBUTING.md)

## Giới hạn hiện tại

- Chạy một instance orchestrator, chưa hỗ trợ nhiều replica.
- Stack preview chuyên cho TodoList; app khác cần thiết kế thêm.
- Phạm vi demo local; chưa đánh giá cho production.
- Docker socket được mount vào container, chỉ dùng với repository và actor tin cậy.

## Giấy phép

[MIT](LICENSE)
