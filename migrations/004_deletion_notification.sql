-- Persist successful delivery so restarts do not resend every warning.
-- Existing pending deletions receive a warning and a fresh grace period.
ALTER TABLE preview_environments ADD COLUMN deletion_notified_at TIMESTAMPTZ;
