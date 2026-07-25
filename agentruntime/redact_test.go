package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/store"
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

func TestMarshalTranscriptRedactsStructuredAndBareTokenFormats(t *testing.T) {
	secrets := []string{
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.signature123",
		"ghp_abcdefghijklmnopqrstuvwxyz123456",
		"github_pat_11AAabcdefghijklmnopqrstuvwxyz",
		"xoxb-1234567890-abcdefghijklmnop",
		"AKIAABCDEFGHIJKLMNOP",
		"sk-ant-abcdefghijklmnopqrstuvwxyz",
		"svk-abcdefghijklmnopqrstuvwxyz",
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
			if strings.Contains(string(data), "ghp_abcdefghijklmnopqrstuvwxyz123456") {
				t.Fatal("custom filter received unredacted secret")
			}
			return []byte(`[{"custom":"filtered"}]`), nil
		},
	}
	m.captureTranscript("s", 0, "t", "u", []session.SessionEntry{{
		Type: session.EntryTypeMessage,
		Data: []byte(`{"text":"ghp_abcdefghijklmnopqrstuvwxyz123456"}`),
	}}, "completed", "completed")
	if !called || st.calls != 1 || string(st.data) != `[{"custom":"filtered"}]` {
		t.Fatalf("called=%v writes=%d data=%s", called, st.calls, st.data)
	}
}
