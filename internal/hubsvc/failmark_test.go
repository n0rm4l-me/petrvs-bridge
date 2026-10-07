package hubsvc

import (
	"context"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The mark on a message comes from the machine (the agent's report), and the hub marks only a failure: a command that could
// not be sent, or that the agent could not carry out.

func marks(f *telegram.Fake) []telegram.Reaction { return f.Reactions() }

func TestACommandThatCannotBeSentIsMarkedAsFailed(t *testing.T) {
	c := base(t) // the agent has not connected: there is no session to send through
	c.DeliveredReaction, c.FailedReaction = "👀", "👎"
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	if h.deliverWith(context.Background(), "laptop", wire.TypeText, "hello", nil, time.Minute, time.Time{}, 41) {
		t.Fatal("a command was sent with no session")
	}
	eventually(t, "the failure mark", func() bool { return len(marks(fake)) == 1 })
	if got := marks(fake)[0]; got != (telegram.Reaction{ChatID: 1, MessageID: 41, Emoji: "👎"}) {
		t.Fatalf("%+v", got)
	}
	never(t, "another mark", 100*time.Millisecond, func() bool { return len(marks(fake)) > 1 })
}

func TestAStaleCommandIsMarkedAsFailedToo(t *testing.T) {
	c := base(t)
	c.FailedReaction = "👎"
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	h.deliverWith(context.Background(), "laptop", wire.TypeText, "old", nil, time.Minute, time.Now().Add(-time.Hour), 42)
	eventually(t, "the failure mark", func() bool { return len(marks(fake)) == 1 })
	if got := marks(fake)[0]; got.MessageID != 42 || got.Emoji != "👎" {
		t.Fatalf("%+v", got)
	}
}

func TestNothingIsMarkedWhenNoFailureMarkIsConfigured(t *testing.T) {
	c := base(t)
	c.DeliveredReaction = "👀"
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	h.deliverWith(context.Background(), "laptop", wire.TypeText, "hello", nil, time.Minute, time.Time{}, 43)
	never(t, "a mark", 150*time.Millisecond, func() bool { return len(marks(fake)) > 0 })
}

func TestTheAgentsReportOfAFailureMarksTheMessageOfThatCommand(t *testing.T) {
	c := two(t)
	c.DeliveredReaction, c.FailedReaction = "👀", "👎"
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	h.rememberCommand("desk", "cmd-1", 7)
	h.rememberCommand("desk", "cmd-2", 8)
	h.markFailedCommand("laptop", "cmd-1") // another agent cannot mark it
	h.markFailedCommand("desk", "")        // a report without a command (an older agent) marks nothing
	h.markFailedCommand("desk", "unknown")
	never(t, "a mark from a report that does not count", 100*time.Millisecond, func() bool { return len(marks(fake)) > 0 })
	h.markFailedCommand("desk", "cmd-1")
	eventually(t, "the failure mark", func() bool { return len(marks(fake)) == 1 })
	if got := marks(fake)[0]; got != (telegram.Reaction{ChatID: 1, MessageID: 7, Emoji: "👎"}) {
		t.Fatalf("%+v", got)
	}
	// The command that failed is not delivered afterwards, and cmd-2 is still waiting for its own report.
	h.markDelivered("desk", "cmd-1")
	h.markDelivered("desk", "cmd-2")
	eventually(t, "the delivered mark of cmd-2", func() bool { return len(marks(fake)) == 2 })
	if got := marks(fake)[1]; got != (telegram.Reaction{ChatID: 1, MessageID: 8, Emoji: "👀"}) {
		t.Fatalf("%+v", got)
	}
}

// The default is one mark, put when the machine has the message; the hub does not mark on reading.
func TestTheHubDoesNotMarkOnReadingWhenOnlyTheAgentsMarkIsConfigured(t *testing.T) {
	c := base(t)
	c.DeliveredReaction = "👀" // ReceivedReaction stays empty, as it does by default
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	h.markReceived(9)
	never(t, "a mark on reading", 150*time.Millisecond, func() bool { return len(marks(fake)) > 0 })
}
