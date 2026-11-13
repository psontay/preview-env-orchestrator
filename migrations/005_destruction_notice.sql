-- Completion and its pending notice are saved together. Historical DESTROYED
-- rows stay quiet: only future cleanup operations enqueue completion notices.
ALTER TABLE preview_environments
    ADD COLUMN destruction_notice_pending BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX idx_env_pending_destruction_notice ON preview_environments (id)
    WHERE status = 'DESTROYED' AND destruction_notice_pending;
