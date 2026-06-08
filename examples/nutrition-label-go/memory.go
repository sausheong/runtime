package nutrition

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// productRecord is a past verdict for a product, keyed by normalized name.
type productRecord struct {
	ProductName    string `json:"product_name"`
	Summary        string `json:"summary"`
	Recommendation string `json:"recommendation"`
}

// memory is cross-run state persisted to a JSON file. Only the exported map
// fields (LearnedAliases, Products) are serialized — the unexported path and mu
// fields are skipped by encoding/json, so the on-disk form is exactly
// {"learned_aliases": ..., "products": ...}.
//
// LearnedAliases maps a normalized label name to an additive number, learned
// when the model supplies an E-number hint. Products maps a normalized product
// name to its last recorded verdict.
type memory struct {
	path           string
	mu             sync.Mutex
	LearnedAliases map[string]string        `json:"learned_aliases"`
	Products       map[string]productRecord `json:"products"`
}

func newMemory(path string) *memory {
	m := &memory{path: path, LearnedAliases: map[string]string{}, Products: map[string]productRecord{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, m)
		if m.LearnedAliases == nil {
			m.LearnedAliases = map[string]string{}
		}
		if m.Products == nil {
			m.Products = map[string]productRecord{}
		}
	}
	return m
}

// save writes the memory to disk. The caller MUST hold m.mu — save does not
// lock, so that Marshal observes a consistent snapshot of the maps without
// racing a concurrent write.
func (m *memory) save() {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(m.path, b, 0o644)
}

func (m *memory) learnedAlias(normName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.LearnedAliases[normName]
}

func (m *memory) learnAlias(normName, number string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LearnedAliases[normName] = number
	m.save()
}

func (m *memory) recall(productName string) (productRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.Products[strings.ToLower(strings.TrimSpace(productName))]
	return rec, ok
}

func (m *memory) remember(rec productRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Products[strings.ToLower(strings.TrimSpace(rec.ProductName))] = rec
	m.save()
}
