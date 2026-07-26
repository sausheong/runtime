-- Recover legacy running rows that predate durable leases by returning them to
-- the claimable pending state. Existing result rows remain available for the
-- resumed worker.
UPDATE eval_runs
   SET status = 'error', lease_owner = '', lease_until = NULL,
       finished_at = COALESCE(finished_at, now()),
       error = CASE
           WHEN error = '' THEN 'invalid legacy evaluation status: ' || status
           ELSE error
       END
 WHERE status NOT IN ('pending', 'running', 'completed', 'error');

UPDATE eval_runs
   SET status = 'pending', lease_owner = '', lease_until = NULL,
       finished_at = NULL
 WHERE status = 'running'
   AND (lease_owner = '' OR lease_until IS NULL);

UPDATE eval_runs
   SET finished_at = NULL
 WHERE status = 'running';

UPDATE eval_runs
   SET lease_owner = '', lease_until = NULL, finished_at = NULL
 WHERE status = 'pending';

UPDATE eval_runs
   SET lease_owner = '', lease_until = NULL,
       finished_at = COALESCE(finished_at, now())
 WHERE status IN ('completed', 'error');

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'eval_runs_valid_status'
           AND conrelid = 'eval_runs'::regclass
    ) THEN
        ALTER TABLE eval_runs
            ADD CONSTRAINT eval_runs_valid_status
            CHECK (status IN ('pending', 'running', 'completed', 'error'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'eval_runs_coherent_state'
           AND conrelid = 'eval_runs'::regclass
    ) THEN
        ALTER TABLE eval_runs
            ADD CONSTRAINT eval_runs_coherent_state CHECK (
                (status = 'pending'
                    AND lease_owner = '' AND lease_until IS NULL
                    AND finished_at IS NULL)
             OR (status = 'running'
                    AND lease_owner <> '' AND lease_until IS NOT NULL
                    AND finished_at IS NULL)
             OR (status IN ('completed', 'error')
                    AND lease_owner = '' AND lease_until IS NULL
                    AND finished_at IS NOT NULL)
            );
    END IF;
END
$$;
