package agentruntime

import (
	"strings"
	"testing"

	"github.com/sausheong/harness/session"
)

func TestMarshalTranscriptRedactsCredentials(t *testing.T) {
	entries := []session.SessionEntry{{
		Type: session.EntryTypeToolResult,
		Data: []byte(`{
			"authorization":"Bearer abcdefghijklmnopqrstuvwxyz",
			"nested":{"api_key":"sk-abcdefghijklmnop","safe":"keep"},
			"output":"request used svk-abcdefghijklmnop"
		}`),
	}}
	got, err := marshalTranscript(entries)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, secret := range []string{"abcdefghijklmnopqrstuvwxyz", "sk-abcdefghijklmnop", "svk-abcdefghijklmnop"} {
		if strings.Contains(text, secret) {
			t.Fatalf("transcript leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "keep") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("unexpected redacted transcript: %s", text)
	}
}
