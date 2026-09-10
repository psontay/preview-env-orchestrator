---
name: Báo lỗi (Bug report)
about: Báo cáo một hành vi sai để giúp dự án tốt hơn
title: "[BUG] "
labels: bug
assignees: ''
---

## Mô tả lỗi

Mô tả ngắn gọn, rõ ràng lỗi là gì.

## Các bước tái hiện

1. Cấu hình biến môi trường '...'
2. Chạy lệnh '...'
3. Gửi request '...'
4. Thấy lỗi

## Hành vi mong đợi

Bạn kỳ vọng điều gì xảy ra thay vì lỗi hiện tại.

## Log / thông báo lỗi

```
Dán log, stack trace hoặc output liên quan vào đây.
```

## Môi trường

- OS:
- Go version (`go version`):
- Commit / tag đang dùng:
- Adapter đang chạy: STORAGE_DRIVER=`memory|postgres`, DOCKER_DRIVER=`mock|real`, GITHUB_DRIVER=`mock|real`

## Điểm nhấn bị ảnh hưởng

- [ ] Idempotency
- [ ] Quota
- [ ] TTL / cleanup
- [ ] Xác nhận trước thao tác phá huỷ (destructive safety)
- [ ] Không liên quan các mục trên

## Thông tin thêm

Bất kỳ ngữ cảnh nào khác về vấn đề.
