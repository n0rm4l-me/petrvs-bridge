package hubsvc

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func runHub(t *testing.T, c Config) *Hub {
	t.Helper()
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	names := wire.DefaultNames()
	c.Transport.(*transport.Mem).Bind(names.ToHub(), names.HubSub()) // Run returns at once if its subscription does not exist
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the hub stopped by itself: %v", err)
		}
	})
	return h
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheCommandMenuIsPublishedInTelegramsFormat(t *testing.T) {
	c := base(t)
	fake := c.Telegram.(*telegram.Fake)
	runHub(t, c)
	eventually(t, "the menu", func() bool { return len(fake.Commands()) > 0 })
	name := regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	for _, cmd := range fake.Commands() {
		if !name.MatchString(cmd.Name) || len(cmd.Description) < 3 || len(cmd.Description) > 256 {
			t.Errorf("Telegram would refuse %+v", cmd)
		}
	}
}

func TestEveryCommandInTheMenuIsUnderstood(t *testing.T) {
	c := base(t)
	fake := c.Telegram.(*telegram.Fake)
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range menu {
		h.HandleText(context.Background(), "/"+cmd.Name)
	}
	for _, s := range fake.Sent() {
		if strings.HasPrefix(s.Text, "Unknown command") {
			t.Errorf("a command in the menu is not understood: %q", s.Text)
		}
	}
	if len(fake.Sent()) == 0 {
		t.Fatal("the commands got no answer at all")
	}
}

func TestHelpAndStartListTheCommands(t *testing.T) {
	c := base(t)
	fake := c.Telegram.(*telegram.Fake)
	h, _ := New(c)
	h.HandleText(context.Background(), "/start")
	h.HandleText(context.Background(), "/help")
	for _, s := range fake.Sent() {
		if s.Text != helpText {
			t.Errorf("got %q", s.Text)
		}
	}
	if len(fake.Sent()) != 2 {
		t.Fatalf("%v", fake.Sent())
	}
}

func TestAMenuThatCannotBePublishedIsRetriedAndDoesNotStopTheHub(t *testing.T) {
	c := base(t)
	c.MenuRetry = 10 * time.Millisecond
	fake := c.Telegram.(*telegram.Fake)
	fake.SetMenuErr(errors.New("telegram setMyCommands: HTTP 500"))
	runHub(t, c)
	time.Sleep(60 * time.Millisecond)
	if len(fake.Commands()) != 0 {
		t.Fatal("the menu was recorded although publishing failed")
	}
	fake.Push(telegram.Update{ID: 1, ChatID: 1, UserID: 1, MessageID: 5, Text: "/status"})
	eventually(t, "an answer while the menu still fails", func() bool { return len(fake.Sent()) > 0 })
	fake.SetMenuErr(nil)
	eventually(t, "the menu after the failure ends", func() bool { return len(fake.Commands()) > 0 })
}

func TestTheReceivedMessageIsMarkedButAStrangersIsNot(t *testing.T) {
	c := base(t)
	c.ReceivedReaction = "👀"
	fake := c.Telegram.(*telegram.Fake)
	runHub(t, c)
	fake.Push(telegram.Update{ID: 1, ChatID: 9, UserID: 9, MessageID: 4, Text: "let me in"})
	fake.Push(telegram.Update{ID: 2, ChatID: 1, UserID: 1, MessageID: 7, Text: "/status"})
	eventually(t, "the mark", func() bool { return len(fake.Reactions()) > 0 })
	never(t, "more than one message was marked", 100*time.Millisecond, func() bool { return len(fake.Reactions()) > 1 })
	if got := fake.Reactions()[0]; got != (telegram.Reaction{ChatID: 1, MessageID: 7, Emoji: "👀"}) {
		t.Fatalf("%+v", got)
	}
}

func TestNoMarkWhenNoReactionIsConfigured(t *testing.T) {
	c := base(t)
	fake := c.Telegram.(*telegram.Fake)
	runHub(t, c)
	fake.Push(telegram.Update{ID: 1, ChatID: 1, UserID: 1, MessageID: 7, Text: "/status"})
	eventually(t, "the answer", func() bool { return len(fake.Sent()) > 0 })
	if n := len(fake.Reactions()); n != 0 {
		t.Fatalf("%d messages were marked", n)
	}
}

func TestARefusedReactionStopsTheMarksButNotTheHub(t *testing.T) {
	c := base(t)
	c.ReceivedReaction = "🫥"
	fake := c.Telegram.(*telegram.Fake)
	fake.SetReactErr(errors.New("telegram setMessageReaction: HTTP 400: Bad Request: REACTION_INVALID"))
	runHub(t, c)
	fake.Push(telegram.Update{ID: 1, ChatID: 1, UserID: 1, MessageID: 7, Text: "/status"})
	eventually(t, "the refused attempt", func() bool { return fake.ReactAttempts() >= 1 })
	fake.SetReactErr(nil) // even if Telegram would accept it now, the hub has given up on this reaction
	fake.Push(telegram.Update{ID: 2, ChatID: 1, UserID: 1, MessageID: 8, Text: "/status"})
	eventually(t, "the second answer", func() bool { return len(fake.Sent()) >= 2 })
	never(t, "a message was marked after the refusal", 100*time.Millisecond, func() bool { return len(fake.Reactions()) > 0 })
}

func TestAFailedMarkDoesNotStopLaterMarks(t *testing.T) {
	c := base(t)
	c.ReceivedReaction = "👍"
	fake := c.Telegram.(*telegram.Fake)
	fake.SetReactErr(errors.New("telegram setMessageReaction: HTTP 502: Bad Gateway"))
	runHub(t, c)
	fake.Push(telegram.Update{ID: 1, ChatID: 1, UserID: 1, MessageID: 7, Text: "/status"})
	eventually(t, "the failed attempt", func() bool { return fake.ReactAttempts() >= 1 })
	fake.SetReactErr(nil)
	fake.Push(telegram.Update{ID: 2, ChatID: 1, UserID: 1, MessageID: 8, Text: "/status"})
	eventually(t, "the mark on the second message", func() bool { return len(fake.Reactions()) == 1 })
}

func TestAStuckTelegramCannotHoldUpACommandBecauseOfTheMarks(t *testing.T) {
	c := base(t)
	c.ReceivedReaction = "👀"
	fake := c.Telegram.(*telegram.Fake)
	block := make(chan struct{})
	fake.BlockReactions(block)
	defer close(block)
	runHub(t, c)
	for i := int64(1); i <= 3*maxMarksQueued; i++ {
		fake.Push(telegram.Update{ID: i, ChatID: 1, UserID: 1, MessageID: i, Text: "/status"})
	}
	eventually(t, "every command answered although no mark finished", func() bool { return len(fake.Sent()) >= 3*maxMarksQueued })
}

func TestADeliveredReportCountsOnlyFromTheAgentThatGotTheCommand(t *testing.T) {
	c := two(t)
	c.ReceivedReaction, c.DeliveredReaction = "👀", "👍"
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	h.rememberCommand("desk", "cmd-1", 7)
	h.rememberCommand("desk", "cmd-2", 8)
	h.markDelivered("laptop", "cmd-1") // another agent cannot mark it
	h.markDelivered("desk", "unknown") // nor can an unknown command
	h.markDelivered("desk", "cmd-2")
	eventually(t, "the mark for cmd-2", func() bool { return len(fake.Reactions()) == 1 })
	never(t, "more marks than expected", 100*time.Millisecond, func() bool { return len(fake.Reactions()) > 1 })
	if got := fake.Reactions()[0]; got != (telegram.Reaction{ChatID: 1, MessageID: 8, Emoji: "👍"}) {
		t.Fatalf("%+v", got)
	}
	h.markDelivered("desk", "cmd-2") // a repeated report marks nothing again
	never(t, "a second mark for the same command", 100*time.Millisecond, func() bool { return len(fake.Reactions()) > 1 })
	h.markDelivered("desk", "cmd-1") // and cmd-1 is still waiting for the right agent
	eventually(t, "the mark for cmd-1", func() bool { return len(fake.Reactions()) == 2 })
}

func TestOnlyTheLatestCommandsWaitForTheirReport(t *testing.T) {
	c := two(t)
	c.DeliveredReaction = "👍"
	h := runHub(t, c)
	for i := 0; i < 3*maxTrackedCommands; i++ {
		h.rememberCommand("desk", strconv.Itoa(i), int64(i+1))
	}
	if len(h.cmdMsg) > maxTrackedCommands || len(h.cmdRing) > maxTrackedCommands {
		t.Fatalf("%d %d", len(h.cmdMsg), len(h.cmdRing))
	}
	if _, old := h.cmdMsg["0"]; old {
		t.Fatal("an old command is still waiting")
	}
}

func TestOneRefusedEmojiDoesNotStopTheOtherMark(t *testing.T) {
	c := base(t)
	c.ReceivedReaction, c.DeliveredReaction = "👀", "🫥"
	fake := c.Telegram.(*telegram.Fake)
	fake.RefuseEmoji("🫥")
	h := runHub(t, c)
	h.rememberCommand("laptop", "cmd-1", 7)
	h.markDelivered("laptop", "cmd-1") // refused by Telegram
	h.markReceived(9)
	eventually(t, "the read mark after a refused delivered mark", func() bool { return len(fake.Reactions()) == 1 })
	if got := fake.Reactions()[0]; got.Emoji != "👀" || got.MessageID != 9 {
		t.Fatalf("%+v", got)
	}
}
