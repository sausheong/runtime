package agentruntime

import (
	"bytes"
	"testing"
)

func TestWriteSSE(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSSE(&buf, WireEvent{Type: "text", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	want := "data: {\"type\":\"text\",\"text\":\"hi\"}\n\n"
	if buf.String() != want {
		t.Fatalf("writeSSE = %q, want %q", buf.String(), want)
	}
}
