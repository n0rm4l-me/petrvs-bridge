package hubsvc_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// What the hub does with a photo or document from the chat, seen from the chat and from the agent's inbox.

// fdChat counts the fetches the hub makes and can hand back whatever bytes a test likes.
type fdChat struct {
	*telegram.Fake
	fetches atomic.Int32
	bytes   func(telegram.FileRef) []byte
	err     error // what Fetch fails with, when set
}

func (c *fdChat) Fetch(ctx context.Context, ref telegram.FileRef, max int64) ([]byte, error) {
	c.fetches.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	if c.bytes != nil {
		return c.bytes(ref), nil
	}
	return c.Fake.Fetch(ctx, ref, max)
}

// fdRig is a rig with an inbox for the agent and a chat client that counts fetches.
type fdRig struct {
	*testrig.Rig
	chat  *fdChat
	inbox string
	gate  *fdGate
}

func fdNew(t *testing.T, o testrig.Options) *fdRig {
	t.Helper()
	f := &fdRig{chat: &fdChat{}, inbox: filepath.Join(t.TempDir(), "inbox")}
	o.WrapTelegram = func(fake *telegram.Fake) telegram.Client { f.chat.Fake = fake; return f.chat }
	prev := o.Agent
	o.Agent = func(name string, c *agentsvc.Config) {
		c.Ext.InboxDir = f.inbox
		if prev != nil {
			prev(name, c)
		}
	}
	f.Rig = testrig.New(t, o)
	return f
}

// fdQuiet are the options of a rig in which only the transfer talks on the bus.
func fdQuiet() testrig.Options {
	return testrig.Options{FaultyBus: true, Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour}
}

// fdGate holds the agent's answers back, to see what the hub does while it waits for them: each answer takes a token,
// and open lets all through. It counts what the hub sends to the agent.
type fdGate struct {
	tokens chan struct{}
	done   chan struct{}
	once   sync.Once
	gating atomic.Bool
	sent   atomic.Int64
}

func (f *fdRig) fdGate() *fdGate {
	g := &fdGate{tokens: make(chan struct{}, 1000), done: make(chan struct{})}
	toAgent, toHub := f.Names.ToAgent("laptop"), f.Names.ToHub()
	f.Faulty.FailPublish(func(topic string) error {
		switch topic {
		case toAgent:
			g.sent.Add(1)
		case toHub:
			if g.gating.Load() {
				select {
				case <-g.tokens:
				case <-g.done:
				}
			}
		}
		return nil
	})
	f.T.Cleanup(g.open) // runs before the rig stops, so that nothing is left waiting
	f.gate = g
	return g
}

func (g *fdGate) open() { g.once.Do(func() { close(g.done) }) }
func (g *fdGate) allow(n int) {
	for i := 0; i < n; i++ {
		g.tokens <- struct{}{}
	}
}

func fdBytes(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13+i>>9) ^ salt
	}
	return b
}

func fdTypedLine(t *testing.T, f *fdRig, n int) (path, caption string, data []byte) {
	t.Helper()
	a := f.Agents["laptop"]
	f.Eventually("the line about the file", func() bool { return len(a.Exec.Texts()) >= n })
	m := regexp.MustCompile(`^\[(photo|file) from the chat, saved at (/\S+) \([^)]*\)\]( .*)?$`).FindStringSubmatch(a.Exec.Texts()[n-1])
	if m == nil {
		t.Fatalf("%q", a.Exec.Texts())
	}
	data, err := os.ReadFile(m[2])
	if err != nil {
		t.Fatal(err)
	}
	return m[2], strings.TrimPrefix(m[3], " "), data
}

func fdNoFiles(t *testing.T, f *fdRig) {
	t.Helper()
	if ents, _ := os.ReadDir(f.inbox); len(ents) != 0 {
		t.Fatalf("files in the inbox: %v", ents)
	}
	if n := len(f.Agents["laptop"].Exec.Texts()); n != 0 {
		t.Fatalf("the session was told: %q", f.Agents["laptop"].Exec.Texts())
	}
}

// The parts go out in batches: a window of sixteen, and the next batch when half of it is free and the turn is open (one
// batch per minTurn, 250ms here). Sent one by one as the answers come, the ratchets of the two ends would take a new key for
// nearly every message and refuse messages (see the acceptance tests in internal/e2e).
func TestThePartsGoOutInBatchesOfAWindowOfSixteenAndTheNextBatchWaitsForHalfOfItToBeFree(t *testing.T) {
	f := fdNew(t, fdQuiet())
	f.StartAll()
	g := f.fdGate()
	g.gating.Store(true)
	size := 39*wire.MaxChunk + 10 // forty parts
	data := fdBytes(size, 1)
	f.TG.Serve("big", data)
	base := g.sent.Load()
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: int64(size)}, "forty")

	f.Eventually("sixteen parts out", func() bool { return g.sent.Load()-base == 16 })
	f.Never("a seventeenth part before any part was answered", 400*time.Millisecond, func() bool { return g.sent.Load()-base > 16 })
	g.allow(7)
	f.Never("a batch for seven answers: it goes when half of the window is free", 500*time.Millisecond, func() bool { return g.sent.Load()-base > 16 })
	g.allow(1)
	f.Eventually("eight answers, a batch of eight parts", func() bool { return g.sent.Load()-base == 24 })
	f.Never("more parts than were answered", 500*time.Millisecond, func() bool { return g.sent.Load()-base > 24 })
	g.open()

	_, caption, got := fdTypedLine(t, f, 1)
	if caption != "forty" || !bytes.Equal(got, data) {
		t.Fatalf("%q, %d bytes", caption, len(got))
	}
	if n := g.sent.Load() - base; n != 41 { // forty parts and the closing command
		t.Fatalf("%d messages to the agent", n)
	}
}

func TestTheHubGivesUpWhenTheAgentStopsAnswering(t *testing.T) {
	f := fdNew(t, fdQuiet())
	f.StartAll()
	hubsvc.SetFileAckWait(f.Hub, 400*time.Millisecond)
	g := f.fdGate()
	g.gating.Store(true)
	f.TG.Serve("big", fdBytes(5*wire.MaxChunk, 2))
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: 5 * wire.MaxChunk}, "")
	f.WaitSent("laptop did not take the file (0 of 5 parts were confirmed)")
	f.Never("a second message about the same file", 600*time.Millisecond, func() bool { return count(f.Rig, "did not take the file") > 1 })
	before := g.sent.Load()
	time.Sleep(200 * time.Millisecond)
	if g.sent.Load() != before {
		t.Fatal("the hub went on sending after it gave up")
	}
	g.open()
	// The parts that reached the agent make no file: without the closing command nothing is saved or typed.
	f.Never("a file from an abandoned transfer", 500*time.Millisecond, func() bool { return len(f.Agents["laptop"].Exec.Texts()) > 0 })
	// The place of the abandoned transfer is free again.
	hubsvc.SetFileAckWait(f.Hub, 10*time.Second)
	small := fdBytes(1000, 3)
	f.TG.Serve("small", small)
	f.SayMedia(telegram.FileRef{ID: "small", Name: "small.bin", Size: 1000}, "after")
	_, caption, got := fdTypedLine(t, f, 1)
	if caption != "after" || !bytes.Equal(got, small) {
		t.Fatalf("%q", caption)
	}
}

func TestAnAgentThatGoesOfflineInTheMiddleEndsTheTransferWithOneMessage(t *testing.T) {
	f := fdNew(t, fdQuiet())
	f.StartAll()
	hubsvc.SetFileAckWait(f.Hub, 500*time.Millisecond)
	g := f.fdGate()
	g.gating.Store(true)
	f.TG.Serve("big", fdBytes(30*wire.MaxChunk, 4))
	base := g.sent.Load()
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: 30 * wire.MaxChunk}, "")
	f.Eventually("the first window", func() bool { return g.sent.Load()-base == 16 })
	f.StopAgent("laptop")
	f.WaitSent("laptop did not take the file")
	if count(f.Rig, "⚠️") != 1 {
		t.Fatalf("%q", f.SentTexts())
	}
	g.open()
	fdNoFiles(t, f)
	// The agent comes back and the next file goes through.
	g.gating.Store(false)
	f.StartAgent("laptop")
	f.WaitConnected("laptop")
	f.TG.Serve("small", []byte("tiny"))
	f.SayMedia(telegram.FileRef{ID: "small", Name: "small.txt", Size: 4}, "")
	if _, _, got := fdTypedLine(t, f, 1); string(got) != "tiny" {
		t.Fatalf("%q", got)
	}
}

func TestARestartedHubTellsTheChatThatTheFileWasLost(t *testing.T) {
	f := fdNew(t, testrig.Options{FaultyBus: true}) // the agent must notice by itself that the new hub does not know its session
	f.StartAll()
	g := f.fdGate()
	g.gating.Store(true)
	f.TG.Serve("big", fdBytes(20*wire.MaxChunk, 5))
	base := g.sent.Load()
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: 20 * wire.MaxChunk}, "")
	f.Eventually("the first window", func() bool { return g.sent.Load()-base >= 16 })
	f.StartHub() // the old hub stops and a new one starts
	f.WaitSent("The hub was stopped while it sent your file")
	g.open()
	g.gating.Store(false)
	// The partial file on the agent is never finished, and the new hub sends the next file as usual (the agent sets up a
	// new session by itself once it hears nothing from the old one; a new hub answers its hello).
	f.Eventually("the agent to be back", func() bool { return f.Hub.Online("laptop") })
	f.TG.Serve("small", []byte("after the restart"))
	f.SayMedia(telegram.FileRef{ID: "small", Name: "small.txt", Size: 17}, "")
	if _, _, got := fdTypedLine(t, f, 1); string(got) != "after the restart" {
		t.Fatalf("%q", got)
	}
	n := 0
	ents, _ := os.ReadDir(f.inbox)
	for _, e := range ents {
		if e.Name() != ".gitignore" { // a new inbox ignores itself in git
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d files in the inbox, want only the second", n)
	}
}

func TestAThirdFileWhileTwoAreOnTheWayIsTurnedAway(t *testing.T) {
	f := fdNew(t, fdQuiet())
	f.StartAll()
	g := f.fdGate()
	g.gating.Store(true)
	f.TG.Serve("a", fdBytes(12*wire.MaxChunk, 6))
	f.TG.Serve("b", fdBytes(12*wire.MaxChunk, 7))
	f.TG.Serve("c", []byte("third"))
	base := g.sent.Load()
	f.SayMedia(telegram.FileRef{ID: "a", Name: "a.bin", Size: 12 * wire.MaxChunk}, "")
	f.SayMedia(telegram.FileRef{ID: "b", Name: "b.bin", Size: 12 * wire.MaxChunk}, "")
	f.Eventually("the parts of both files out", func() bool { return g.sent.Load()-base == 24 })
	f.SayMedia(telegram.FileRef{ID: "c", Name: "c.txt", Size: 5}, "")
	f.WaitSent("busy with other files")
	if n := f.chat.fetches.Load(); n != 2 {
		t.Fatalf("%d fetches: the turned away file was fetched", n)
	}
	g.open()
	f.Eventually("both files typed", func() bool { return len(f.Agents["laptop"].Exec.Texts()) == 2 })
	// a place is free again
	f.SayMedia(telegram.FileRef{ID: "c", Name: "c.txt", Size: 5}, "")
	f.Eventually("the third typed", func() bool { return len(f.Agents["laptop"].Exec.Texts()) == 3 })
}

func TestABusThatRefusesPartsEndsTheTransferWithOneMessage(t *testing.T) {
	f := fdNew(t, fdQuiet())
	f.StartAll()
	toAgent := f.Names.ToAgent("laptop")
	var n atomic.Int32
	f.Faulty.FailPublish(func(topic string) error {
		if topic == toAgent && n.Add(1) > 3 {
			return errors.New("bus down")
		}
		return nil
	})
	f.TG.Serve("big", fdBytes(40*wire.MaxChunk, 8))
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: 40 * wire.MaxChunk}, "")
	f.WaitSent("The message bus did not accept the file")
	f.Never("more than one message about it", 800*time.Millisecond, func() bool { return count(f.Rig, "⚠️") > 1 })
	fdNoFiles(t, f)
}

func TestALockdownDuringATransferStopsIt(t *testing.T) {
	f := fdNew(t, fdQuiet())
	f.StartAll()
	g := f.fdGate()
	g.gating.Store(true)
	f.TG.Serve("big", fdBytes(20*wire.MaxChunk, 9))
	base := g.sent.Load()
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: 20 * wire.MaxChunk}, "")
	f.Eventually("the first window", func() bool { return g.sent.Load()-base == 16 })
	f.Say("/lockdown")
	f.WaitSent("Lockdown:")
	g.open()
	f.WaitSent("Lockdown is on: the file was not sent completely")
	fdNoFiles(t, f)
	f.Eventually("the agent locked", f.Agents["laptop"].Agent.Locked)
}

func TestAnAgentThatIsLockedRefusesTheFileAndSaysSoOnce(t *testing.T) {
	// The hub does not know (its last heartbeat said unlocked), so it sends; the agent keeps nothing and tells why.
	f := fdNew(t, fdQuiet())
	f.StartAll()
	f.Say("/lockdown")
	f.Eventually("the agent locked", f.Agents["laptop"].Agent.Locked)
	f.Say("/unlock")
	f.WaitSent("The hub relays again")
	hubsvc.SetFileAckWait(f.Hub, 300*time.Millisecond)
	f.TG.Serve("big", fdBytes(5*wire.MaxChunk, 10))
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: 5 * wire.MaxChunk}, "")
	f.WaitSent("refused the file: this machine is locked")
	f.WaitSent("laptop did not take the file")
	if count(f.Rig, "refused the file") != 1 {
		t.Fatalf("the refusal was said more than once: %q", f.SentTexts())
	}
	fdNoFiles(t, f)
}

func TestAnAgentThatReportsItselfLockedIsNotSentAFile(t *testing.T) {
	f := fdNew(t, testrig.Options{FaultyBus: true, Heartbeat: 40 * time.Millisecond})
	f.StartAll()
	f.Say("/lockdown")
	f.Eventually("the agent locked", f.Agents["laptop"].Agent.Locked)
	f.Say("/unlock")
	f.WaitSent("The hub relays again")
	time.Sleep(300 * time.Millisecond) // a few heartbeats: the hub learns that the agent is locked
	f.TG.Serve("x", []byte("data"))
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, "")
	f.WaitSent("laptop is locked")
	if f.chat.fetches.Load() != 0 {
		t.Fatal("the file was fetched for a locked machine")
	}
	fdNoFiles(t, f)
}

func TestAnOfflineAgentIsNotSentAFile(t *testing.T) {
	f := fdNew(t, testrig.Options{})
	f.StartAll()
	f.StopAgent("laptop")
	f.Eventually("the hub to see the agent offline", func() bool { return !f.Hub.Online("laptop") })
	f.TG.Serve("x", []byte("data"))
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, "")
	f.WaitSent("laptop is offline")
	if f.chat.fetches.Load() != 0 {
		t.Fatal("the file was fetched for an offline machine")
	}
}

func TestWhileTheHubIsLockedDownNoFileIsFetched(t *testing.T) {
	f := fdNew(t, testrig.Options{})
	f.StartAll()
	f.Say("/lockdown")
	f.WaitSent("Lockdown:")
	f.TG.Serve("x", []byte("data"))
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, "")
	f.WaitSent("Lockdown is on: nothing is sent")
	if f.chat.fetches.Load() != 0 {
		t.Fatal("the file was fetched during a lockdown")
	}
}

func TestAClientThatCannotFetchFilesIsSaidSo(t *testing.T) {
	r := testrig.New(t, testrig.Options{WrapTelegram: func(f *telegram.Fake) telegram.Client { return struct{ telegram.Client }{f} }})
	r.StartAll()
	r.TG.Serve("x", []byte("data"))
	r.SayMedia(telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, "")
	r.WaitSent("cannot take files")
}

func TestAFileAboveTheLimitIsRefusedBeforeItIsFetched(t *testing.T) {
	o := testrig.Options{Hub: func(c *hubsvc.Config) { c.MaxFileBytes = 1000 }}
	f := fdNew(t, o)
	f.StartAll()
	f.TG.Serve("x", make([]byte, 2000))
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.bin", Size: 2000}, "")
	f.WaitSent("The file is too large")
	if f.chat.fetches.Load() != 0 {
		t.Fatal("a file that is too large was fetched")
	}
	// Telegram did not say how big it is and the download turns out larger than the limit.
	f.chat.bytes = func(telegram.FileRef) []byte { return make([]byte, 5000) }
	f.SayMedia(telegram.FileRef{ID: "y", Name: "y.bin"}, "")
	f.Eventually("the second refusal", func() bool { return count(f.Rig, "The file is too large") == 2 })
	fdNoFiles(t, f)
}

func TestAnEmptyFileAndAFailedFetchAreReported(t *testing.T) {
	f := fdNew(t, testrig.Options{})
	f.StartAll()
	f.TG.Serve("empty", []byte{})
	f.SayMedia(telegram.FileRef{ID: "empty", Name: "empty.txt"}, "")
	f.WaitSent("The file is empty")
	f.TG.SetFetchErr(errors.New("telegram is down"))
	f.SayMedia(telegram.FileRef{ID: "other", Name: "x.txt", Size: 4}, "")
	f.WaitSent("Could not fetch the file")
	f.TG.SetFetchErr(nil)
	f.SayMedia(telegram.FileRef{ID: "never-served", Name: "x.txt", Size: 4}, "")
	f.Eventually("the third report", func() bool { return count(f.Rig, "Could not fetch the file") == 2 })
	fdNoFiles(t, f)
	if strings.Contains(strings.Join(f.SentTexts(), "\n"), "telegram is down") {
		t.Fatal("the error of the client was shown in the chat")
	}
}

func TestTheReasonAFetchFailedIsSaidInWords(t *testing.T) {
	f := fdNew(t, testrig.Options{})
	f.StartAll()
	f.chat.err = telegram.ErrFileTooLarge
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.bin"}, "")
	f.WaitSent("too large to fetch")
	f.chat.err = telegram.ErrRateLimited{After: 3 * time.Second}
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.bin"}, "")
	f.WaitSent("asks to slow down")
	f.chat.err = &telegram.APIError{Method: "getFile", Code: 400, Desc: "Bad Request: wrong file_id"}
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.bin"}, "")
	f.WaitSent("Could not fetch the file")
	if strings.Contains(strings.Join(f.SentTexts(), "\n"), "wrong file_id") {
		t.Fatal("the text of Telegram's error was shown in the chat")
	}
	fdNoFiles(t, f)
}

func TestAnOldFileIsIgnored(t *testing.T) {
	f := fdNew(t, testrig.Options{})
	f.StartAll()
	f.TG.Serve("x", []byte("data"))
	f.SayMediaWith(telegram.Update{Media: &telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, Date: time.Now().Add(-time.Hour)})
	f.WaitSent("Ignored an old file")
	if f.chat.fetches.Load() != 0 {
		t.Fatal("an old file was fetched")
	}
}

func TestAFileFromAnotherUserIsNeitherFetchedNorAnswered(t *testing.T) {
	f := fdNew(t, testrig.Options{})
	f.StartAll()
	f.TG.Serve("x", []byte("data"))
	f.SayMediaWith(telegram.Update{Media: &telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, UserID: 666})
	f.SayMediaWith(telegram.Update{Media: &telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, ChatID: 777})
	f.Say("marker") // the hub has read everything before this
	f.Eventually("the marker", func() bool { return len(f.Agents["laptop"].Exec.Texts()) == 1 })
	if f.chat.fetches.Load() != 0 || len(f.SentTexts()) != 1 { // "laptop is connected"
		t.Fatalf("fetched %d, chat %q", f.chat.fetches.Load(), f.SentTexts())
	}
	if f.Hub.Stats().Unauthorized != 2 {
		t.Fatalf("%d", f.Hub.Stats().Unauthorized)
	}
}

func TestAHugeCaptionIsCutBeforeItTravels(t *testing.T) {
	f := fdNew(t, testrig.Options{})
	f.StartAll()
	f.TG.Serve("x", []byte("data"))
	f.SayMedia(telegram.FileRef{ID: "x", Name: "x.txt", Size: 4}, strings.Repeat("é", 2000)+"\n/lockdown")
	_, caption, _ := fdTypedLine(t, f, 1)
	if len([]rune(caption)) != 500 || strings.Contains(caption, "lockdown") {
		t.Fatalf("%d characters", len([]rune(caption)))
	}
	if f.Hub.Stats().Rejected != 0 {
		t.Fatal("a message was rejected")
	}
}
