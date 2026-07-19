package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// MemStore is an in-memory PolicyStore for hermetic tests. It mirrors Store's
// validation and generation semantics without Postgres.
type MemStore struct {
	mu   sync.RWMutex
	rows map[string]map[string]Row // tenant -> name -> row
	gen  uint64
}

// NewMemStore returns an empty in-memory policy store.
func NewMemStore() *MemStore {
	return &MemStore{rows: map[string]map[string]Row{}}
}

// Insert validates and stores one policy; rejects empty keys, bad/multi-policy
// text, and duplicate (tenant,name).
func (m *MemStore) Insert(_ context.Context, r Row) error {
	if r.Tenant == "" || r.Name == "" {
		return errors.New("policy: tenant and name are required")
	}
	if err := validateOne(r.CedarText); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[r.Tenant][r.Name]; ok {
		return fmt.Errorf("policy %q already exists", r.Name)
	}
	if m.rows[r.Tenant] == nil {
		m.rows[r.Tenant] = map[string]Row{}
	}
	m.rows[r.Tenant][r.Name] = r
	m.gen++
	return nil
}

// List returns a tenant's policies (or all when tenant==""), name-ordered.
func (m *MemStore) List(_ context.Context, tenant string) ([]Row, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Row
	collect := func(byName map[string]Row) {
		for _, r := range byName {
			out = append(out, r)
		}
	}
	if tenant != "" {
		collect(m.rows[tenant])
	} else {
		for _, byName := range m.rows {
			collect(byName)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Delete removes one policy; returns false when absent.
func (m *MemStore) Delete(_ context.Context, tenant, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[tenant][name]; !ok {
		return false, nil
	}
	delete(m.rows[tenant], name)
	m.gen++
	return true, nil
}

// PoliciesFor returns NamedPolicy entries (id tenant/<name>) + generation.
func (m *MemStore) PoliciesFor(ctx context.Context, tenant string) ([]NamedPolicy, uint64, error) {
	rows, err := m.List(ctx, tenant)
	if err != nil {
		return nil, 0, err
	}
	m.mu.RLock()
	gen := m.gen
	m.mu.RUnlock()
	out := make([]NamedPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, NamedPolicy{ID: "tenant/" + r.Name, Source: r.CedarText})
	}
	return out, gen, nil
}
