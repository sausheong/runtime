package agentruntime

import (
	"encoding/json"
	"io"
	"strconv"
)

// writeSSE encodes one WireEvent as a Server-Sent Event frame. When the event
// carries a positive Seq, an id: line is emitted before data: so clients get
// standard Last-Event-ID semantics for dedupe/resume.
func writeSSE(w io.Writer, ev WireEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var sb []byte
	if ev.Seq > 0 {
		sb = append(sb, []byte("id: "+strconv.FormatInt(ev.Seq, 10)+"\n")...)
	}
	sb = append(sb, []byte("data: "+string(b)+"\n\n")...)
	_, err = w.Write(sb)
	return err
}
