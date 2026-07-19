package quota

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// MemStore is an in-memory QuotaStore for hermetic tests. Mirrors Store's
// validation and generation semantics.
type MemStore struct {
	mu   sync.RWMutex
	rows map[string]Rule // "tenant\x00upstream" -> rule
	gen  uint64
}

func NewMemStore() *MemStore { return &MemStore{rows: map[string]Rule{}} }

func key(tenant, upstream string) string { return tenant + "\x00" + upstream }

func (m *MemStore) Insert(_ context.Context, r Rule) error {
	if err := validRule(r); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[key(r.Tenant, r.Upstream)]; ok {
		return fmt.Errorf("quota %s/%s already exists", r.Tenant, r.Upstream)
	}
	m.rows[key(r.Tenant, r.Upstream)] = r
	m.gen++
	return nil
}

func (m *MemStore) List(_ context.Context, tenant string) ([]Rule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Rule
	for _, r := range m.rows {
		if tenant == "" || r.Tenant == tenant {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Upstream < out[j].Upstream
	})
	return out, nil
}

func (m *MemStore) Delete(_ context.Context, tenant, upstream string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[key(tenant, upstream)]; !ok {
		return false, nil
	}
	delete(m.rows, key(tenant, upstream))
	m.gen++
	return true, nil
}

func (m *MemStore) Rules(_ context.Context) ([]Rule, uint64, error) {
	rows, _ := m.List(context.Background(), "")
	m.mu.RLock()
	gen := m.gen
	m.mu.RUnlock()
	return rows, gen, nil
}
