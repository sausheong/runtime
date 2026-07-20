package agentruntime

import (
	"context"
	"testing"

	"github.com/sausheong/runtime/internal/store"
)

// TestStatusForReason pins the transcript status mapping: only a clean
// completed turn is "completed", any other terminal reason is "error", and a
// non-terminal turn is "running". The full capture+score turn path needs a
// Manager+DBOS+provider harness that does not exist at this layer, so the
// end-to-end assertion (rate-100 policy ⇒ transcript row + result row) is
// covered by the Task 9 e2e; this pins the one new pure helper.
func TestStatusForReason(t *testing.T) {
	cases := []struct {
		name string
		out  turnOutput
		want string
	}{
		{"completed", turnOutput{Done: true, Reason: "completed"}, "completed"},
		{"aborted", turnOutput{Done: true, Reason: "aborted"}, "error"},
		{"error", turnOutput{Done: true, Reason: "error"}, "error"},
		{"running", turnOutput{Done: false, Reason: "end_turn"}, "running"},
	}
	for _, tc := range cases {
		if got := statusForReason(tc.out); got != tc.want {
			t.Errorf("%s: statusForReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPublishFanoutAndUnsubscribe validates the concurrency core: every live
// subscriber receives a published event, published events carry the store seq,
// and an unsubscribed channel stops receiving.
func TestPublishFanoutAndUnsubscribe(t *testing.T) {
	m := &Manager{agentID: "a", st: store.NewMemStore(), subscribers: map[string][]chan WireEvent{}}
	id, _ := m.st.CreateSession(context.Background(), "a", 0)
	ch1, unsub1 := m.subscribe(id)
	ch2, _ := m.subscribe(id)

	m.publish(id, WireEvent{Type: "text", Text: "x"})
	if got := (<-ch1).Text; got != "x" {
		t.Fatalf("ch1 got %q", got)
	}
	if got := (<-ch2).Text; got != "x" {
		t.Fatalf("ch2 got %q", got)
	}
	// the published event must carry a seq from the store
	m.publish(id, WireEvent{Type: "text", Text: "y"})
	e2 := <-ch2
	if e2.Seq == 0 {
		t.Fatalf("expected non-zero seq on published event, got %+v", e2)
	}

	unsub1()
	m.publish(id, WireEvent{Type: "text", Text: "z"})
	select {
	case ev := <-ch2:
		if ev.Text != "z" {
			t.Fatalf("ch2 got %q, want z", ev.Text)
		}
	default:
		t.Fatal("ch2 should still receive after ch1 unsub")
	}
	// ch1 must NOT receive z (it was unsubscribed). Drain "y" buffered from
	// before unsub, then assert no further events.
	select {
	case ev := <-ch1:
		if ev.Text == "z" {
			t.Fatalf("ch1 received z after unsub: %+v", ev)
		}
	default:
	}
	select {
	case ev := <-ch1:
		t.Fatalf("ch1 received after unsub: %+v", ev)
	default:
	}
}
