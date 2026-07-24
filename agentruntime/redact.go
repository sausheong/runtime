package agentruntime

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/sausheong/harness/session"
)

var embeddedSecretPattern = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]{12,}|\b(?:sk|svk)-[A-Za-z0-9._~+/=-]{12,}`)

// marshalTranscript serializes a turn after recursively redacting values under
// credential-shaped keys and common bearer/service-key patterns in free text.
// The live session retains the original values; only the evaluation/audit copy
// is transformed.
func marshalTranscript(entries []session.SessionEntry) ([]byte, error) {
	raw, err := json.Marshal(entries)
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
		"authorization", "cookie", "credential", "password", "secret",
		"api_key", "access_token", "refresh_token", "id_token",
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
