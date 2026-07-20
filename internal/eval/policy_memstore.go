package eval

import (
	"context"
	"sort"
	"sync"
	"time"
)

// PolicyMemStore is an in-memory PolicyStoreAPI for hermetic tests. Mirrors
// PolicyStore's validation and generation semantics.
type PolicyMemStore struct {
	mu       sync.RWMutex
	policies map[string]Policy // "tenant\x00agent_id" -> policy
	gen      uint64
}

func NewPolicyMemStore() *PolicyMemStore {
	return &PolicyMemStore{policies: map[string]Policy{}}
}

func (m *PolicyMemStore) PutPolicy(_ context.Context, p Policy) error {
	if err := ValidatePolicy(p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	cp := make([]Criterion, len(p.Criteria))
	copy(cp, p.Criteria)
	p.Criteria = cp
	m.policies[key(p.Tenant, p.AgentID)] = p
	m.gen++
	return nil
}

func (m *PolicyMemStore) GetPolicy(_ context.Context, tenant, agentID string) (Policy, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.policies[key(tenant, agentID)]
	if !ok {
		return Policy{}, false, nil
	}
	cp := make([]Criterion, len(p.Criteria))
	copy(cp, p.Criteria)
	p.Criteria = cp
	return p, true, nil
}

func (m *PolicyMemStore) ListPolicies(_ context.Context, tenant string) ([]Policy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Policy
	for _, p := range m.policies {
		if tenant == "" || p.Tenant == tenant {
			cp := make([]Criterion, len(p.Criteria))
			copy(cp, p.Criteria)
			p.Criteria = cp
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].AgentID < out[j].AgentID
	})
	return out, nil
}

func (m *PolicyMemStore) DeletePolicy(_ context.Context, tenant, agentID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.policies[key(tenant, agentID)]; !ok {
		return false, nil
	}
	delete(m.policies, key(tenant, agentID))
	m.gen++
	return true, nil
}

var _ PolicyStoreAPI = (*PolicyMemStore)(nil)
