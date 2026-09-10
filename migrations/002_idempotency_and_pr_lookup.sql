-- 002: bảng khóa idempotency riêng + cột tra cứu cho pull_requests.
--
-- Vì sao tách bảng riêng thay vì INSERT thẳng vào preview_environments:
-- preview_environments.pull_request_id có ràng buộc FK tới pull_requests,
-- nhưng WebhookService gọi TryAcquireLock TRƯỚC khi lưu PullRequest — nếu
-- lock bằng cách INSERT một dòng environment thì sẽ vi phạm FK. Một bảng
-- khóa tối giản cho phép TryAcquireLock chạy đúng 1 statement
-- "INSERT ... ON CONFLICT (idempotency_key) DO NOTHING" mà không phụ thuộc
-- thứ tự ghi các bảng khác. Ngữ nghĩa "chỉ 1 tiến trình xử lý 1 key" được
-- Postgres bảo đảm bằng UNIQUE (ở đây là PRIMARY KEY).

CREATE TABLE idempotency_keys (
    idempotency_key TEXT PRIMARY KEY,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- pull_requests không có sẵn cột idempotency_key; domain sinh khóa từ
-- (repo, id, commit_hash). Dùng generated column + unique index để
-- PullRequestRepository.FindByIdempotencyKey tra cứu nhanh, khớp đúng
-- công thức trong domain.PullRequest.IdempotencyKey().
ALTER TABLE pull_requests
    ADD COLUMN idempotency_key TEXT
    GENERATED ALWAYS AS (repo || ':' || id || ':' || commit_hash) STORED;

CREATE UNIQUE INDEX idx_pr_idempotency_key ON pull_requests (idempotency_key);
