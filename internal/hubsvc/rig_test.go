package hubsvc_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func count(r *testrig.Rig, sub string) int {
	n := 0
	for _, s := range r.SentTexts() {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func indexOf(r *testrig.Rig, sub string) int {
	for i, s := range r.SentTexts() {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}

func TestALockdownThatDidNotGetThroughIsNotReportedAsSentAndIsRepeated(t *testing.T) {
	r := testrig.New(t, testrig.Options{FaultyBus: true, SilenceLimit: time.Hour, // the agent must not start a new handshake meanwhile
		Hub: func(c *hubsvc.Config) { c.LockdownResend = 150 * time.Millisecond }})
	r.StartAll()
	a := r.Agents["laptop"]
	toAgent := r.Names.ToAgent("laptop")
	r.Faulty.FailPublish(func(topic string) error {
		if topic == toAgent {
			return errors.New("bus down")
		}
		return nil
	})
	r.Say("/lockdown")
	r.WaitSent("Lockdown:")
	said := r.SentTexts()[indexOf(r, "Lockdown:")]
	if !strings.Contains(said, "told 0 agent(s)") || !strings.Contains(said, "Could not reach laptop") {
		t.Fatalf("the chat was told the lockdown was sent: %q", said)
	}
	r.Never("the agent locked although nothing reached it", 300*time.Millisecond, a.Agent.Locked)

	// The bus works again. The agent's next heartbeat says it is not locked, and the hub, still locked, says it again.
	r.Faulty.FailPublish(nil)
	r.Eventually("the hub to repeat the lockdown to the agent that reports itself unlocked", a.Agent.Locked)
	r.Say("/unlock") // the hub relays again, the agent must stay locked
	r.WaitSent("The hub relays again")
	r.Say("type this")
	r.WaitSent("refused: this machine is locked")
	if len(a.Exec.Texts()) != 0 {
		t.Fatal("an agent that was ordered to lock typed the text after a hub /unlock")
	}
}

func TestTheOfflineNoticeIsSaidAfterAChatOutage(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	r.TG.SetSendErr(errors.New("telegram is down"))
	r.StopAgent("laptop")
	time.Sleep(900 * time.Millisecond) // past the offline threshold (500ms); the hub tries and fails
	if r.Sent("went offline") {
		t.Fatal("the notice reached a chat that was down")
	}
	r.TG.SetSendErr(nil)
	r.WaitSent("went offline")
}

func TestACommandAfterAFailedPublishStillRunsInTime(t *testing.T) {
	r := testrig.New(t, testrig.Options{FaultyBus: true, Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour}) // quiet bus: only commands go to the agent
	r.StartAll()
	a := r.Agents["laptop"]
	toAgent := r.Names.ToAgent("laptop")
	var failed atomic.Bool
	r.Faulty.FailPublish(func(topic string) error {
		if topic == toAgent && failed.CompareAndSwap(false, true) {
			return errors.New("bus hiccup")
		}
		return nil
	})
	start := time.Now()
	r.Say("first")
	r.Say("second")
	r.Eventually("both commands typed", func() bool { return len(a.Exec.Texts()) == 2 })
	// the agent waits 2s for a missing number; a command that needed a second try must not cost that
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("took %v", took)
	}
	if got := a.Exec.Texts(); got[0] != "first" || got[1] != "second" {
		t.Fatalf("%v", got)
	}
	if r.Sent("did not arrive") || r.Sent("did not accept") {
		t.Fatalf("the user was told about a problem: %q", r.SentTexts())
	}
}

func TestACommandThatCouldNotBePreparedGivesItsNumberBack(t *testing.T) {
	r := testrig.New(t, testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour})
	r.StartAll()
	a := r.Agents["laptop"]
	r.Hub.HandleText(context.Background(), strings.Repeat("a", 2<<20)) // more than one message can carry
	r.WaitSent("Could not prepare")
	start := time.Now()
	r.Say("ok")
	r.Eventually("the next command typed", func() bool { return len(a.Exec.Texts()) == 1 })
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("the agent waited for a number that was never sent: %v", took)
	}
	if r.Sent("did not arrive") {
		t.Fatalf("%q", r.SentTexts())
	}
}

// slowChat holds the hub's long polls back while a gate is set, and makes the first "Active agent" message wait for the
// hub to be stopped, as a Telegram that answers slowly does.
type slowChat struct {
	*telegram.Fake
	mu      sync.Mutex
	gate    chan struct{}
	stuck   atomic.Bool
	entered chan struct{}
}

func (s *slowChat) Hold() chan struct{} {
	g := make(chan struct{})
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
	return g
}

func (s *slowChat) Release(g chan struct{}) {
	s.mu.Lock()
	s.gate = nil
	s.mu.Unlock()
	close(g)
}

func (s *slowChat) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]telegram.Update, error) {
	s.mu.Lock()
	g := s.gate
	s.mu.Unlock()
	if g != nil && timeout > 0 { // the hub's confirming poll (no waiting) is not held
		select {
		case <-g:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Fake.GetUpdates(ctx, offset, timeout)
}

func (s *slowChat) SendMessage(ctx context.Context, chatID int64, m telegram.Message) ([]int64, error) {
	if strings.HasPrefix(m.Text, "Active agent") && s.stuck.CompareAndSwap(false, true) {
		close(s.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Fake.SendMessage(ctx, chatID, m)
}

func TestAStoppedHubConfirmsWhatItHandledSoTheNextOneDoesNotTypeItAgain(t *testing.T) {
	var chat *slowChat
	r := testrig.New(t, testrig.Options{WrapTelegram: func(f *telegram.Fake) telegram.Client {
		chat = &slowChat{Fake: f, entered: make(chan struct{})}
		return chat
	}})
	r.StartAll()
	a := r.Agents["laptop"]

	// One batch: a command, then a message whose answer to the chat is slow.
	gate := chat.Hold()
	time.Sleep(300 * time.Millisecond) // the poll in flight returns and the next one is held
	r.Say("type me once")
	r.Say("/agent")
	chat.Release(gate)
	r.Eventually("the command typed", func() bool { return len(a.Exec.Texts()) == 1 })
	select {
	case <-chat.entered:
	case <-time.After(r.Timeout()):
		t.Fatal("the second update was never handled")
	}

	// Stop the hub in the middle of that, and start another on the same chat. Its first poll waits until the agent is
	// connected to it, so that a replayed command would be typed.
	gate = chat.Hold()
	r.StartHub()
	r.WaitConnected("laptop")
	chat.Release(gate)
	r.Never("the command was typed again by the next hub", time.Second, func() bool { return len(a.Exec.Texts()) > 1 })
	if r.Sent("is not connected") {
		t.Fatal("the next hub was handed the old batch again")
	}
}

// gatedChat holds every message that contains a word until Release, as a Telegram that is slow or waiting out a 429.
type gatedChat struct {
	*telegram.Fake
	word  string
	gate  chan struct{}
	mu    sync.Mutex
	calls int
}

func (g *gatedChat) Calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *gatedChat) SendMessage(ctx context.Context, chatID int64, m telegram.Message) ([]int64, error) {
	if strings.Contains(m.Text, g.word) {
		g.mu.Lock()
		g.calls++
		g.mu.Unlock()
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Fake.SendMessage(ctx, chatID, m)
}

func TestAResendOfAReplyThatIsStillBeingSentIsNotShownTwice(t *testing.T) {
	var chat *gatedChat
	r := testrig.New(t, testrig.Options{FaultyBus: true, WrapTelegram: func(f *telegram.Fake) telegram.Client {
		chat = &gatedChat{Fake: f, word: "the answer", gate: make(chan struct{})}
		return chat
	}})
	r.Faulty.Concurrent = true // like Pub/Sub: a resend is handled while the first copy is still being written
	r.StartAll()
	a := r.Agents["laptop"]
	a.Spool.Add(wire.TypeReply, "the answer")
	r.Eventually("the reply to reach the chat client", func() bool { return chat.Calls() >= 1 })
	time.Sleep(time.Second) // the agent sends it again every 300ms while it has no receipt
	if n := chat.Calls(); n != 1 {
		t.Fatalf("the resend was written to the chat too: %d sends", n)
	}
	close(chat.gate)
	r.Eventually("the reply shown", func() bool { return count(r, "the answer") >= 1 })
	r.Eventually("the receipt to reach the agent", func() bool { return a.Spool.Len() == 0 })
	r.Never("the reply shown twice", 500*time.Millisecond, func() bool { return count(r, "the answer") > 1 })
}

func TestAReplyTelegramRefusesIsDroppedButOneItMayTakeLaterIsKept(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	a.Spool.Add(wire.TypeReply, "my reply")
	for name, err := range map[string]error{
		"server error": &telegram.APIError{Method: "sendMessage", Code: 500, Desc: "Internal Server Error"},
		"forbidden":    &telegram.APIError{Method: "sendMessage", Code: 403, Desc: "Forbidden: bot was blocked by the user"},
		"rate limited": telegram.ErrRateLimited{After: time.Second},
		"no network":   errors.New("telegram sendMessage: connection reset"),
	} {
		r.TG.SetSendErr(err)
		r.Never("a receipt although the chat may take the reply later ("+name+")", 500*time.Millisecond, func() bool { return a.Spool.Len() == 0 })
	}
	r.TG.SetSendErr(&telegram.APIError{Method: "sendMessage", Code: 400, Desc: "Bad Request: message text is empty"})
	r.Eventually("the agent to be told to forget a reply that can never be shown", func() bool { return a.Spool.Len() == 0 })
	r.TG.SetSendErr(nil)
	r.Never("the refused reply shown after all", 300*time.Millisecond, func() bool { return r.Sent("my reply") })
}

func TestAnAgentThatSpeaksAfterBeingOfflineIsWelcomedBackWhateverItSays(t *testing.T) {
	r := testrig.New(t, testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour}) // nothing but what the test adds
	r.StartAll()
	a := r.Agents["laptop"]
	r.WaitSent("went offline")
	a.Spool.Add(wire.TypeReply, "late reply")
	r.WaitSent("late reply")
	back, reply := indexOf(r, "is back"), indexOf(r, "late reply")
	if back < 0 || back > reply {
		t.Fatalf("the chat was not told the agent is back before its reply: %q", r.SentTexts())
	}
	if count(r, "is back") != 1 {
		t.Fatalf("%q", r.SentTexts())
	}
}

func TestTheDeliveredMarkIsKeptWhenTheAgentAnswersBeforePublishReturns(t *testing.T) {
	r := testrig.New(t, testrig.Options{FaultyBus: true, Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour, ReceivedReaction: "👀", DeliveredReaction: "👍"})
	r.Faulty.Concurrent = true
	r.StartAll()
	a := r.Agents["laptop"]
	toAgent := r.Names.ToAgent("laptop")
	r.Faulty.AfterPublish(func(topic string) { // a slow acknowledgement from the bus
		if topic == toAgent {
			time.Sleep(400 * time.Millisecond)
		}
	})
	msg := r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	marks := func() (out []string) {
		for _, x := range r.TG.Reactions() {
			if x.MessageID == msg {
				out = append(out, x.Emoji)
			}
		}
		return out
	}
	r.Eventually("both marks", func() bool { return len(marks()) == 2 })
	if got := marks(); got[0] != "👀" || got[1] != "👍" {
		t.Fatalf("%v", got)
	}
}
