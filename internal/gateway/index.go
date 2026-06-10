package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"sync"

	"github.com/sausheong/harness/tool"
	"github.com/sausheong/runtime/internal/memory"
)

// Match is one search result: enough for the agent to call the tool
// immediately (full schema inline, no second round-trip).
type Match struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Score       float64         `json:"score"`
}

// Index ranks a view's tools against a natural-language query by embedding
// cosine similarity. Vectors are cached by tool content identity (hash of
// name+description), so each distinct tool text embeds once per process
// lifetime — across views, generations, and reconnects. Lazy only: vectors
// are computed on the first Search that needs them.
type Index struct {
	emb      memory.Embedder
	floor    float64
	defaultK int

	mu     sync.Mutex
	vecs   map[[32]byte][]float32 // content hash → vector
	logged map[[32]byte]bool      // embed-failure logged once per text
}

// searchCapK is the hard ceiling on k regardless of the request.
const searchCapK = 20

// NewIndex builds an Index. floor is the minimum cosine similarity for a
// match; defaultK is used when a search request omits k (or passes <=0).
func NewIndex(emb memory.Embedder, floor float64, defaultK int) *Index {
	return &Index{
		emb:      emb,
		floor:    floor,
		defaultK: defaultK,
		vecs:     map[[32]byte][]float32{},
		logged:   map[[32]byte]bool{},
	}
}

// toolText is the embedding input for a tool: name + description, newline
// separated (tool names cannot contain newlines).
func toolText(t tool.Tool) string { return t.Name() + "\n" + t.Description() }

func toolKey(text string) [32]byte { return sha256.Sum256([]byte(text)) }

// Search embeds query, ensures vectors for tools (embedding misses
// sequentially; a tool whose embed fails is skipped this round, logged once
// per text, retried next Search), then returns up to k matches with cosine
// >= floor, sorted descending. k<=0 ⇒ defaultK; k clamps to searchCapK. A
// query-embed failure returns an error (caller maps it to an MCP isError).
func (ix *Index) Search(ctx context.Context, tools []tool.Tool, query string, k int) ([]Match, error) {
	if k <= 0 {
		k = ix.defaultK
	}
	if k > searchCapK {
		k = searchCapK
	}
	qv, err := ix.emb.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	var out []Match
	for _, t := range tools {
		text := toolText(t)
		key := toolKey(text)
		ix.mu.Lock()
		v, ok := ix.vecs[key]
		ix.mu.Unlock()
		if !ok {
			ev, eerr := ix.emb.Embed(ctx, text)
			if eerr != nil {
				ix.mu.Lock()
				already := ix.logged[key]
				ix.logged[key] = true
				ix.mu.Unlock()
				if !already {
					slog.Warn("gateway: tool embed failed; excluded from search until it succeeds",
						"tool", t.Name(), "err", eerr)
				}
				continue
			}
			ix.mu.Lock()
			ix.vecs[key] = ev
			delete(ix.logged, key)
			ix.mu.Unlock()
			v = ev
		}
		score := cosine(qv, v)
		if score < ix.floor {
			continue
		}
		out = append(out, Match{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(t.Parameters()),
			Score:       score,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// cosine computes cosine similarity; 0 on dimension mismatch or zero vector.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
