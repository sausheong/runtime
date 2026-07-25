package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/sausheong/harness/session"
)

var embeddedSecretPattern = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]{12,}|\b(?:` +
	`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}|` +
	`(?:sk|svk)-[A-Za-z0-9._~+/=-]{12,}|sk-ant-[A-Za-z0-9_-]{12,}|` +
	`gh[pousr]_[A-Za-z0-9]{12,}|github_pat_[A-Za-z0-9_]{12,}|` +
	`xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[A-Z0-9]{16}` +
	`)\b`)

// marshalTranscript serializes a turn after recursively redacting both the
// SessionEntry envelope and structured/plain tool Data payloads. This remains
// best-effort pattern matching, not a guarantee that all personal or secret
// data has been removed.
func marshalTranscript(entries []session.SessionEntry) ([]byte, error) {
	sanitized := append([]session.SessionEntry(nil), entries...)
	for i := range sanitized {
		if len(sanitized[i].Data) == 0 {
			continue
		}
		var payload any
		if json.Unmarshal(sanitized[i].Data, &payload) == nil {
			redactValue(payload)
			data, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			sanitized[i].Data = data
		} else {
			sanitized[i].Data = []byte(redactEmbeddedSecrets(string(sanitized[i].Data)))
		}
	}
	raw, err := json.Marshal(sanitized)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	redactValue(value)
	return json.Marshal(value)
}

func redactValue(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if sensitiveTranscriptKey(key) {
				current[key] = "[REDACTED]"
				continue
			}
			if text, ok := child.(string); ok {
				current[key] = redactEmbeddedSecrets(text)
				continue
			}
			redactValue(child)
		}
	case []any:
		for i, child := range current {
			if text, ok := child.(string); ok {
				current[i] = redactEmbeddedSecrets(text)
				continue
			}
			redactValue(child)
		}
	}
}

func sensitiveTranscriptKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, marker := range []string{
		"authorization", "cookie", "set_cookie", "credential", "password", "passwd",
		"secret", "api_key", "apikey", "access_key", "private_key",
		"access_token", "refresh_token", "id_token", "session_token",
	} {
		if normalized == marker || strings.HasSuffix(normalized, "_"+marker) {
			return true
		}
	}
	return false
}

func redactEmbeddedSecrets(text string) string {
	return embeddedSecretPattern.ReplaceAllStringFunc(text, func(match string) string {
		if strings.HasPrefix(strings.ToLower(match), "bearer ") {
			return match[:7] + "[REDACTED]"
		}
		return "[REDACTED]"
	})
}

func (m *Manager) transcriptsEnabled() bool {
	return m.transcriptCapture == nil || *m.transcriptCapture
}

func (m *Manager) captureTranscript(sessionID string, turn int, tenant, actor string, entries []session.SessionEntry, stopReason, status string) {
	if !m.transcriptsEnabled() {
		return
	}
	data, err := marshalTranscript(entries)
	if err == nil && m.transcriptFilter != nil {
		data, err = m.transcriptFilter(data)
		if err == nil && !json.Valid(data) {
			err = fmt.Errorf("custom transcript filter returned invalid JSON")
		}
	}
	if err != nil {
		slog.Warn("filter transcript entries failed", "session", sessionID, "turn", turn, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.st.AppendTranscript(ctx, sessionID, turn, tenant, actor, data, stopReason, status); err != nil {
		slog.Warn("append transcript failed", "session", sessionID, "turn", turn, "err", err)
	}
}
