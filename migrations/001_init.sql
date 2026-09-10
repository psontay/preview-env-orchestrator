-- Schema tối thiểu cho preview-env-orchestrator.
-- Idempotency được đảm bảo bằng UNIQUE constraint + INSERT ... ON CONFLICT,
-- tương đương "advisory lock ở tầng ứng dụng" mà không cần pg_advisory_lock.

CREATE TABLE pull_requests (
    id              TEXT PRIMARY KEY,          -- GitHub node_id
    repo            TEXT NOT NULL,
    number          INT NOT NULL,
    author          TEXT NOT NULL,
    commit_hash     TEXT NOT NULL,
    status          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE preview_environments (
    id                      TEXT PRIMARY KEY,
    pull_request_id         TEXT NOT NULL REFERENCES pull_requests(id),
    idempotency_key         TEXT NOT NULL UNIQUE,   -- repo:pr_id:commit_hash
    repo                    TEXT NOT NULL,           -- denormalized để CountActiveByRepo nhanh
    status                  TEXT NOT NULL,
    url                     TEXT,
    container_id            TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl_seconds             INT NOT NULL,
    marked_for_deletion_at  TIMESTAMPTZ
);

-- Idempotency: tra cứu theo key phải cực nhanh.
CREATE UNIQUE INDEX idx_env_idempotency_key ON preview_environments (idempotency_key);

-- Quota check: đếm nhanh số env đang active theo repo.
CREATE INDEX idx_env_repo_status ON preview_environments (repo, status);

-- TTL sweep: worker quét theo created_at + ttl_seconds.
CREATE INDEX idx_env_status_created_at ON preview_environments (status, created_at);

-- Gợi ý cho phần TryAcquireLock trong Go (pgx):
--
--   INSERT INTO preview_environments (id, pull_request_id, idempotency_key, repo, status, created_at, ttl_seconds)
--   VALUES ($1, $2, $3, $4, 'PROVISIONING', now(), $5)
--   ON CONFLICT (idempotency_key) DO NOTHING;
--
-- Nếu rowsAffected == 0 nghĩa là key đã tồn tại -> coi như lock không acquire được,
-- bỏ qua xử lý (duplicate webhook).
