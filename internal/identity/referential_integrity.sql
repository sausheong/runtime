DO $$
DECLARE
    table_name TEXT;
    constraint_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['identity_users', 'service_keys', 'secrets']
    LOOP
        constraint_name := NULL;
        IF NOT EXISTS (
            SELECT 1 FROM pg_constraint
             WHERE conrelid = table_name::regclass
               AND confrelid = 'tenants'::regclass
               AND contype = 'f' AND confdeltype = 'c'
               AND conkey = ARRAY[
                   (SELECT attnum FROM pg_attribute
                     WHERE attrelid = table_name::regclass
                       AND attname = 'tenant_id')
               ]::smallint[]
        ) THEN
            SELECT conname INTO constraint_name
              FROM pg_constraint
             WHERE conrelid = table_name::regclass
               AND contype = 'f'
               AND conkey = ARRAY[
                   (SELECT attnum FROM pg_attribute
                     WHERE attrelid = table_name::regclass
                       AND attname = 'tenant_id')
               ]::smallint[]
             LIMIT 1;
            IF constraint_name IS NOT NULL THEN
                EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I',
                    table_name, constraint_name);
            END IF;
            EXECUTE format(
                'ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE',
                table_name, table_name || '_tenant_id_fkey');
        END IF;
    END LOOP;
END
$$;
