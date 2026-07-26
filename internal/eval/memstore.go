package eval

import (
	"context"
	"fmt"
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
	if err := validateNewRun(r); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.runs[r.RunID]; exists {
		return fmt.Errorf("%w: %s", ErrRunExists, r.RunID)
	}
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

func (m *MemStore) ListIncompleteRuns(_ context.Context, limit int) ([]Run, error) {
	if limit < 1 {
		limit = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Run, 0, limit)
	now := time.Now().UTC()
	for _, r := range m.runs {
		claimable := r.LeaseOwner == "" || r.LeaseUntil == nil || !r.LeaseUntil.After(now)
		if (r.Status == StatusPending || r.Status == StatusRunning) && claimable {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].RunID < out[j].RunID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) ClaimRun(_ context.Context, runID, owner string, now, until time.Time) (bool, error) {
	if err := validateClaim(owner, now, until); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok || (r.Status != StatusPending && r.Status != StatusRunning) {
		return false, nil
	}
	if r.LeaseOwner != "" && r.LeaseOwner != owner && r.LeaseUntil != nil && r.LeaseUntil.After(now) {
		return false, nil
	}
	r.Status = StatusRunning
	r.LeaseOwner = owner
	r.LeaseUntil = &until
	m.runs[runID] = r
	m.gen++
	return true, nil
}

func (m *MemStore) FailPendingRun(_ context.Context, runID, errMsg string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok || r.Status != StatusPending || r.LeaseOwner != "" {
		return false, nil
	}
	now := time.Now().UTC()
	r.Status = StatusError
	r.Error = errMsg
	r.FinishedAt = &now
	m.runs[runID] = r
	m.gen++
	return true, nil
}

func (m *MemStore) FinishRunClaimed(_ context.Context, runID, owner, status string, total, passed, failed int, score float64, errMsg string) (bool, error) {
	if err := validateFinalization(owner, status); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok || r.LeaseOwner != owner || r.Status != StatusRunning ||
		r.LeaseUntil == nil || !r.LeaseUntil.After(time.Now().UTC()) {
		return false, nil
	}
	now := time.Now().UTC()
	r.Status = status
	r.Total = total
	r.Passed = passed
	r.Failed = failed
	r.Score = score
	r.Error = errMsg
	r.LeaseOwner = ""
	r.LeaseUntil = nil
	r.FinishedAt = &now
	m.runs[runID] = r
	m.gen++
	return true, nil
}

func (m *MemStore) PutResultClaimed(_ context.Context, runID, owner string, res Result) (bool, error) {
	if owner == "" {
		return false, fmt.Errorf("%w: result owner is required", ErrInvalidRunTransition)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok || r.LeaseOwner != owner || r.Status != StatusRunning ||
		r.LeaseUntil == nil || !r.LeaseUntil.After(time.Now().UTC()) {
		return false, nil
	}
	results := m.results[runID]
	for i := range results {
		if results[i].CaseIndex == res.CaseIndex {
			results[i] = res
			m.results[runID] = results
			m.gen++
			return true, nil
		}
	}
	m.results[runID] = append(results, res)
	m.gen++
	return true, nil
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

func (m *MemStore) ReapBefore(_ context.Context, before time.Time, batch int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if batch < 1 {
		batch = 1
	}
	var n int64
	for id, run := range m.runs {
		if n >= int64(batch) {
			break
		}
		if run.FinishedAt == nil || !run.FinishedAt.Before(before) ||
			(run.Status != StatusCompleted && run.Status != StatusError) {
			continue
		}
		delete(m.runs, id)
		delete(m.results, id)
		n++
	}
	if n > 0 {
		m.gen++
	}
	return n, nil
}

var _ EvalStore = (*MemStore)(nil)
