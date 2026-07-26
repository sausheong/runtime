DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'eval_results'::regclass
           AND confrelid = 'eval_runs'::regclass
           AND contype = 'f' AND confdeltype = 'c'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'eval_results'::regclass
                   AND attname = 'run_id')
           ]::smallint[]
    ) THEN
        SELECT conname INTO constraint_name
          FROM pg_constraint
         WHERE conrelid = 'eval_results'::regclass
           AND contype = 'f'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'eval_results'::regclass
                   AND attname = 'run_id')
           ]::smallint[]
         LIMIT 1;
        IF constraint_name IS NOT NULL THEN
            EXECUTE format('ALTER TABLE eval_results DROP CONSTRAINT %I', constraint_name);
        END IF;
        ALTER TABLE eval_results
            ADD CONSTRAINT eval_results_run_id_fkey
            FOREIGN KEY (run_id) REFERENCES eval_runs(run_id) ON DELETE CASCADE;
    END IF;
END
$$;
