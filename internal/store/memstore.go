package store

import (
	"context"
	"fmt"
	"sync"
)

type memStore struct {
	mu       sync.Mutex
	seq      int
	sessions map[string]*SessionRow
	events   map[string][]Event
}

func NewMemStore() Store {
	return &memStore{sessions: map[string]*SessionRow{}, events: map[string][]Event{}}
}

func (m *memStore) CreateSession(_ context.Context, agentID, workflowID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("ses-%d", m.seq)
	m.sessions[id] = &SessionRow{ID: id, AgentID: agentID, WorkflowID: workflowID, Status: "created"}
	return id, nil
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

func (m *memStore) AppendEvent(_ context.Context, sessionID, typ string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.events[sessionID]
	next := int64(len(evs) + 1)
	cp := make([]byte, len(payload))
	copy(cp, payload)
	m.events[sessionID] = append(evs, Event{Seq: next, Type: typ, Payload: cp})
	return nil
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

func (m *memStore) Close() error { return nil }
