// Package httplimit provides byte-bounded response decoding for internal HTTP
// clients. A timeout limits duration; these helpers independently limit memory.
package httplimit

import (
	"encoding/json"
	"fmt"
	"io"
)

// ReadAll reads at most max bytes and rejects a response that has any byte
// beyond the limit.
func ReadAll(r io.Reader, max int64) ([]byte, error) {
	if max < 1 {
		return nil, fmt.Errorf("response limit must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return data, nil
}

// DecodeJSON reads a bounded response and decodes exactly one JSON value.
func DecodeJSON(r io.Reader, max int64, out any) error {
	data, err := ReadAll(r, max)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}
	return nil
}
