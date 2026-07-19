CREATE TABLE IF NOT EXISTS gateway_quotas (
    tenant       TEXT NOT NULL,          -- '*' = all tenants
    upstream     TEXT NOT NULL,          -- '*' = all upstreams
    rate_per_min INT  NOT NULL CHECK (rate_per_min > 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, upstream)
);
