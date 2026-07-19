package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	hrt "github.com/sausheong/harness/runtime"
)

// summarySystemPrompt instructs the model to emit a concise running digest.
const summarySystemPrompt = "Write a concise running summary of this conversation so far: the user's goal, key facts established, decisions made, and any open threads. Prefer 3-8 sentences. Return ONLY the summary text, with no preamble."

// Summarizer condenses a finished conversation thread into a single running
// digest. Implementations are safe for concurrent use. Optional on the KG; when
// absent, the summary strategy is simply not wired.
type Summarizer interface {
	Summarize(ctx context.Context, thread []hrt.Message) (string, error)
}

// httpSummarizer calls an OpenAI-compatible POST {baseURL}/chat/completions and
// returns the whole reply text as the digest (no JSON parse). Mirrors
// httpExtractor's transport shape.
type httpSummarizer struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// Summarize renders the thread into one user message, requests a summary, and
// returns the trimmed reply text. A transport error or non-200 status is a real
// error; an empty reply yields ("", nil) so the strategy degrades (no write)
// rather than breaking a turn.
func (s *httpSummarizer) Summarize(ctx context.Context, thread []hrt.Message) (string, error) {
	reqBody := chatRequest{
		Model: s.model,
		Messages: []chatMessage{
			{Role: "system", Content: summarySystemPrompt},
			{Role: "user", Content: renderThread(thread)},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(s.baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("memory: summarize request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("memory: summarize status %d", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("memory: summarize decode: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", nil
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}

// NewSummarizerFromEnv builds a Summarizer, enabled only when a model resolves:
//
//	RUNTIME_SUMMARY_MODEL  summary chat model (falls back to RUNTIME_INGEST_MODEL)
//	OPENAI_BASE_URL        proxy base (reused; defaults to the OpenAI public API)
//	OPENAI_API_KEY         proxy bearer (reused)
//
// Returns enabled=false when no model is configured.
func NewSummarizerFromEnv() (Summarizer, bool) {
	model := strings.TrimSpace(os.Getenv("RUNTIME_SUMMARY_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("RUNTIME_INGEST_MODEL"))
	}
	if model == "" {
		return nil, false
	}
	base := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	s := &httpSummarizer{
		baseURL: base,
		apiKey:  os.Getenv("OPENAI_API_KEY"),
		model:   model,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	return s, true
}
