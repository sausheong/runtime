package memory

import (
	"context"
	"testing"
	"time"
)

func TestRunRetentionSweepDryRunCountsOnceWithoutDeletionMetric(t *testing.T) {
	calls := 0
	callbacks := 0
	runRetentionSweep(context.Background(), map[string]time.Duration{
		KindFact: time.Hour,
	}, 10, true, func(_ context.Context, kind string, _ time.Time, batch int, dryRun bool) (int, error) {
		calls++
		if kind != KindFact || batch != 10 || !dryRun {
			t.Fatalf("reap args: kind=%q batch=%d dryRun=%v", kind, batch, dryRun)
		}
		return 10, nil
	}, func(string, int) {
		callbacks++
	})
	if calls != 1 {
		t.Fatalf("dry-run calls=%d, want one bounded count pass", calls)
	}
	if callbacks != 0 {
		t.Fatalf("dry-run emitted %d deletion metric callbacks", callbacks)
	}
}

func TestRunRetentionSweepReportsDeletedRowsByKind(t *testing.T) {
	calls := 0
	gotKind, gotRows := "", 0
	runRetentionSweep(context.Background(), map[string]time.Duration{
		KindSummary: time.Hour,
	}, 2, false, func(_ context.Context, kind string, _ time.Time, _ int, dryRun bool) (int, error) {
		calls++
		if dryRun {
			t.Fatal("delete sweep unexpectedly used dry-run")
		}
		if calls == 1 {
			return 2, nil
		}
		return 1, nil
	}, func(kind string, rows int) {
		gotKind, gotRows = kind, rows
	})
	if calls != 2 || gotKind != KindSummary || gotRows != 3 {
		t.Fatalf("calls=%d callback=(%q,%d), want 2/(summary,3)", calls, gotKind, gotRows)
	}
}
