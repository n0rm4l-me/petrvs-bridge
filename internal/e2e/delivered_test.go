package e2e

import (
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

func marks(r *testrig.Rig, msg int64) []string {
	var out []string
	for _, x := range r.TG.Reactions() {
		if x.MessageID == msg {
			out = append(out, x.Emoji)
		}
	}
	return out
}

func markedRig(t *testing.T) (*testrig.Rig, *testrig.AgentRig) {
	return one(t, testrig.Options{ReceivedReaction: "👀", DeliveredReaction: "👍"})
}

func TestAMessageIsMarkedReadAndThenDeliveredWhenTheAgentTypedIt(t *testing.T) {
	r, a := markedRig(t)
	msg := r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.Eventually("both marks", func() bool { return len(marks(r, msg)) == 2 })
	if got := marks(r, msg); got[0] != "👀" || got[1] != "👍" {
		t.Fatalf("marks in the wrong order: %v", got)
	}
	// a key press is delivered the same way
	key := r.Say("/enter")
	r.Eventually("pressed", func() bool { return len(a.Exec.Keys()) == 1 })
	r.Eventually("both marks on the key", func() bool { return len(marks(r, key)) == 2 })
}

func TestAMessageTheAgentRefusedStaysMarkedOnlyAsRead(t *testing.T) {
	r, a := markedRig(t)
	r.Say("/lockdown")
	r.WaitSent("Lockdown")
	r.Eventually("the agent is locked", func() bool { return a.Agent.Locked() })
	r.Say("/unlock") // the hub relays again, the agent stays locked
	r.WaitSent("The hub relays again")
	msg := r.Say("please type this")
	r.WaitSent("refus") // the agent says why it did not type it
	r.Never("a delivered mark on a refused command", 500*time.Millisecond, func() bool { return len(marks(r, msg)) > 1 })
	if got := marks(r, msg); len(got) != 1 || got[0] != "👀" {
		t.Fatalf("%v", got)
	}
	if len(a.Exec.Texts()) != 0 {
		t.Fatal("a locked agent typed the text")
	}
}

func TestWithoutADeliveredReactionTheReadMarkStays(t *testing.T) {
	r, a := one(t, testrig.Options{ReceivedReaction: "👀"})
	msg := r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.Eventually("the read mark", func() bool { return len(marks(r, msg)) == 1 })
	r.Never("a second mark", 300*time.Millisecond, func() bool { return len(marks(r, msg)) > 1 })
}

func TestTheDeliveredMarkDoesNotNeedTheReadMark(t *testing.T) {
	r, a := one(t, testrig.Options{DeliveredReaction: "👍"})
	msg := r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.Eventually("the delivered mark", func() bool { return len(marks(r, msg)) == 1 })
	if got := marks(r, msg); got[0] != "👍" {
		t.Fatalf("%v", got)
	}
}
