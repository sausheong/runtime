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

// EpisodeExtractor reads a finished thread and returns timestamped event records
// ("what happened"). Implementations are safe for concurrent use.
type EpisodeExtractor interface {
	Extract(ctx context.Context, thread []hrt.Message) ([]string, error)
}

const episodeSystemPrompt = "Extract timestamped event records from this conversation: what the user asked for and what happened as a result (actions taken and their outcomes). Return ONLY a JSON array of short event statements (strings), each a single 'what happened' occurrence. Return [] if nothing happened worth recording. Exclude standing facts about the user, pleasantries, and the assistant's internal reasoning."

// httpEpisodeExtractor calls an OpenAI-compatible POST {baseURL}/chat/completions.
type httpEpisodeExtractor struct {
	baseURL     string
	apiKey      string
	model       string
	maxEpisodes int
	client      *http.Client
}

// Extract requests episode extraction and parses a JSON array of strings from
// the reply. Malformed/empty ⇒ nil (degrade, do not break ingest); transport or
// non-200 ⇒ real error.
func (e *httpEpisodeExtractor) Extract(ctx context.Context, thread []hrt.Message) ([]string, error) {
	reqBody := chatRequest{
		Model: e.model,
		Messages: []chatMessage{
			{Role: "system", Content: episodeSystemPrompt},
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
		return nil, fmt.Errorf("memory: episode request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memory: episode status %d", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("memory: episode decode: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, nil
	}
	events := parseFacts(cr.Choices[0].Message.Content) // reuse the JSON-array parser
	if e.maxEpisodes > 0 && len(events) > e.maxEpisodes {
		events = events[:e.maxEpisodes]
	}
	return events, nil
}

// NewEpisodeExtractorFromEnv builds an EpisodeExtractor, enabled only when a
// model resolves:
//
//	RUNTIME_EPISODIC_MODEL  extraction chat model (falls back to RUNTIME_INGEST_MODEL)
//	RUNTIME_EPISODIC_MAX    hard cap on episodes per turn (default 5)
//	OPENAI_BASE_URL         proxy base (reused)
//	OPENAI_API_KEY          proxy bearer (reused)
func NewEpisodeExtractorFromEnv() (EpisodeExtractor, bool) {
	model := strings.TrimSpace(os.Getenv("RUNTIME_EPISODIC_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("RUNTIME_INGEST_MODEL"))
	}
	if model == "" {
		return nil, false
	}
	maxEp := 5
	if v := os.Getenv("RUNTIME_EPISODIC_MAX"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			maxEp = n
		} else {
			slog.Warn("memory: ignoring malformed RUNTIME_EPISODIC_MAX; using default", "value", v, "default", maxEp)
		}
	}
	e := &httpEpisodeExtractor{
		baseURL:     os.Getenv("OPENAI_BASE_URL"),
		apiKey:      os.Getenv("OPENAI_API_KEY"),
		model:       model,
		maxEpisodes: maxEp,
		client:      &http.Client{Timeout: 30 * time.Second},
	}
	return e, true
}
