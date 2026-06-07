package agentruntime

import (
	"encoding/json"
	"io"
)

// writeSSE encodes one WireEvent as a Server-Sent Event frame.
func writeSSE(w io.Writer, ev WireEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte("data: " + string(b) + "\n\n"))
	return err
}
