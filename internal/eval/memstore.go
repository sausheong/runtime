package eval

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemStore is an in-memory EvalStore for hermetic tests. Mirrors Store's
// validation and generation semantics.
type MemStore struct {
	mu      sync.RWMutex
	sets    map[string]Set      // "tenant\x00name" -> set
	runs    map[string]Run      // run_id -> run
	results map[string][]Result // run_id -> results
	gen     uint64
}

func NewMemStore() *MemStore {
	return &MemStore{
		sets:    map[string]Set{},
		runs:    map[string]Run{},
		results: map[string][]Result{},
	}
}

func key(tenant, name string) string { return tenant + "\x00" + name }

func (m *MemStore) PutSet(_ context.Context, s Set) error {
	if err := ValidateSet(s.Name, s.Cases); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	cp := make([]Case, len(s.Cases))
	copy(cp, s.Cases)
	s.Cases = cp
	m.sets[key(s.Tenant, s.Name)] = s
	m.gen++
	return nil
}

func (m *MemStore) GetSet(_ context.Context, tenant, name string) (Set, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sets[key(tenant, name)]
	if !ok {
		return Set{}, false, nil
	}
	cp := make([]Case, len(s.Cases))
	copy(cp, s.Cases)
	s.Cases = cp
	return s, true, nil
}

func (m *MemStore) ListSets(_ context.Context, tenant string) ([]Set, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Set
	for _, s := range m.sets {
		if tenant == "" || s.Tenant == tenant {
			cp := make([]Case, len(s.Cases))
			copy(cp, s.Cases)
			s.Cases = cp
			out = append(out, s)
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

func (m *MemStore) DeleteSet(_ context.Context, tenant, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sets[key(tenant, name)]; !ok {
		return false, nil
	}
	delete(m.sets, key(tenant, name))
	m.gen++
	return true, nil
}

func (m *MemStore) CreateRun(_ context.Context, r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	m.runs[r.RunID] = r
	m.gen++
	return nil
}

func (m *MemStore) GetRun(_ context.Context, runID string) (Run, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[runID]
	if !ok {
		return Run{}, false, nil
	}
	return r, true, nil
}

func (m *MemStore) ListRuns(_ context.Context, tenant string) ([]Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Run
	for _, r := range m.runs {
		if tenant == "" || r.Tenant == tenant {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].RunID < out[j].RunID
	})
	return out, nil
}

func (m *MemStore) SetRunStatus(_ context.Context, runID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return nil
	}
	r.Status = status
	m.runs[runID] = r
	m.gen++
	return nil
}

func (m *MemStore) FinishRun(_ context.Context, runID, status string, total, passed, failed int, score float64, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return nil
	}
	now := time.Now().UTC()
	r.Status = status
	r.Total = total
	r.Passed = passed
	r.Failed = failed
	r.Score = score
	r.Error = errMsg
	r.FinishedAt = &now
	m.runs[runID] = r
	m.gen++
	return nil
}

func (m *MemStore) PutResult(_ context.Context, runID string, res Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[runID] = append(m.results[runID], res)
	m.gen++
	return nil
}

func (m *MemStore) ListResults(_ context.Context, runID string) ([]Result, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.results[runID]
	out := make([]Result, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CaseIndex < out[j].CaseIndex
	})
	return out, nil
}

var _ EvalStore = (*MemStore)(nil)
