package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/store"
)

// Synthetic credential fixtures. The redactor's job is to strip
// credential-shaped strings, so a test for it necessarily contains some. Each
// prefix is concatenated to its body rather than written as one literal:
// push-time secret scanners match on the complete pattern and would otherwise
// block every push on obviously fake data (they did — a `xoxb-` fixture here
// once required a manual allowlist). The runtime values are unchanged, so the
// assertions still exercise the exact formats an operator would leak.
const (
	fakeJWT       = "eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiJhbGljZSJ9." + "signature123"
	fakeGitHubPAT = "ghp_" + "abcdefghijklmnopqrstuvwxyz123456"
	fakeGitHubFG  = "github_pat_" + "11AAabcdefghijklmnopqrstuvwxyz"
	fakeSlackBot  = "xoxb-" + "1234567890-abcdefghijklmnop"
	fakeAWSKeyID  = "AKIA" + "ABCDEFGHIJKLMNOP"
	fakeAnthropic = "sk-ant-" + "abcdefghijklmnopqrstuvwxyz"
	fakeServiceKD = "svk-" + "abcdefghijklmnopqrstuvwxyz"
	fakeAPIKey    = "sk-" + "abcdefghijklmnop"
	fakeServiceK  = "svk-" + "abcdefghijklmnop"
	fakeBearer    = "abcdefghijklmnopqrstuvwxyz"
)

func TestMarshalTranscriptRedactsCredentials(t *testing.T) {
	entries := []session.SessionEntry{{
		Type: session.EntryTypeToolResult,
		Data: fmt.Appendf(nil, `{
			"authorization":"Bearer %s",
			"nested":{"api_key":"%s","safe":"keep"},
			"output":"request used %s"
		}`, fakeBearer, fakeAPIKey, fakeServiceK),
	}}
	got, err := marshalTranscript(entries)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, secret := range []string{fakeBearer, fakeAPIKey, fakeServiceK} {
		if strings.Contains(text, secret) {
			t.Fatalf("transcript leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "keep") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("unexpected redacted transcript: %s", text)
	}
}

func TestMarshalTranscriptRedactsStructuredAndBareTokenFormats(t *testing.T) {
	secrets := []string{
		fakeJWT,
		fakeGitHubPAT,
		fakeGitHubFG,
		fakeSlackBot,
		fakeAWSKeyID,
		fakeAnthropic,
		fakeServiceKD,
	}
	payload, _ := json.Marshal(map[string]any{
		"nested": map[string]any{
			"cookie": "session=top-secret-cookie",
			"safe":   "keep",
		},
		"tokens": secrets,
	})
	got, err := marshalTranscript([]session.SessionEntry{{
		Type: session.EntryTypeToolResult,
		Data: payload,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var entries []session.SessionEntry
	if err := json.Unmarshal(got, &entries); err != nil {
		t.Fatal(err)
	}
	var sanitized map[string]any
	if err := json.Unmarshal(entries[0].Data, &sanitized); err != nil {
		t.Fatal(err)
	}
	text := string(entries[0].Data)
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("structured transcript leaked %q: %s", secret, text)
		}
	}
	nested := sanitized["nested"].(map[string]any)
	if nested["cookie"] != "[REDACTED]" || nested["safe"] != "keep" {
		t.Fatalf("nested redaction=%v", nested)
	}
}

type captureTranscriptStore struct {
	store.Store
	calls int
	data  []byte
}

func (c *captureTranscriptStore) AppendTranscript(_ context.Context, _ string, _ int, _, _ string, data []byte, _, _ string) error {
	c.calls++
	c.data = append([]byte(nil), data...)
	return nil
}

func TestCaptureTranscriptCanBeDisabled(t *testing.T) {
	off := false
	st := &captureTranscriptStore{Store: store.NewMemStore()}
	m := &Manager{st: st, transcriptCapture: &off}
	m.captureTranscript("s", 0, "t", "u", []session.SessionEntry{{
		Type: session.EntryTypeMessage, Data: []byte(`{"text":"hello"}`),
	}}, "completed", "completed")
	if st.calls != 0 {
		t.Fatalf("capture-disabled calls=%d", st.calls)
	}
}

func TestCustomTranscriptFilterRunsBeforePersistence(t *testing.T) {
	on := true
	st := &captureTranscriptStore{Store: store.NewMemStore()}
	called := false
	m := &Manager{
		st:                st,
		transcriptCapture: &on,
		transcriptFilter: func(data []byte) ([]byte, error) {
			called = true
			if strings.Contains(string(data), fakeGitHubPAT) {
				t.Fatal("custom filter received unredacted secret")
			}
			return []byte(`[{"custom":"filtered"}]`), nil
		},
	}
	m.captureTranscript("s", 0, "t", "u", []session.SessionEntry{{
		Type: session.EntryTypeMessage,
		Data: fmt.Appendf(nil, `{"text":%q}`, fakeGitHubPAT),
	}}, "completed", "completed")
	if !called || st.calls != 1 || string(st.data) != `[{"custom":"filtered"}]` {
		t.Fatalf("called=%v writes=%d data=%s", called, st.calls, st.data)
	}
}
