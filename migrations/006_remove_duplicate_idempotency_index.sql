-- 001 creates both UNIQUE(idempotency_key) and an equivalent standalone index.
-- Retain the constraint-backed index; refuse unexpected catalog definitions.
SET LOCAL lock_timeout = '5s';
LOCK TABLE preview_environments IN ACCESS EXCLUSIVE MODE;

DO $migration$
DECLARE
    env_table REGCLASS := 'preview_environments'::regclass;
    redundant_index REGCLASS := to_regclass(format('%I.idx_env_idempotency_key', current_schema()));
    retained_index OID;
BEGIN
    SELECT c.conindid INTO retained_index
    FROM pg_constraint c
    JOIN pg_index i ON i.indexrelid = c.conindid
    JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attname = 'idempotency_key'
    WHERE c.conrelid = env_table AND c.contype = 'u'
      AND c.conkey = ARRAY[a.attnum]
      AND i.indisunique AND i.indisvalid AND i.indisready AND i.indislive
      AND i.indimmediate AND i.indnkeyatts = 1 AND i.indnatts = 1
      AND i.indpred IS NULL AND i.indexprs IS NULL;

    IF retained_index IS NULL OR retained_index = redundant_index THEN
        RAISE EXCEPTION '006: a separate valid UNIQUE constraint on idempotency_key is required';
    END IF;

    -- An already removed standalone index is safe; UNIQUE must still exist.
    IF redundant_index IS NULL THEN
        RETURN;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_index d
        JOIN pg_index k ON k.indexrelid = retained_index
        JOIN pg_class dc ON dc.oid = d.indexrelid
        JOIN pg_class kc ON kc.oid = k.indexrelid
        WHERE d.indexrelid = redundant_index AND d.indrelid = env_table
          AND d.indisunique AND d.indisvalid AND d.indisready AND d.indislive
          AND d.indimmediate AND NOT d.indisreplident AND NOT d.indisclustered
          AND d.indpred IS NULL AND d.indexprs IS NULL
          AND d.indnkeyatts = k.indnkeyatts AND d.indnatts = k.indnatts
          AND d.indkey = k.indkey AND d.indclass = k.indclass
          AND d.indcollation = k.indcollation AND d.indoption = k.indoption
          AND d.indnullsnotdistinct = k.indnullsnotdistinct
          AND dc.relam = kc.relam
          AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = d.indexrelid)
    ) THEN
        RAISE EXCEPTION '006: idx_env_idempotency_key is not a redundant standalone index; review catalog';
    END IF;

    -- No CASCADE: dependencies must stop the upgrade instead of being removed.
    EXECUTE format('DROP INDEX %I.idx_env_idempotency_key', current_schema());
END;
$migration$;
