-- Match domain.EnvStatus. This checks stored values, not transition ordering.
-- Validate existing rows as well; invalid history must abort the transaction.
SET LOCAL lock_timeout = '5s';
ALTER TABLE preview_environments
    ADD CONSTRAINT preview_environments_status_check
    CHECK (status IN ('PROVISIONING', 'RUNNING', 'MARK_FOR_DELETION',
                      'DESTROYING', 'DESTROYED', 'FAILED'));

-- No ttl_seconds > 0 constraint: the domain permits immediate expiry (TTL=0),
-- and positive subsecond durations currently persist as zero whole seconds.
