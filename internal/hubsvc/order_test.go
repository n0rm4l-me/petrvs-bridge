package hubsvc_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// The order of the chat. A file reaches the agent seconds after the user sent it, and what the user types in the meantime
// used to be numbered first and typed first: "describe the photo I just sent" before the photo. The hub keeps a line per
// agent (agentGate): the place of a file is taken when its message arrives, and the commands typed after it wait behind it.

// ordChat makes a fetch wait until the test lets it go, fail, or panic, per file id.
type ordChat struct {
	*telegram.Fake
	mu      sync.Mutex
	gates   map[string]chan struct{}
	errs    map[string]error
	panics  map[string]bool
	started chan string
}

func (c *ordChat) hold(id string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	g := make(chan struct{})
	c.gates[id] = g
	return g
}

func (c *ordChat) failWith(id string, err error) { c.mu.Lock(); c.errs[id] = err; c.mu.Unlock() }
func (c *ordChat) panicOn(id string)             { c.mu.Lock(); c.panics[id] = true; c.mu.Unlock() }

func (c *ordChat) Fetch(ctx context.Context, ref telegram.FileRef, max int64) ([]byte, error) {
	c.started <- ref.ID
	c.mu.Lock()
	g := c.gates[ref.ID]
	c.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.mu.Lock()
	err, boom := c.errs[ref.ID], c.panics[ref.ID]
	c.mu.Unlock()
	if boom {
		panic("a bug in the client")
	}
	if err != nil {
		return nil, err
	}
	return c.Fake.Fetch(ctx, ref, max)
}

type ordRig struct {
	*testrig.Rig
	chat  *ordChat
	inbox string
}

func ordNew(t *testing.T, o testrig.Options) *ordRig {
	t.Helper()
	f := &ordRig{chat: &ordChat{gates: map[string]chan struct{}{}, errs: map[string]error{}, panics: map[string]bool{}, started: make(chan string, 64)}}
	f.inbox = t.TempDir()
	o.WrapTelegram = func(fake *telegram.Fake) telegram.Client { f.chat.Fake = fake; return f.chat }
	prev := o.Agent
	o.Agent = func(name string, c *agentsvc.Config) {
		c.Ext.InboxDir = f.inbox + "/" + name
		if prev != nil {
			prev(name, c)
		}
	}
	f.Rig = testrig.New(t, o)
	f.StartAll()
	return f
}

// photo sends a picture to the chat. It does not wait for the hub: what is said next is in the chat's queue right behind
// it, and the place of the file in line has to be taken when the hub reads the message, not when its transfer begins.
func (f *ordRig) photo(id, caption string) int64 {
	f.T.Helper()
	f.TG.Serve(id, []byte("bytes of "+id))
	return f.SayMedia(telegram.FileRef{ID: id, Photo: true, Size: int64(len("bytes of " + id))}, caption)
}

// typed is what the agent typed so far, each file line shortened to "[file:caption]".
func (f *ordRig) typed(agent string) []string {
	var out []string
	for _, line := range f.Agents[agent].Exec.Texts() {
		if strings.HasPrefix(line, "[photo from the chat") || strings.HasPrefix(line, "[file from the chat") {
			_, caption, _ := strings.Cut(line, ")] ")
			line = "[file:" + caption + "]"
		}
		out = append(out, line)
	}
	return out
}

func (f *ordRig) waitTyped(agent string, want ...string) {
	f.T.Helper()
	f.Eventually(fmt.Sprintf("%q to be typed on %s", want, agent), func() bool { return len(f.Agents[agent].Exec.Texts()) >= len(want) })
	got := f.typed(agent)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.T.Fatalf("typed %q, want %q", got, want)
	}
}

func (f *ordRig) marks(msg int64) (out []string) {
	for _, x := range f.TG.Reactions() {
		if x.MessageID == msg {
			out = append(out, x.Emoji)
		}
	}
	return out
}

func TestTextTypedAfterAPhotoIsTypedAfterIt(t *testing.T) {
	f := ordNew(t, testrig.Options{ReceivedReaction: "👀", DeliveredReaction: "👍"})
	gate := f.chat.hold("p1")
	photo := f.photo("p1", "see this")
	text := f.Say("describe the photo I just sent")
	f.Never("the text typed before the photo", 500*time.Millisecond, func() bool { return len(f.Agents["laptop"].Exec.Texts()) > 0 })
	close(gate)
	f.waitTyped("laptop", "[file:see this]", "describe the photo I just sent")
	// what the user sees is what they saw before, only later: a mark when the message is read, another when it is typed
	f.Eventually("both marks on both messages", func() bool { return len(f.marks(photo)) == 2 && len(f.marks(text)) == 2 })
	if got := f.marks(text); got[0] != "👀" || got[1] != "👍" {
		t.Fatalf("marks on the text: %v", got)
	}
	if f.Sent("⚠️") {
		t.Fatalf("%q", f.SentTexts())
	}
}

func TestTextThenPhotoThenTextKeepsTheOrderOfTheChat(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	f.Say("before")
	f.waitTyped("laptop", "before")
	gate := f.chat.hold("p1")
	f.photo("p1", "the photo")
	f.Say("after")
	f.Say("and after that")
	close(gate)
	f.waitTyped("laptop", "before", "[file:the photo]", "after", "and after that")
}

func TestTwoPhotosThenTextAreTypedInTheOrderTheyWereSent(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	slow := f.chat.hold("slow")
	f.photo("slow", "first")
	fast := f.chat.hold("fast")
	f.photo("fast", "second")
	f.Say("about both")
	close(fast) // the second photo is through long before the first
	f.Never("the second photo typed before the first", 500*time.Millisecond, func() bool { return len(f.Agents["laptop"].Exec.Texts()) > 0 })
	close(slow)
	f.waitTyped("laptop", "[file:first]", "[file:second]", "about both")
}

func TestAKeyTypedAfterAPhotoIsPressedAfterIt(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	a := f.Agents["laptop"]
	gate := f.chat.hold("p1")
	f.photo("p1", "")
	f.Say("/enter")
	f.Never("the key pressed before the photo", 400*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 })
	close(gate)
	f.Eventually("the key", func() bool { return len(a.Exec.Keys()) == 1 })
	if len(a.Exec.Texts()) != 1 { // read after the key: the line about the file was typed first
		t.Fatalf("the key was pressed before the file was typed: %q", a.Exec.Texts())
	}
}

func TestAFailingTransferSendsOnWhatWaitedBehindIt(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	gate := f.chat.hold("bad")
	f.chat.failWith("bad", fmt.Errorf("telegram is down"))
	f.photo("bad", "")
	f.Say("still wanted")
	close(gate)
	f.WaitSent("Could not fetch the file")
	f.waitTyped("laptop", "still wanted")
	// and the line is open again for the next file
	gate2 := f.chat.hold("good")
	f.photo("good", "next")
	f.Say("then this")
	close(gate2)
	f.waitTyped("laptop", "still wanted", "[file:next]", "then this")
}

func TestAPanicInATransferDoesNotLoseWhatWaitedBehindItOrStopTheHub(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	gate := f.chat.hold("boom")
	f.chat.panicOn("boom")
	f.photo("boom", "")
	f.Say("do not lose me")
	close(gate)
	f.waitTyped("laptop", "do not lose me")
	f.WaitSent("because of an error in the hub")
	f.Say("the hub is alive")
	f.waitTyped("laptop", "do not lose me", "the hub is alive")
}

func TestLockdownIsImmediateAndDropsWhatWaitedBehindAFile(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	a := f.Agents["laptop"]
	gate := f.chat.hold("p1")
	f.photo("p1", "")
	f.Say("typed before the lockdown")
	f.Say("/key 1")
	f.Say("/lockdown") // the fetch is still waiting: the lockdown does not
	f.WaitSent("Lockdown: the hub stopped relaying")
	f.WaitSent("2 message(s) that waited for a file were discarded")
	close(gate)
	f.WaitSent("Lockdown is on")
	f.Never("something typed after the lockdown", 600*time.Millisecond, func() bool { return len(a.Exec.Texts()) > 0 || len(a.Exec.Keys()) > 0 })
	// after /unlock the line works for new commands again (the agent itself stays locked)
	f.Say("/unlock")
	f.WaitSent("The hub relays again")
	f.Say("after the unlock")
	f.WaitSent("refused: this machine is locked")
}

func TestACommandSentWhileTheHubIsLockedIsNotHeldBehindAFile(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	gate := f.chat.hold("p1")
	f.photo("p1", "")
	f.Say("/lockdown")
	f.WaitSent("Lockdown:")
	f.Say("typed during the lockdown")
	f.WaitSent("Lockdown is on: nothing is sent")
	close(gate)
}

func TestAFileForOneAgentDoesNotHoldCommandsForAnother(t *testing.T) {
	f := ordNew(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	f.Say("/agent laptop")
	f.WaitSent("Active agent: laptop")
	gate := f.chat.hold("p1")
	f.photo("p1", "for the laptop")
	f.Say("@nas hello nas")
	f.waitTyped("nas", "hello nas") // not held
	f.Say("@laptop hello laptop")
	f.Never("the laptop's text typed before its file", 400*time.Millisecond, func() bool { return len(f.Agents["laptop"].Exec.Texts()) > 0 })
	close(gate)
	f.waitTyped("laptop", "[file:for the laptop]", "hello laptop")
}

func TestOnlyWhatFitsIsHeldAndTheRestIsRefusedInWords(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	gate := f.chat.hold("p1")
	f.photo("p1", "")
	const sent = 40
	for i := 0; i < sent; i++ {
		f.Say(fmt.Sprintf("m%02d", i))
	}
	f.Eventually("the refusals", func() bool { return count(f.Rig, "Too many messages are waiting") >= sent-31 })
	close(gate)
	want := []string{"[file:]"}
	for i := 0; i < 31; i++ { // the place of the file and 31 commands fill the line of 32
		want = append(want, fmt.Sprintf("m%02d", i))
	}
	f.waitTyped("laptop", want...)
	if n := count(f.Rig, "Too many messages are waiting"); n != sent-31 {
		t.Fatalf("%d refusals, want %d", n, sent-31)
	}
	f.Never("a refused message typed", 300*time.Millisecond, func() bool { return len(f.Agents["laptop"].Exec.Texts()) > len(want) })
}

// A command that arrives while the line is being sent on joins the end of it: it must not be numbered ahead of the ones
// that are still waiting to go.
func TestACommandThatArrivesWhileTheLineIsBeingSentOnJoinsTheEnd(t *testing.T) {
	f := ordNew(t, testrig.Options{FaultyBus: true, Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour})
	toAgent := f.Names.ToAgent("laptop")
	gate := f.chat.hold("p1")
	f.photo("p1", "")
	for i := 0; i < 5; i++ {
		f.Say(fmt.Sprintf("queued %d", i))
	}
	time.Sleep(200 * time.Millisecond) // the hub has read them all
	var once sync.Once
	entered, proceed := make(chan struct{}), make(chan struct{})
	var letGo sync.Once
	release := func() { letGo.Do(func() { close(proceed) }) }
	t.Cleanup(release) // a test that fails must not leave the publish stuck
	f.Faulty.AfterPublish(func(topic string) {
		if topic == toAgent {
			once.Do(func() { close(entered); <-proceed }) // the first command of the line is out; hold the run here
		}
	})
	close(gate)
	select {
	case <-entered:
	case <-time.After(f.Timeout()):
		t.Fatal("the line was never sent on")
	}
	f.Hub.HandleText(context.Background(), "late") // a command typed in the middle of the run
	release()
	f.waitTyped("laptop", "[file:]", "queued 0", "queued 1", "queued 2", "queued 3", "queued 4", "late")
}

func TestTheHubStoppingDuringATransferTellsTheChatWhatWaitedBehindIt(t *testing.T) {
	f := ordNew(t, testrig.Options{})
	f.chat.hold("p1") // never let go: the stop ends the fetch
	f.photo("p1", "")
	f.Say("one")
	f.Say("two")
	time.Sleep(200 * time.Millisecond)
	f.StopHub()
	f.WaitSent("The hub was stopped while it fetched your file")
	f.WaitSent("2 message(s) typed after it were not sent")
	if n := len(f.Agents["laptop"].Exec.Texts()); n != 0 {
		t.Fatalf("%d texts typed by a stopped hub", n)
	}
}
