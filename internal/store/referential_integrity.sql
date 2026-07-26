DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'session_events'::regclass
           AND confrelid = 'sessions'::regclass
           AND contype = 'f' AND confdeltype = 'c'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'session_events'::regclass
                   AND attname = 'session_id')
           ]::smallint[]
    ) THEN
        SELECT conname INTO constraint_name
          FROM pg_constraint
         WHERE conrelid = 'session_events'::regclass
           AND contype = 'f'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'session_events'::regclass
                   AND attname = 'session_id')
           ]::smallint[]
         LIMIT 1;
        IF constraint_name IS NOT NULL THEN
            EXECUTE format('ALTER TABLE session_events DROP CONSTRAINT %I', constraint_name);
        END IF;
        ALTER TABLE session_events
            ADD CONSTRAINT session_events_session_id_fkey
            FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;
    END IF;

    constraint_name := NULL;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'session_transcripts'::regclass
           AND confrelid = 'sessions'::regclass
           AND contype = 'f' AND confdeltype = 'c'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'session_transcripts'::regclass
                   AND attname = 'session_id')
           ]::smallint[]
    ) THEN
        SELECT conname INTO constraint_name
          FROM pg_constraint
         WHERE conrelid = 'session_transcripts'::regclass
           AND contype = 'f'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'session_transcripts'::regclass
                   AND attname = 'session_id')
           ]::smallint[]
         LIMIT 1;
        IF constraint_name IS NOT NULL THEN
            EXECUTE format('ALTER TABLE session_transcripts DROP CONSTRAINT %I', constraint_name);
        END IF;
        ALTER TABLE session_transcripts
            ADD CONSTRAINT session_transcripts_session_id_fkey
            FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;
    END IF;

    constraint_name := NULL;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'online_eval_results'::regclass
           AND confrelid = 'sessions'::regclass
           AND contype = 'f' AND confdeltype = 'c'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'online_eval_results'::regclass
                   AND attname = 'session_id')
           ]::smallint[]
    ) THEN
        SELECT conname INTO constraint_name
          FROM pg_constraint
         WHERE conrelid = 'online_eval_results'::regclass
           AND contype = 'f'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'online_eval_results'::regclass
                   AND attname = 'session_id')
           ]::smallint[]
         LIMIT 1;
        IF constraint_name IS NOT NULL THEN
            EXECUTE format('ALTER TABLE online_eval_results DROP CONSTRAINT %I', constraint_name);
        END IF;
        ALTER TABLE online_eval_results
            ADD CONSTRAINT online_eval_results_session_id_fkey
            FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;
    END IF;
END
$$;
