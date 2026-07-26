DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'gateway_upstreams'::regclass
           AND confrelid = 'tenants'::regclass
           AND contype = 'f' AND confdeltype = 'c'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'gateway_upstreams'::regclass
                   AND attname = 'tenant_id')
           ]::smallint[]
    ) THEN
        SELECT conname INTO constraint_name
          FROM pg_constraint
         WHERE conrelid = 'gateway_upstreams'::regclass
           AND contype = 'f'
           AND conkey = ARRAY[
               (SELECT attnum FROM pg_attribute
                 WHERE attrelid = 'gateway_upstreams'::regclass
                   AND attname = 'tenant_id')
           ]::smallint[]
         LIMIT 1;
        IF constraint_name IS NOT NULL THEN
            EXECUTE format('ALTER TABLE gateway_upstreams DROP CONSTRAINT %I',
                constraint_name);
        END IF;
        ALTER TABLE gateway_upstreams
            ADD CONSTRAINT gateway_upstreams_tenant_id_fkey
            FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE;
    END IF;
END
$$;
