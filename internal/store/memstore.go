package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

type memStore struct {
	mu          sync.Mutex
	seq         int
	sessions    map[string]*SessionRow
	events      map[string][]Event
	transcripts map[string][]byte       // key: session\x00turn
	results     map[string]OnlineResult // key: session\x00criterion
}

func NewMemStore() Store {
	return &memStore{
		sessions:    map[string]*SessionRow{},
		events:      map[string][]Event{},
		transcripts: map[string][]byte{},
		results:     map[string]OnlineResult{},
	}
}

func (m *memStore) CreateSession(_ context.Context, agentID string, replica int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("ses-%d", m.seq)
	m.sessions[id] = &SessionRow{ID: id, AgentID: agentID, WorkflowID: id, Status: "created", Replica: replica}
	return id, nil
}

func (m *memStore) SessionReplica(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return 0, fmt.Errorf("session %q not found", id)
	}
	return s.Replica, nil
}

func (m *memStore) ActiveSessionsByReplica(_ context.Context, agentID string) (map[int]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int]int{}
	for _, s := range m.sessions {
		if s.AgentID != agentID {
			continue
		}
		if s.Status == "completed" || s.Status == "error" || s.Status == "limit_exceeded" {
			continue
		}
		out[s.Replica]++
	}
	return out, nil
}

func (m *memStore) ListSessions(_ context.Context, agentID string) ([]SessionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SessionRow
	for _, s := range m.sessions {
		if s.AgentID == agentID {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (m *memStore) SetTurnCount(_ context.Context, id string, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.TurnCount = n
	return nil
}

func (m *memStore) SetSessionUsage(_ context.Context, id string, tokens int64, cost float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.TokensTotal = tokens
	s.CostUSD = cost
	return nil
}

func (m *memStore) SetFailureCategory(_ context.Context, id, category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.FailureCategory = category
	return nil
}

func (m *memStore) FailureBreakdownByAgent(_ context.Context, agentID string, since time.Time) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, s := range m.sessions {
		if s.AgentID != agentID || s.FailureCategory == "" {
			continue
		}
		// memStore has no created_at; the since filter is a no-op here (the PG
		// impl enforces it and the integration test covers it). Documented.
		out[s.FailureCategory]++
	}
	return out, nil
}

func (m *memStore) GetSession(_ context.Context, id string) (SessionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return SessionRow{}, fmt.Errorf("session %q not found", id)
	}
	return *s, nil
}

func (m *memStore) SetSessionStatus(_ context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.Status = status
	return nil
}

func (m *memStore) AppendEvent(_ context.Context, sessionID, typ string, payload []byte) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.events[sessionID]
	next := int64(len(evs) + 1)
	cp := make([]byte, len(payload))
	copy(cp, payload)
	m.events[sessionID] = append(evs, Event{Seq: next, Type: typ, Payload: cp})
	return next, nil
}

func (m *memStore) EventsSince(_ context.Context, sessionID string, afterSeq int64) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events[sessionID] {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memStore) AppendTranscript(_ context.Context, sessionID string, turn int, tenant, actor string, entries []byte, stopReason, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(entries))
	copy(cp, entries)
	m.transcripts[sessionID+"\x00"+strconv.Itoa(turn)] = cp
	return nil
}

func (m *memStore) PutOnlineResult(_ context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionID + "\x00" + criterion
	existing, ok := m.results[key]
	created := time.Now()
	if ok {
		created = existing.CreatedAt
	}
	m.results[key] = OnlineResult{
		SessionID: sessionID,
		Criterion: criterion,
		Tenant:    tenant,
		Actor:     actor,
		Scorer:    scorer,
		Passed:    passed,
		Detail:    detail,
		CreatedAt: created,
	}
	return nil
}

func (m *memStore) ListOnlineResults(_ context.Context, sessionID string) ([]OnlineResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OnlineResult
	for _, r := range m.results {
		if r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Criterion < out[j].Criterion })
	return out, nil
}

func (m *memStore) ListOnlineResultsByTenant(_ context.Context, tenant string, limit int) ([]OnlineResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OnlineResult
	for _, r := range m.results {
		if r.Tenant == tenant {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }
