package hubsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/handshake"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func TestAHubLockFileThatCannotBeCheckedMeansLocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can look inside any directory")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "hub-lock")
	locked := func() (bool, string) {
		c := base(t)
		buf := &lockedBuf{}
		c.Log = slog.New(slog.NewTextHandler(buf, nil))
		c.LockFile = lock
		h, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.locked, buf.String()
	}
	if l, _ := locked(); l {
		t.Fatal("a missing lock file must not lock the hub")
	}
	if err := os.WriteFile(lock, []byte("lockdown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, _ := locked(); !l {
		t.Fatal("an existing lock file must lock the hub")
	}
	os.Remove(lock)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	l, log := locked()
	if !l {
		t.Fatal("a lock file that cannot be checked must lock the hub, as it does for the agent")
	}
	if !strings.Contains(log, "could not check the hub's lock file") {
		t.Fatalf("no warning: %s", log)
	}
}

func TestACutReplyNeverEndsInTheMiddleOfACharacter(t *testing.T) {
	for name, tc := range map[string]struct {
		text  string
		limit int
		want  string
	}{
		"cyrillic, limit inside a letter": {"приветприветпривет", 11, "приве\n[cut: 26 more bytes]"},
		"cyrillic, limit on a boundary":   {"приветприветпривет", 12, "привет\n[cut: 24 more bytes]"},
		"emoji, limit inside":             {"ab🙂🙂", 5, "ab\n[cut: 8 more bytes]"},
		"ascii":                           {"abcdefgh", 3, "abc\n[cut: 5 more bytes]"},
		"fits":                            {"привет", 12, "привет"},
	} {
		c := base(t)
		c.MaxReplyBytes = tc.limit
		fake := c.Telegram.(*telegram.Fake)
		h, _ := New(c)
		if _, err := h.send(context.Background(), telegram.Message{Text: tc.text}); err != nil {
			t.Fatal(err)
		}
		got := fake.Sent()[0].Text
		if !utf8.ValidString(got) || got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// Replies to messages the hub does not know. With one agent there is nothing to confuse; with several the author could
// be any of them, so nothing is typed.
func TestAnAnswerToAMessageTheHubForgotIsNotGuessedWithSeveralAgents(t *testing.T) {
	c := two(t)
	c.DefaultAgent = "laptop"
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	h.track("desk", []int64{500})
	lastSaid := func() string {
		s := fake.Sent()
		if len(s) == 0 {
			return ""
		}
		return s[len(s)-1].Text
	}
	push := func(id int64, text string, replyTo int64) {
		fake.Push(telegram.Update{ID: id, ChatID: 1, UserID: 1, MessageID: 9000 + id, Text: text, ReplyToID: replyTo})
	}
	said := func(sub string) bool {
		for _, s := range fake.Sent() {
			if strings.Contains(s.Text, sub) {
				return true
			}
		}
		return false
	}

	// known: goes to the agent that wrote it (not connected here, which is how the route shows)
	push(1, "yes", 500)
	eventually(t, "the known answer to reach desk", func() bool { return said("desk is not connected") })
	if said("laptop is not connected") || said("no longer knows") {
		t.Fatalf("%+v", fake.Sent())
	}

	// unknown: nothing is sent to anyone, and the user is told what to do
	n := len(fake.Sent())
	push(2, "yes", 777)
	eventually(t, "the refusal", func() bool { return said("no longer knows which agent") })
	push(3, "/enter", 777)
	eventually(t, "the refusal for a key", func() bool { return len(fake.Sent()) >= n+2 })
	if said("laptop is not connected") || h.Stats().Relayed != 0 {
		t.Fatalf("the hub guessed: %+v", fake.Sent())
	}
	if !strings.Contains(lastSaid(), "@name") || !strings.Contains(lastSaid(), "/agent") {
		t.Fatalf("the user is not told what to do: %q", lastSaid())
	}

	// not a reply: the active agent, as before; @name is not affected by the lookup either
	push(4, "plain", 0)
	eventually(t, "plain text to the active agent", func() bool { return said("laptop is not connected") })
	push(5, "@desk hello", 777)
	eventually(t, "@name to win over the reply", func() bool { return len(fake.Sent()) >= n+4 && strings.Contains(lastSaid(), "desk is not connected") })
}

func TestAnAnswerToAMessageTheHubForgotGoesToTheOnlyAgent(t *testing.T) {
	c := base(t)
	fake := c.Telegram.(*telegram.Fake)
	runHub(t, c)
	fake.Push(telegram.Update{ID: 1, ChatID: 1, UserID: 1, MessageID: 9001, Text: "yes", ReplyToID: 777})
	eventually(t, "the text to be routed", func() bool {
		s := fake.Sent()
		return len(s) == 1 && strings.Contains(s[0].Text, "laptop is not connected")
	})
}

func TestAPollingErrorThatNamesAWaitIsWaitedOut(t *testing.T) {
	c := base(t)
	poll := &limitedPolls{Fake: c.Telegram.(*telegram.Fake), after: 1500 * time.Millisecond}
	c.Telegram = poll
	runHub(t, c)
	eventually(t, "the second poll", func() bool { return len(poll.times()) >= 2 })
	// the hub's own back-off would have been one second
	if gap := poll.times()[1].Sub(poll.times()[0]); gap < 1400*time.Millisecond {
		t.Fatalf("polled again after %v although Telegram asked for 1.5s", gap)
	}
}

type limitedPolls struct {
	*telegram.Fake
	after time.Duration
	mu    sync.Mutex
	at    []time.Time
}

func (l *limitedPolls) times() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.at...)
}

func (l *limitedPolls) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]telegram.Update, error) {
	l.mu.Lock()
	l.at = append(l.at, time.Now())
	first := len(l.at) == 1
	l.mu.Unlock()
	if first {
		return nil, fmt.Errorf("polling: %w", telegram.ErrRateLimited{After: l.after})
	}
	return l.Fake.GetUpdates(ctx, offset, timeout)
}

// failAcks is a bus that refuses the first n messages published to one topic.
type failAcks struct {
	*transport.Mem
	topic string
	mu    sync.Mutex
	left  int
}

func (f *failAcks) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	f.mu.Lock()
	fail := topic == f.topic && f.left != 0
	if fail && f.left > 0 {
		f.left--
	}
	f.mu.Unlock()
	if fail {
		return errors.New("bus down")
	}
	return f.Mem.Publish(ctx, topic, data, attrs)
}

// helloRig is a hub whose bus can refuse the ack, and the agent side of a handshake done by hand.
type helloRig struct {
	hub          *Hub
	bus          *failAcks
	hubID, agent *identity.Identity
	names        wire.Names
	acks         chan []byte
}

func newHelloRig(t *testing.T, failures int) *helloRig {
	t.Helper()
	hubID, _ := identity.Generate("hub")
	agentID, _ := identity.Generate("laptop")
	mem := transport.NewMem()
	names := wire.DefaultNames()
	mem.Bind(names.ToHub(), names.HubSub())
	mem.Bind(names.ToAgent("laptop"), names.AgentSub("laptop"))
	bus := &failAcks{Mem: mem, topic: names.ToAgent("laptop"), left: failures}
	h, err := New(Config{Identity: hubID, Agents: []identity.Public{agentID.Public()}, Transport: bus, Telegram: telegram.NewFake(), ChatID: 1, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { h.Run(ctx); done <- struct{}{} }()
	rig := &helloRig{hub: h, bus: bus, hubID: hubID, agent: agentID, names: names, acks: make(chan []byte, 8)}
	go func() {
		mem.Receive(ctx, names.AgentSub("laptop"), func(_ context.Context, m *transport.Message) {
			m.Ack()
			if m.Attrs[wire.AttrKind] == wire.KindAck {
				rig.acks <- m.Data
			}
		})
		done <- struct{}{}
	}()
	t.Cleanup(func() { cancel(); <-done; <-done })
	rig.hello(t)
	return rig
}

// hello sends one hello from the agent.
func (r *helloRig) hello(t *testing.T) {
	t.Helper()
	_, msg, err := handshake.NewInitiator(r.agent, r.hubID.Public(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, err := msg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.bus.Mem.Publish(context.Background(), r.names.ToHub(), data, wire.Attrs(wire.KindHello, "laptop")); err != nil {
		t.Fatal(err)
	}
}

func (r *helloRig) pending() (n int, reconnecting bool) {
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	st := r.hub.agents["laptop"]
	return len(st.pending), st.reconnecting(time.Now())
}

func TestAHelloWhoseAckCouldNotBePublishedIsAnsweredWhenTheBusDeliversItAgain(t *testing.T) {
	r := newHelloRig(t, 1)
	select {
	case <-r.acks:
	case <-time.After(3 * time.Second):
		s := r.hub.Stats()
		t.Fatalf("the redelivered hello was never answered (hellos %d, refused %d)", s.Hellos, s.HellosRefused)
	}
	// The hub counts the hello after the publish of the ack has returned, and the test sees the ack inside that publish: wait for
	// the count instead of reading it once.
	for end := time.Now().Add(3 * time.Second); r.hub.Stats().Hellos == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	if s := r.hub.Stats(); s.Hellos != 1 || s.HellosRefused != 0 {
		t.Fatalf("%+v", s)
	}
	if n, _ := r.pending(); n != 1 {
		t.Fatalf("%d sessions offered", n)
	}
}

func TestAHelloThatCannotBeAnsweredLeavesNoSessionBehind(t *testing.T) {
	r := newHelloRig(t, -1)            // the bus never takes the ack
	time.Sleep(500 * time.Millisecond) // the bus delivers the hello again a few times, one after the other
	if n, reconnecting := r.pending(); n != 0 || reconnecting {
		t.Fatalf("an unanswered hello left %d offered sessions, reconnecting=%v: the hub would refuse commands for 15s", n, reconnecting)
	}
}
