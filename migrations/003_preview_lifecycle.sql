-- Keep the original creation time while allowing one two-minute extension.
ALTER TABLE preview_environments ADD COLUMN expires_at TIMESTAMPTZ;
ALTER TABLE preview_environments ADD COLUMN deletion_cancel_count INT NOT NULL DEFAULT 0
    CHECK (deletion_cancel_count BETWEEN 0 AND 1);
ALTER TABLE preview_environments ADD COLUMN commit_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE preview_environments ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
-- A stable cleanup handle survives partial deletion of the app container.
ALTER TABLE preview_environments ADD COLUMN resource_group TEXT NOT NULL DEFAULT '';
