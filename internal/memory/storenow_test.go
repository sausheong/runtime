package memory

import (
	"testing"
	"time"
)

// nanosecondInstant is a sub-microsecond timestamp of the shape time.Now()
// returns on Linux. It is the exact value from the CI failure that exposed the
// bug, so these tests reproduce it on any host regardless of clock resolution.
var nanosecondInstant = time.Date(2026, 7, 26, 12, 47, 17, 285252235, time.UTC)

// TestStoreNowSurvivesPostgresRoundTrip pins the resolution contract between
// the timestamps this package returns to callers and the TIMESTAMPTZ column
// they are written to. PostgreSQL stores microseconds; a nanosecond-precision
// time.Now() silently loses its last three digits on the way in.
//
// That was a live, platform-dependent bug. Update carries the birth CreatedAt
// forward by reading the persisted row back, so on Linux — whose clock returns
// nanoseconds — the entry Save returned and the entry Update returned differed,
// failing TestStore_UpdateSupersedes in CI. On macOS, whose time.Now() is
// already microsecond-granular, the same code passed every time. This test is
// hermetic and does not depend on the host clock's resolution, so it fails on
// either platform if the truncation is removed.
func TestStoreNowSurvivesPostgresRoundTrip(t *testing.T) {
	now := storeNow()
	if now.Truncate(time.Microsecond) != now {
		t.Fatalf("storeNow() = %v has sub-microsecond precision; it cannot survive a TIMESTAMPTZ round trip", now.Format(time.RFC3339Nano))
	}
	if now.Location() != time.UTC {
		t.Fatalf("storeNow() = %v, want UTC", now)
	}
	// Asserting on storeNow()'s own output is not sufficient: on a host whose
	// clock is already microsecond-granular (macOS) the check above passes even
	// with the truncation deleted, which is precisely how the bug reached CI.
	// Feed it a known nanosecond instant through the same rounding rule so the
	// assertion has teeth on every platform.
	if truncateForStore(nanosecondInstant).Equal(nanosecondInstant) {
		t.Fatal("storeNow's rounding rule is a no-op on a sub-microsecond instant; a nanosecond clock would desync from the database")
	}
}

// TestStoreNowMatchesSimulatedPostgresTruncation is the part the test above
// cannot prove on a microsecond-clock host: that the value we hand back equals
// what the database would hand back. It simulates the column's rounding
// explicitly rather than trusting the local clock to expose the difference.
func TestStoreNowMatchesSimulatedPostgresTruncation(t *testing.T) {
	// What PostgreSQL persists and returns for a nanosecond-precision input.
	asStored := nanosecondInstant.Truncate(time.Microsecond)
	if nanosecondInstant.Equal(asStored) {
		t.Fatal("fixture is not sub-microsecond; the test would prove nothing")
	}

	// The contract: the value this package hands a caller must already equal
	// the value the database would hand back, so the two compare equal after a
	// round trip. This calls the production rule — asserting on locally
	// computed arithmetic instead would pass no matter what the code does.
	got := truncateForStore(nanosecondInstant)
	if !got.Equal(asStored) {
		t.Fatalf("truncateForStore(%v) = %v, want %v (what TIMESTAMPTZ returns)",
			nanosecondInstant.Format(time.RFC3339Nano),
			got.Format(time.RFC3339Nano), asStored.Format(time.RFC3339Nano))
	}
	if got.Nanosecond()%1000 != 0 {
		t.Fatalf("value %v retains sub-microsecond digits", got.Format(time.RFC3339Nano))
	}
}
