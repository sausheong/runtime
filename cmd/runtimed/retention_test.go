package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunSessionRetentionSweepRecordsPartialSuccess(t *testing.T) {
	calls := 0
	recorded := map[string]int64{}
	total, err := runSessionRetentionSweep(
		context.Background(),
		time.Now(),
		500,
		false,
		func(context.Context, time.Time, int, bool) (int64, error) {
			calls++
			if calls == 1 {
				return 500, nil
			}
			return 0, errors.New("later batch failed")
		},
		func(kind string, n int64) { recorded[kind] += n },
	)
	if err == nil || total != 500 || recorded["session"] != 500 {
		t.Fatalf("total=%d recorded=%v err=%v", total, recorded, err)
	}
}

func TestRunSessionRetentionSweepDryRunDoesNotRecordDeletion(t *testing.T) {
	recorded := int64(0)
	total, err := runSessionRetentionSweep(
		context.Background(),
		time.Now(),
		500,
		true,
		func(context.Context, time.Time, int, bool) (int64, error) {
			return 500, nil
		},
		func(_ string, n int64) { recorded += n },
	)
	if err != nil || total != 500 || recorded != 0 {
		t.Fatalf("total=%d recorded=%d err=%v", total, recorded, err)
	}
}

func TestRunEvalRetentionSweepRecordsCaptureBeforeFailure(t *testing.T) {
	calls := 0
	recorded := map[string]int64{}
	captured, runs, err := runEvalRetentionSweep(
		context.Background(),
		time.Now(),
		1000,
		func(context.Context, time.Time, int) (int64, error) {
			calls++
			if calls == 1 {
				return 7, nil
			}
			return 0, errors.New("capture failed")
		},
		func(context.Context, time.Time, int) (int64, error) {
			t.Fatal("run retention must not start after capture failure")
			return 0, nil
		},
		func(kind string, n int64) { recorded[kind] += n },
	)
	if err == nil || captured != 7 || runs != 0 || recorded["evaluation_capture"] != 7 {
		t.Fatalf("captured=%d runs=%d recorded=%v err=%v", captured, runs, recorded, err)
	}
}

func TestRunEvalRetentionSweepRecordsRunsBeforeLaterFailure(t *testing.T) {
	runCalls := 0
	recorded := map[string]int64{}
	captured, runs, err := runEvalRetentionSweep(
		context.Background(),
		time.Now(),
		1000,
		func(context.Context, time.Time, int) (int64, error) { return 0, nil },
		func(context.Context, time.Time, int) (int64, error) {
			runCalls++
			if runCalls == 1 {
				return 1000, nil
			}
			return 0, errors.New("run batch failed")
		},
		func(kind string, n int64) { recorded[kind] += n },
	)
	if err == nil || captured != 0 || runs != 1000 || recorded["evaluation_run"] != 1000 {
		t.Fatalf("captured=%d runs=%d recorded=%v err=%v", captured, runs, recorded, err)
	}
}
