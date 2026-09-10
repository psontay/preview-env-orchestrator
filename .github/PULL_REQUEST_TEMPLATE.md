## Mô tả

Tóm tắt thay đổi và động cơ. Nếu sửa lỗi, mô tả nguyên nhân gốc.

Closes #

## Loại thay đổi

- [ ] `feat` — thêm tính năng
- [ ] `fix` — sửa lỗi
- [ ] `docs` — chỉ tài liệu
- [ ] `test` — thêm/sửa test
- [ ] `refactor` — đổi cấu trúc, không đổi hành vi
- [ ] `chore` — build / CI / dependency

## Checklist

- [ ] `gofmt -l .` không in ra file nào
- [ ] `go vet ./...` sạch
- [ ] `go build ./... && go test ./...` pass
- [ ] Integration test (`go test -tags integration ./...`) pass — nếu chạm Postgres/Docker
- [ ] Commit theo Conventional Commits
- [ ] `internal/domain/` không import package khác trong repo
- [ ] Không đổi chữ ký port công khai, hoặc đã giải thích lý do bên dưới
- [ ] Không commit secret; cấu hình mới đã thêm vào `.env.example`

## Ảnh hưởng tới 4 điểm nhấn

Đánh dấu mục bị chạm tới và mô tả test bảo vệ:

- [ ] Idempotency
- [ ] Quota
- [ ] TTL / cleanup
- [ ] Xác nhận trước thao tác phá huỷ
- [ ] Không chạm mục nào ở trên

## Cách kiểm thử

Các bước reviewer có thể làm để tự xác minh.
