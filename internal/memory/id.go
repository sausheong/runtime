// Package memory is a multi-tenant Postgres backend for harness's
// tool/memory.MemoryStore. One append-only memory_events table holds the
// create/update/delete event log; reads project the live set in SQL. Each Store
// instance is pinned to a tenant at construction; every query filters by it.
package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// generateID returns a sortable, collision-resistant id of the form
// mem_YYYY-MM-DD_<8-char-hex>, matching the JSONL reference backend's scheme.
func generateID(now time.Time) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("mem_%s_%s", now.UTC().Format("2006-01-02"), hex.EncodeToString(buf[:]))
}
