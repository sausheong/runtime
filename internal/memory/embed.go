package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Embedder turns text into a fixed-length embedding vector. Implementations are
// safe for concurrent use. An Embedder is optional on the Store; when absent the
// store behaves exactly as M1 (no vectors, no semantic recall).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dim reports the embedding dimension (the vector(N) column width).
	Dim() int
}

// httpEmbedder calls an OpenAI-compatible POST {baseURL}/embeddings.
type httpEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	dim     int
	client  *http.Client
}

// Dim reports the configured embedding dimension.
func (e *httpEmbedder) Dim() int { return e.dim }

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}
type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed requests one embedding and validates its length against the configured
// dimension. A length mismatch is an error (prevents a pgvector insert failure
// from a misconfigured model).
func (e *httpEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(e.baseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memory: embed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memory: embed status %d", resp.StatusCode)
	}
	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("memory: embed decode: %w", err)
	}
	if len(er.Data) == 0 {
		return nil, fmt.Errorf("memory: embed: empty data")
	}
	vec := er.Data[0].Embedding
	if len(vec) != e.dim {
		return nil, fmt.Errorf("memory: embed dim mismatch: got %d want %d", len(vec), e.dim)
	}
	return vec, nil
}

// NewEmbedderFromEnv builds an Embedder from operator env:
//
//	RUNTIME_EMBED_MODEL  embedding model (unset ⇒ disabled)
//	RUNTIME_EMBED_DIM    vector dimension (required + positive when model set)
//	OPENAI_BASE_URL      proxy base (reused)
//	OPENAI_API_KEY       proxy bearer (reused)
//
// Returns enabled=false (no error) when the model is unset. Returns an error
// when the model is set but the dim is missing/non-positive (operator error;
// runtimed/agentd should treat it as fatal).
func NewEmbedderFromEnv() (emb Embedder, dim int, enabled bool, err error) {
	model := os.Getenv("RUNTIME_EMBED_MODEL")
	if model == "" {
		return nil, 0, false, nil
	}
	dimStr := os.Getenv("RUNTIME_EMBED_DIM")
	d, derr := strconv.Atoi(dimStr)
	if derr != nil || d <= 0 {
		return nil, 0, false, fmt.Errorf("memory: RUNTIME_EMBED_DIM must be a positive integer when RUNTIME_EMBED_MODEL is set (got %q)", dimStr)
	}
	e := &httpEmbedder{
		baseURL: os.Getenv("OPENAI_BASE_URL"),
		apiKey:  os.Getenv("OPENAI_API_KEY"),
		model:   model,
		dim:     d,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
	return e, d, true, nil
}
