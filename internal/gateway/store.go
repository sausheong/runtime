package gateway

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"

	"github.com/lib/pq" // pq.Array for TEXT[]; pgx stdlib is the driver

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/store"
)

//go:embed upstream_schema.sql
var upstreamSchemaSQL string

// UpstreamRow is one tenant-registered gateway upstream (http/openapi only).
type UpstreamRow struct {
	ID         string
	TenantID   string
	Name       string
	Transport  string // "http" | "openapi"
	URL        string
	OpenAPI    string
	BaseURL    string
	Operations []string
	CredSecret string
	CredHeader string
}

// ToConfig maps a stored row onto config.GatewayServer. The owning tenant is
// placed in Tenants so the Manager's existing visibility filter scopes the
// upstream to exactly that tenant.
func (r UpstreamRow) ToConfig() config.GatewayServer {
	return config.GatewayServer{
		Name:       r.Name,
		URL:        r.URL,
		OpenAPI:    r.OpenAPI,
		BaseURL:    r.BaseURL,
		Operations: r.Operations,
		Tenants:    []string{r.TenantID},
		CredSecret: r.CredSecret,
		CredHeader: r.CredHeader,
	}
}

// UpstreamStore persists tenant-registered upstreams in Postgres.
type UpstreamStore struct{ db *sql.DB }

// NewUpstreamStore applies the gateway_upstreams DDL (under the shared lock) and
// returns a store. The tenants table (identity schema) must already exist (FK).
func NewUpstreamStore(ctx context.Context, db *sql.DB) (*UpstreamStore, error) {
	if err := store.ApplyDDLLocked(ctx, db, upstreamSchemaSQL); err != nil {
		return nil, err
	}
	return &UpstreamStore{db: db}, nil
}

func (s *UpstreamStore) InsertUpstream(ctx context.Context, r UpstreamRow) error {
	// pq.Array(nil) emits SQL NULL, which overrides the column's NOT NULL DEFAULT
	// '{}' and fails the constraint — http upstreams legitimately have no
	// operations, so coerce nil to a non-nil empty slice (stored as '{}').
	ops := r.Operations
	if ops == nil {
		ops = []string{}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_upstreams
		 (id, tenant_id, name, transport, url, openapi, base_url, operations, cred_secret, cred_header)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		r.ID, r.TenantID, r.Name, r.Transport, r.URL, r.OpenAPI, r.BaseURL,
		pq.Array(ops), r.CredSecret, r.CredHeader)
	return err
}

// ListUpstreams returns rows for one tenant, or all rows when tenant=="".
func (s *UpstreamStore) ListUpstreams(ctx context.Context, tenant string) ([]UpstreamRow, error) {
	q := `SELECT id, tenant_id, name, transport, url, openapi, base_url, operations, cred_secret, cred_header
	      FROM gateway_upstreams`
	args := []any{}
	if tenant != "" {
		q += ` WHERE tenant_id=$1`
		args = append(args, tenant)
	}
	q += ` ORDER BY tenant_id, name`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamRow
	for rows.Next() {
		var r UpstreamRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Transport, &r.URL,
			&r.OpenAPI, &r.BaseURL, pq.Array(&r.Operations), &r.CredSecret, &r.CredHeader); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetUpstream returns one row by id (ok=false when absent).
func (s *UpstreamStore) GetUpstream(ctx context.Context, id string) (UpstreamRow, bool, error) {
	var r UpstreamRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, transport, url, openapi, base_url, operations, cred_secret, cred_header
		 FROM gateway_upstreams WHERE id=$1`, id).
		Scan(&r.ID, &r.TenantID, &r.Name, &r.Transport, &r.URL,
			&r.OpenAPI, &r.BaseURL, pq.Array(&r.Operations), &r.CredSecret, &r.CredHeader)
	if errors.Is(err, sql.ErrNoRows) {
		return UpstreamRow{}, false, nil
	}
	if err != nil {
		return UpstreamRow{}, false, err
	}
	return r, true, nil
}

// DeleteUpstream removes a row scoped to its owning tenant (idempotent).
func (s *UpstreamStore) DeleteUpstream(ctx context.Context, tenant, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM gateway_upstreams WHERE id=$1 AND tenant_id=$2`, id, tenant)
	return err
}
