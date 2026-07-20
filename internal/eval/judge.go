package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Judge grades an agent's output against a target (expected answer or rubric).
type Judge interface {
	Grade(ctx context.Context, input, target, output string) (bool, string, error)
}

const evalJudgeSystemPrompt = "You are grading an AI agent's answer. Given the INPUT, the TARGET (expected answer or grading rubric), and the agent's ACTUAL output, decide whether the actual output satisfies the target. Return ONLY a JSON object {\"pass\": true|false, \"reason\": \"<one short sentence>\"}."

// httpJudge grades via an OpenAI-compatible chat endpoint.
type httpJudge struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewJudgeFromEnv builds a judge from RUNTIME_EVAL_JUDGE_MODEL + OPENAI_BASE_URL
// + OPENAI_API_KEY. Returns (nil,false) when no model is configured (judge
// cases then fail with an "unavailable" detail).
func NewJudgeFromEnv() (Judge, bool) {
	model := os.Getenv("RUNTIME_EVAL_JUDGE_MODEL")
	if model == "" {
		return nil, false
	}
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &httpJudge{
		baseURL: strings.TrimRight(base, "/"),
		apiKey:  os.Getenv("OPENAI_API_KEY"),
		model:   model,
		client:  &http.Client{Timeout: 60 * time.Second},
	}, true
}

type judgeChatReq struct {
	Model    string         `json:"model"`
	Messages []judgeChatMsg `json:"messages"`
}
type judgeChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type judgeChatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (h *httpJudge) Grade(ctx context.Context, input, target, output string) (bool, string, error) {
	user := fmt.Sprintf("INPUT:\n%s\n\nTARGET:\n%s\n\nACTUAL:\n%s", input, target, output)
	reqBody, _ := json.Marshal(judgeChatReq{
		Model: h.model,
		Messages: []judgeChatMsg{
			{Role: "system", Content: evalJudgeSystemPrompt},
			{Role: "user", Content: user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", h.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("judge non-200: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cr judgeChatResp
	if err := json.Unmarshal(body, &cr); err != nil || len(cr.Choices) == 0 {
		return false, "", fmt.Errorf("judge: unparseable response")
	}
	verdict := stripFence(cr.Choices[0].Message.Content)
	var v struct {
		Pass   bool   `json:"pass"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(verdict), &v); err != nil {
		return false, "", fmt.Errorf("judge: bad verdict json: %w", err)
	}
	return v.Pass, v.Reason, nil
}

// stripFence removes a leading/trailing ```json fence if present.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}
