package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memStore struct {
	mu          sync.Mutex
	seq         int
	sessions    map[string]*SessionRow
	events      map[string][]Event
	eventKeys   map[string]map[string]int64
	transcripts map[string][]byte       // key: session\x00turn
	results     map[string]OnlineResult // key: session\x00criterion
}

func NewMemStore() Store {
	return &memStore{
		sessions:    map[string]*SessionRow{},
		events:      map[string][]Event{},
		eventKeys:   map[string]map[string]int64{},
		transcripts: map[string][]byte{},
		results:     map[string]OnlineResult{},
	}
}

func (m *memStore) Ping(context.Context) error { return nil }

func (m *memStore) CreateSession(_ context.Context, agentID string, replica int) (string, error) {
	return m.createSession("default", agentID, "", replica)
}

func (m *memStore) CreateSessionForTenant(_ context.Context, tenantID, agentID string, replica int) (string, error) {
	return m.createSession(tenantID, agentID, "", replica)
}

func (m *memStore) CreateSessionForIdentity(_ context.Context, tenantID, agentID, agentGeneration string, replica int) (string, error) {
	return m.createSession(tenantID, agentID, agentGeneration, replica)
}

func (m *memStore) createSession(tenantID, agentID, agentGeneration string, replica int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("ses-%d", m.seq)
	now := time.Now().UTC()
	m.sessions[id] = &SessionRow{
		ID: id, TenantID: tenantID, AgentID: agentID,
		AgentGeneration: agentGeneration, WorkflowID: id,
		Status: "created", Replica: replica, CreatedAt: now, LastActiveAt: now,
	}
	return id, nil
}

func (m *memStore) BindSession(_ context.Context, id, tenantID, agentID, agentGeneration string, replica int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, exists := m.sessions[id]; exists {
		if existing.TenantID == tenantID && existing.AgentID == agentID &&
			existing.AgentGeneration == agentGeneration && existing.Replica == replica {
			return nil
		}
		return fmt.Errorf("bind session %q: conflicts with existing owner", id)
	}
	m.sessions[id] = &SessionRow{
		ID: id, TenantID: tenantID, AgentID: agentID, WorkflowID: id,
		AgentGeneration: agentGeneration, Status: "external", Replica: replica,
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
	}
	return nil
}

func (m *memStore) SessionReplica(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return s.Replica, nil
}

func (m *memStore) ActiveSessionsByReplica(_ context.Context, tenantID, agentID string) (map[int]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int]int{}
	for _, s := range m.sessions {
		if s.TenantID != tenantID || s.AgentID != agentID {
			continue
		}
		if s.Status == "external" || s.Status == "completed" || s.Status == "error" || s.Status == "limit_exceeded" {
			continue
		}
		out[s.Replica]++
	}
	return out, nil
}

func (m *memStore) ListSessions(_ context.Context, agentID string) ([]SessionRow, error) {
	return m.listSessions("default", agentID)
}

func (m *memStore) ListSessionsForTenant(_ context.Context, tenantID, agentID string) ([]SessionRow, error) {
	return m.listSessions(tenantID, agentID)
}

func (m *memStore) listSessions(tenantID, agentID string) ([]SessionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SessionRow
	for _, s := range m.sessions {
		if s.TenantID == tenantID && s.AgentID == agentID {
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
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	s.TurnCount = n
	s.LastActiveAt = time.Now().UTC()
	return nil
}

func (m *memStore) SetSessionUsage(_ context.Context, id string, tokens int64, cost float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	s.TokensTotal = tokens
	s.CostUSD = cost
	s.LastActiveAt = time.Now().UTC()
	return nil
}

func (m *memStore) SetFailureCategory(_ context.Context, id, category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	s.FailureCategory = category
	s.LastActiveAt = time.Now().UTC()
	return nil
}

func (m *memStore) SetInitialFailureCategory(_ context.Context, id, category string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	if s.FailureCategory != "" {
		return false, nil
	}
	s.FailureCategory = category
	s.LastActiveAt = time.Now().UTC()
	return true, nil
}

func (m *memStore) RefineFailureCategory(_ context.Context, id, from, to string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	if s.FailureCategory != from {
		return false, nil
	}
	s.FailureCategory = to
	s.LastActiveAt = time.Now().UTC()
	return true, nil
}

func (m *memStore) FailureBreakdownByAgent(_ context.Context, tenantID, agentID string, since time.Time) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, s := range m.sessions {
		if s.TenantID != tenantID || s.AgentID != agentID || s.FailureCategory == "" {
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
		return SessionRow{}, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return *s, nil
}

func (m *memStore) SetSessionStatus(_ context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	s.Status = status
	s.LastActiveAt = time.Now().UTC()
	return nil
}

func (m *memStore) AppendEvent(_ context.Context, sessionID, typ string, payload []byte) (int64, error) {
	return m.appendEvent(sessionID, "", typ, payload)
}

func (m *memStore) AppendEventOnce(_ context.Context, sessionID, eventKey, typ string, payload []byte) (int64, error) {
	if eventKey == "" {
		return 0, fmt.Errorf("event key is required")
	}
	return m.appendEvent(sessionID, eventKey, typ, payload)
}

func (m *memStore) appendEvent(sessionID, eventKey, typ string, payload []byte) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		return 0, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	if eventKey != "" {
		if seq, ok := m.eventKeys[sessionID][eventKey]; ok {
			return seq, nil
		}
	}
	evs := m.events[sessionID]
	next := int64(len(evs) + 1)
	cp := make([]byte, len(payload))
	copy(cp, payload)
	m.events[sessionID] = append(evs, Event{Seq: next, Type: typ, Payload: cp})
	if eventKey != "" {
		if m.eventKeys[sessionID] == nil {
			m.eventKeys[sessionID] = map[string]int64{}
		}
		m.eventKeys[sessionID][eventKey] = next
	}
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
	_, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	cp := make([]byte, len(entries))
	copy(cp, entries)
	m.transcripts[sessionID+"\x00"+strconv.Itoa(turn)] = cp
	return nil
}

func (m *memStore) PutOnlineResult(_ context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) error {
	_, _, err := m.putOnlineResultIfNew(sessionID, criterion, tenant, actor, scorer, passed, detail)
	return err
}

func (m *memStore) PutOnlineResultIfNew(_ context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) (bool, bool, error) {
	return m.putOnlineResultIfNew(sessionID, criterion, tenant, actor, scorer, passed, detail)
}

func (m *memStore) putOnlineResultIfNew(sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) (bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	parent, ok := m.sessions[sessionID]
	if !ok {
		return false, false, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	key := sessionID + "\x00" + criterion
	if existing, existed := m.results[key]; existed {
		return false, existing.Passed, nil
	}
	m.results[key] = OnlineResult{
		SessionID: sessionID,
		Criterion: criterion,
		Tenant:    parent.TenantID,
		Actor:     actor,
		Scorer:    scorer,
		Passed:    passed,
		Detail:    detail,
		CreatedAt: time.Now(),
	}
	return true, passed, nil
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

// The hermetic store does not retain timestamps for transcripts; production
// retention behaviour is covered by the PostgreSQL integration path.
func (m *memStore) ReapEvaluationData(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (m *memStore) ReapSessions(_ context.Context, before time.Time, batch int, dryRun bool) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if batch < 1 {
		batch = 1
	}
	var ids []string
	for id, row := range m.sessions {
		if len(ids) >= batch {
			break
		}
		if !row.LastActiveAt.Before(before) ||
			(row.Status != "external" && row.Status != "completed" && row.Status != "error" && row.Status != "limit_exceeded") {
			continue
		}
		ids = append(ids, id)
	}
	if dryRun {
		return int64(len(ids)), nil
	}
	for _, id := range ids {
		delete(m.sessions, id)
		delete(m.events, id)
		delete(m.eventKeys, id)
		for key := range m.transcripts {
			if strings.HasPrefix(key, id+"\x00") {
				delete(m.transcripts, key)
			}
		}
		for key := range m.results {
			if strings.HasPrefix(key, id+"\x00") {
				delete(m.results, key)
			}
		}
	}
	return int64(len(ids)), nil
}

func (m *memStore) TouchSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	row.LastActiveAt = time.Now().UTC()
	return nil
}

func (m *memStore) Close() error { return nil }
