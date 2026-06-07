package agentruntime

import (
	"testing"

	"github.com/sausheong/harness/session"
)

func TestApplyEntries_RebuildsSession(t *testing.T) {
	sess := session.NewSession("a", "k")
	turn1 := []session.SessionEntry{
		session.UserMessageEntry("hi"),
		session.AssistantMessageEntry("hello"),
	}
	applyEntries(sess, turn1)
	if got := len(sess.Entries()); got != 2 {
		t.Fatalf("after apply, entries = %d, want 2", got)
	}
}

func TestPublishableEvents_FromEntries(t *testing.T) {
	entries := []session.SessionEntry{
		session.UserMessageEntry("hi"),
		session.AssistantMessageEntry("the answer"),
	}
	evs := publishableEvents(entries)
	if len(evs) != 1 || evs[0].Type != "text" {
		t.Fatalf("events = %+v, want one text event", evs)
	}
	if evs[0].Text != "the answer" {
		t.Fatalf("text = %q", evs[0].Text)
	}
}
