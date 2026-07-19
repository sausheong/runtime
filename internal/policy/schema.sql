CREATE TABLE IF NOT EXISTS gateway_policies (
    tenant     TEXT NOT NULL,
    name       TEXT NOT NULL,
    cedar_text TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, name)
);
