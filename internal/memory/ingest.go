package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	hrt "github.com/sausheong/harness/runtime"
)

// Extractor reads a finished conversation thread and returns durable facts worth
// remembering long-term. Implementations are safe for concurrent use. Optional on
// the KG; when absent, Ingest is a no-op (M2 behavior).
type Extractor interface {
	Extract(ctx context.Context, thread []hrt.Message) ([]string, error)
}

// extractSystemPrompt instructs the model to emit durable facts as a JSON array.
const extractSystemPrompt = "Extract durable, user-specific facts worth remembering long-term from this conversation. Return ONLY a JSON array of short factual statements (strings). Return [] if nothing is worth remembering. Exclude ephemeral details, pleasantries, and the assistant's own reasoning."

// httpExtractor calls an OpenAI-compatible POST {baseURL}/chat/completions.
type httpExtractor struct {
	baseURL  string
	apiKey   string
	model    string
	maxFacts int
	client   *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// renderThread joins the thread into a single user-message body.
func renderThread(thread []hrt.Message) string {
	var b strings.Builder
	for _, m := range thread {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	return b.String()
}

// Extract requests fact extraction and parses a JSON array of strings from the
// model reply. A malformed (non-JSON / non-array) or empty reply yields zero
// facts (nil, nil) rather than an error — extraction degrades, it does not break
// ingest. A transport error or non-200 status is a real error.
func (e *httpExtractor) Extract(ctx context.Context, thread []hrt.Message) ([]string, error) {
	reqBody := chatRequest{
		Model: e.model,
		Messages: []chatMessage{
			{Role: "system", Content: extractSystemPrompt},
			{Role: "user", Content: renderThread(thread)},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(e.baseURL, "/") + "/chat/completions"
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
		return nil, fmt.Errorf("memory: extract request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memory: extract status %d", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("memory: extract decode: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, nil
	}
	facts := parseFacts(cr.Choices[0].Message.Content)
	if e.maxFacts > 0 && len(facts) > e.maxFacts {
		facts = facts[:e.maxFacts]
	}
	return facts, nil
}

// parseFacts extracts a JSON array of strings from a model reply, tolerating a
// surrounding markdown code fence. Any failure (non-JSON, non-array) yields nil.
func parseFacts(content string) []string {
	s := stripCodeFence(strings.TrimSpace(content))
	var facts []string
	if err := json.Unmarshal([]byte(s), &facts); err != nil {
		return nil
	}
	return facts
}

// stripCodeFence removes a leading ``` (optionally ```json) line and a trailing
// ``` fence if present; otherwise returns s unchanged.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// NewExtractorFromEnv builds an Extractor from operator env:
//
//	RUNTIME_INGEST_MODEL      extraction chat model (unset ⇒ disabled)
//	RUNTIME_INGEST_MAX_FACTS  hard cap on facts per turn (default 10)
//	OPENAI_BASE_URL           proxy base (reused)
//	OPENAI_API_KEY            proxy bearer (reused)
//
// Returns enabled=false when the model is unset. Construction itself cannot fail
// (there is nothing to validate beyond the model presence the caller checks via
// enabled), so there is no error return; the "enabled but no model" fatal lives
// in the caller (agentkind.wireMemory).
func NewExtractorFromEnv() (ext Extractor, enabled bool) {
	model := os.Getenv("RUNTIME_INGEST_MODEL")
	if model == "" {
		return nil, false
	}
	maxFacts := 10
	if v := os.Getenv("RUNTIME_INGEST_MAX_FACTS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			maxFacts = n
		} else {
			slog.Warn("memory: ignoring malformed RUNTIME_INGEST_MAX_FACTS; using default", "value", v, "default", maxFacts)
		}
	}
	e := &httpExtractor{
		baseURL:  os.Getenv("OPENAI_BASE_URL"),
		apiKey:   os.Getenv("OPENAI_API_KEY"),
		model:    model,
		maxFacts: maxFacts,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	return e, true
}
