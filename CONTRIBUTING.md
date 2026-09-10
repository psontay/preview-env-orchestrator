# Hướng dẫn đóng góp

Cảm ơn bạn đã quan tâm tới **preview-env-orchestrator**. Tài liệu này mô tả
quy trình đóng góp: từ lúc fork repo tới lúc pull request được merge.

## 1. Chuẩn bị môi trường

- Go **1.25** trở lên (`go version`).
- Docker + Docker Compose (chỉ cần khi chạy integration test hoặc
  `make compose-up`).
- `gofmt`, `go vet` (đi kèm bản phân phối Go).

```bash
git clone https://github.com/<your-username>/preview-env-orchestrator
cd preview-env-orchestrator
cp .env.example .env      # điền giá trị nếu chạy adapter thật
go build ./... && go test ./...
```

## 2. Quy trình fork / branch

1. **Fork** repo về tài khoản của bạn, rồi clone bản fork.
2. Thêm remote `upstream` để đồng bộ về sau:
   ```bash
   git remote add upstream https://github.com/psontay/preview-env-orchestrator
   ```
3. Luôn tạo nhánh mới từ `master` đã cập nhật:
   ```bash
   git checkout master
   git pull upstream master
   git checkout -b <loại>/<mô-tả-ngắn>
   ```
   Ví dụ tên nhánh: `feat/postgres-adapter`, `fix/webhook-signature-timing`,
   `docs/architecture-diagram`.
4. Một nhánh / một pull request chỉ giải quyết **một** vấn đề.

## 3. Quy ước commit — Conventional Commits

Mọi commit phải theo cú pháp:

```
<type>(<scope tùy chọn>): <mô tả ngắn, không viết hoa đầu, không chấm cuối>
```

| `type`   | Dùng khi |
|----------|----------|
| `feat`   | Thêm tính năng cho người dùng cuối |
| `fix`    | Sửa lỗi |
| `docs`   | Chỉ thay đổi tài liệu |
| `test`   | Thêm / sửa test, không đụng code sản phẩm |
| `refactor` | Đổi cấu trúc code, không đổi hành vi |
| `chore`  | Build, CI, cấu hình, dependency |

Ví dụ hợp lệ:

```
feat(postgres): TryAcquireLock dùng INSERT ... ON CONFLICT DO NOTHING
fix(cleanup): không destroy env vừa được CancelDeletion
docs(readme): thêm sơ đồ Mermaid port/adapter
```

Lịch sử commit sạch được xem là một phần của sản phẩm — hãy squash các
commit "wip", "fixup" trước khi mở PR.

## 4. Trước khi mở pull request

Chạy đủ và bảo đảm **pass**:

```bash
gofmt -l .            # phải rỗng
go vet ./...
go build ./... && go test ./...
```

Nếu thay đổi liên quan Postgres/Docker, chạy thêm integration test:

```bash
go test -tags integration ./...
```

Nguyên tắc kiến trúc bắt buộc giữ nguyên (xem `CLAUDE.md` mục 3):

- `internal/domain/` **không** import package nào khác trong repo.
- Không đổi chữ ký các port (`domain.PullRequestRepository`,
  `domain.EnvironmentRepository`, `application.DockerOrchestrationService`,
  `application.GitHubNotifier`) trừ khi thật sự cần.
- Không làm suy yếu 4 điểm nhấn: **Idempotency, Quota, TTL,
  Destructive-safety**. Thay đổi chạm tới chúng phải kèm test.
- Không commit secret. Cấu hình đọc từ biến môi trường; `.env` nằm trong
  `.gitignore`.

## 5. Mở pull request

1. Push nhánh lên bản fork, mở PR nhắm vào `master` của `upstream`.
2. Điền đầy đủ mẫu `.github/PULL_REQUEST_TEMPLATE.md`.
3. Liên kết issue liên quan bằng `Closes #<số>`.
4. CI phải xanh. Reviewer có thể yêu cầu chỉnh sửa — cập nhật cùng nhánh,
   không mở PR mới.

## 6. Báo lỗi & đề xuất tính năng

Dùng template tương ứng trong `.github/ISSUE_TEMPLATE/`. Trước khi tạo
issue mới, tìm xem đã có issue trùng chưa.
