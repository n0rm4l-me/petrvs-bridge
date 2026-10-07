package hubsvc_test

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// These tests run the whole way from an agent's spool to the fake chat, with the pieces the hub is really built of: the
// ratchet, the receipts, the claim on an item that is being dealt with.

func upFileBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func upPartItem(t string, index int, data []byte) spool.Item {
	return spool.Item{Type: wire.TypePart, Body: filetx.EncodePart(data), Meta: map[string]string{wire.MetaTransfer: t, wire.MetaIndex: strconv.Itoa(index)}}
}

func upClosingItem(t string, parts int, data []byte, name, caption string) spool.Item {
	return spool.Item{Type: wire.TypeFile, Body: caption, Meta: map[string]string{
		wire.MetaTransfer: t, wire.MetaParts: strconv.Itoa(parts), wire.MetaName: name, wire.MetaSize: strconv.Itoa(len(data)),
		wire.MetaSHA: filetx.SHA(data), wire.MetaKind: filetx.Kind(name, int64(len(data))),
	}}
}

// queue puts a file into an agent's spool as `agent send-file` does.
func upQueue(t *testing.T, a *testrig.AgentRig, data []byte, name, caption string) string {
	t.Helper()
	tr := filetx.NewID()
	chunks := filetx.Chunks(data)
	for i, c := range chunks {
		if err := a.Spool.AddItem(upPartItem(tr, i, c)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Spool.AddItem(upClosingItem(tr, len(chunks), data, name, caption)); err != nil {
		t.Fatal(err)
	}
	return tr
}

func upEmptied(a *testrig.AgentRig) func() bool { return func() bool { return a.Spool.Len() == 0 } }

func TestAFileOfThreePartsIsUploadedOnceAndConfirmed(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	data := upFileBytes(300_000, 1)
	upQueue(t, a, data, "report.pdf", "the report")
	r.Eventually("the upload", func() bool { return len(r.TG.Uploads()) >= 1 })
	r.Eventually("the agent to be told that everything arrived", upEmptied(a))
	r.Never("a second upload", 600*time.Millisecond, func() bool { return len(r.TG.Uploads()) > 1 })
	up := r.TG.Uploads()[0]
	if !bytes.Equal(up.Data, data) || up.Name != "report.pdf" || up.Caption != "the report" || up.Photo {
		t.Fatalf("%q %q photo %v, %d bytes", up.Name, up.Caption, up.Photo, len(up.Data))
	}
	if r.Sent("did not arrive") || r.Sent("⚠️") {
		t.Fatalf("the user was told of a problem: %q", r.SentTexts())
	}
}

func TestAPictureGoesAsAPhotoAndATextFileAsADocument(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "desk"}})
	r.StartAll()
	upQueue(t, r.Agents["laptop"], []byte("\x89PNG\r\n\x1a\n and some bytes"), "screen.png", "")
	upQueue(t, r.Agents["desk"], []byte("just text"), "notes.txt", "my notes")
	r.Eventually("both uploads", func() bool { return len(r.TG.Uploads()) == 2 })
	got := map[string]telegram.OutFile{}
	for _, u := range r.TG.Uploads() {
		got[u.Name] = u
	}
	if u := got["screen.png"]; !u.Photo || u.Caption != "[laptop] screen.png" {
		t.Fatalf("%+v", u)
	}
	if u := got["notes.txt"]; u.Photo || u.Caption != "[desk] my notes" {
		t.Fatalf("%+v", u)
	}
}

// The bus does not keep order. Whatever order the parts arrive in, the file is whole once.
func TestPartsThatArriveBackwardsAreStillOneFile(t *testing.T) {
	r := testrig.New(t, testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour}) // only the parts are on the bus
	r.StartAll()
	a := r.Agents["laptop"]
	hubSub := r.Names.HubSub()
	before := len(r.Mem.Published(r.Names.ToHub()))
	r.Mem.Hold(hubSub)
	data := upFileBytes(400_000, 2) // 4 parts
	upQueue(t, a, data, "scan.bin", "")
	r.Eventually("the four parts to be held on the bus", func() bool { return len(r.Mem.Published(r.Names.ToHub()))-before >= 4 })
	r.Mem.Release(hubSub, func(n int) []int {
		order := make([]int, n)
		for i := range order {
			order[i] = n - 1 - i
		}
		return order
	})
	r.Eventually("the upload", func() bool { return len(r.TG.Uploads()) >= 1 })
	r.Eventually("the receipts", upEmptied(a))
	if ups := r.TG.Uploads(); len(ups) != 1 || !bytes.Equal(ups[0].Data, data) {
		t.Fatalf("%d uploads", len(ups))
	}
}

// A part that the bus loses is sent again by the agent after the receipt timeout; the closing item waits for it.
func TestALostPartIsResentAndTheFileStillArrivesOnce(t *testing.T) {
	r := testrig.New(t, testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour})
	r.StartAll()
	a := r.Agents["laptop"]
	hubTopic := r.Names.ToHub()
	var armed atomic.Bool
	var seen atomic.Int32
	r.Mem.SetDrop(func(p transport.Published) bool {
		return armed.Load() && p.Topic == hubTopic && seen.Add(1) == 2 // the second message after this point: part 1
	})
	armed.Store(true)
	data := upFileBytes(400_000, 3)
	upQueue(t, a, data, "scan.bin", "")
	r.Eventually("the upload", func() bool { return len(r.TG.Uploads()) >= 1 })
	r.Eventually("the receipts", upEmptied(a))
	r.Never("a second upload", 600*time.Millisecond, func() bool { return len(r.TG.Uploads()) > 1 })
	if ups := r.TG.Uploads(); !bytes.Equal(ups[0].Data, data) {
		t.Fatal("the file is not what was sent")
	}
	if r.Sent("did not arrive") {
		t.Fatalf("%q", r.SentTexts())
	}
}

func TestAFileWithAPartMissingIsReportedAndTheQueueDrains(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	data := upFileBytes(300_000, 4)
	chunks := filetx.Chunks(data)
	tr := filetx.NewID()
	a.Spool.AddItem(upPartItem(tr, 0, chunks[0]))
	a.Spool.AddItem(upPartItem(tr, 2, chunks[2])) // part 1 was never queued (the sender died)
	a.Spool.AddItem(upClosingItem(tr, 3, data, "lost.bin", ""))
	r.WaitSent("⚠️ laptop: the file lost.bin did not arrive whole (2 of 3 parts); send it again")
	r.Eventually("the agent to stop resending", upEmptied(a))
	time.Sleep(400 * time.Millisecond)
	if count(r, "lost.bin") != 1 || len(r.TG.Uploads()) != 0 {
		t.Fatalf("%q, %d uploads", r.SentTexts(), len(r.TG.Uploads()))
	}
	// the next file is not held up by the wreck of this one
	data2 := upFileBytes(1000, 5)
	upQueue(t, a, data2, "fine.bin", "")
	r.Eventually("the next file", func() bool { return len(r.TG.Uploads()) == 1 })
}

func TestAWrongHashIsReportedAndTheQueueDrains(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	data := upFileBytes(5000, 6)
	tr := filetx.NewID()
	a.Spool.AddItem(upPartItem(tr, 0, data))
	it := upClosingItem(tr, 1, data, "bad.bin", "")
	it.Meta[wire.MetaSHA] = filetx.SHA([]byte("something else"))
	a.Spool.AddItem(it)
	r.WaitSent("⚠️ laptop: the file bad.bin arrived damaged")
	r.Eventually("the queue to drain", upEmptied(a))
	if len(r.TG.Uploads()) != 0 {
		t.Fatal("a file with the wrong hash was uploaded")
	}
}

func TestADuplicatedPartAndAPartsOutOfOrderInTheQueue(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	data := upFileBytes(300_000, 7)
	chunks := filetx.Chunks(data)
	tr := filetx.NewID()
	for _, i := range []int{2, 0, 0, 1, 2} {
		a.Spool.AddItem(upPartItem(tr, i, chunks[i]))
	}
	a.Spool.AddItem(upClosingItem(tr, 3, data, "mixed.bin", ""))
	r.Eventually("the upload", func() bool { return len(r.TG.Uploads()) >= 1 })
	r.Eventually("the queue to drain", upEmptied(a))
	if ups := r.TG.Uploads(); len(ups) != 1 || !bytes.Equal(ups[0].Data, data) {
		t.Fatalf("%d uploads", len(ups))
	}
}

func TestATelegramThatFailsAtFirstGetsTheFileLaterAndOnlyOnce(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	r.TG.SetFileErr(&telegram.APIError{Method: "sendDocument", Code: 502, Desc: "Bad Gateway"})
	data := upFileBytes(250_000, 8)
	upQueue(t, a, data, "later.bin", "")
	// the parts are confirmed, the closing item waits for Telegram and is sent again now and then
	r.Eventually("only the closing item is left", func() bool { return a.Spool.Len() == 1 })
	r.Never("an upload or a word about it while Telegram may still recover", 500*time.Millisecond, func() bool {
		return len(r.TG.Uploads()) > 0 || r.Sent("later.bin")
	})
	r.TG.SetFileErr(nil)
	r.Eventually("the upload", func() bool { return len(r.TG.Uploads()) == 1 })
	r.Eventually("the receipt", upEmptied(a))
	r.Never("a second upload", 600*time.Millisecond, func() bool { return len(r.TG.Uploads()) > 1 })
	if !bytes.Equal(r.TG.Uploads()[0].Data, data) {
		t.Fatal("the file is not what was sent")
	}
}

func TestATelegramThatNeverRecoversIsGivenUpOnWithAWord(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	r.TG.SetFileErr(telegram.ErrRateLimited{After: time.Minute})
	upQueue(t, a, []byte("hello"), "never.txt", "")
	r.WaitSent("⚠️ laptop: Telegram did not take the file never.txt after several tries")
	r.Eventually("the agent to stop resending", upEmptied(a))
}

func TestATelegramThatRefusesTheFileIsAskedOnceAndTheUserToldWhy(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	r.TG.SetFileErr(&telegram.APIError{Method: "sendDocument", Code: 400, Desc: "Bad Request: wrong file identifier"})
	upQueue(t, a, []byte("hello"), "odd.txt", "")
	r.WaitSent("⚠️ laptop: Telegram refused the file odd.txt (Bad Request: wrong file identifier)")
	r.Eventually("the agent to stop resending", upEmptied(a))
	r.TG.SetFileErr(nil)
	r.Never("the refused file sent after all", 500*time.Millisecond, func() bool { return len(r.TG.Uploads()) > 0 })
}

func TestAHubWhoseChatCannotUploadSaysSo(t *testing.T) {
	r := testrig.New(t, testrig.Options{WrapTelegram: func(f *telegram.Fake) telegram.Client { return struct{ telegram.Client }{f} }})
	r.StartAll()
	a := r.Agents["laptop"]
	upQueue(t, a, []byte("hello"), "a.txt", "")
	r.WaitSent("this hub cannot upload files")
	r.Eventually("the agent to stop resending", upEmptied(a))
}

// upSlowUploads holds every upload until the gate is closed.
type upSlowUploads struct {
	*telegram.Fake
	gate  chan struct{}
	mu    sync.Mutex
	calls int
}

func (s *upSlowUploads) SendFile(ctx context.Context, chatID int64, o telegram.OutFile) ([]int64, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case <-s.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Fake.SendFile(ctx, chatID, o)
}

func (s *upSlowUploads) Calls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// The agent sends the closing item again every receipt timeout while it has no receipt. The resend of an item that is
// being uploaded must be dropped, not uploaded a second time.
func TestAResendOfAClosingItemThatIsBeingUploadedIsNotUploadedTwice(t *testing.T) {
	var chat *upSlowUploads
	r := testrig.New(t, testrig.Options{FaultyBus: true, WrapTelegram: func(f *telegram.Fake) telegram.Client {
		chat = &upSlowUploads{Fake: f, gate: make(chan struct{})}
		return chat
	}})
	r.Faulty.Concurrent = true // like Pub/Sub: the resend is handled while the first copy is still at work
	r.StartAll()
	a := r.Agents["laptop"]
	upQueue(t, a, upFileBytes(2000, 9), "slow.bin", "")
	r.Eventually("the upload to begin", func() bool { return chat.Calls() >= 1 })
	time.Sleep(time.Second) // the agent sends the closing item again every 300ms
	if n := chat.Calls(); n != 1 {
		t.Fatalf("%d uploads were started", n)
	}
	close(chat.gate)
	r.Eventually("the file in the chat", func() bool { return len(r.TG.Uploads()) == 1 })
	r.Eventually("the receipt", upEmptied(a))
	r.Never("a second upload", 500*time.Millisecond, func() bool { return len(r.TG.Uploads()) > 1 })
}

// A stranger with the right to publish can send what it likes; whatever an agent's session carries is checked, and the
// queue of an agent with junk in it still moves.
func TestJunkInTheQueueDoesNotStopWhatComesAfterIt(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	for _, it := range []spool.Item{
		{Type: wire.TypePart, Body: "???", Meta: map[string]string{wire.MetaTransfer: "aaaaaa", wire.MetaIndex: "0"}},
		{Type: wire.TypePart, Body: "QQ==", Meta: map[string]string{wire.MetaTransfer: "../x", wire.MetaIndex: "0"}},
		{Type: wire.TypePart, Body: "QQ=="},
		{Type: wire.TypePart, Body: "QQ==", Meta: map[string]string{wire.MetaTransfer: "aaaaaa", wire.MetaIndex: "-1"}},
		{Type: wire.TypeFile, Body: strings.Repeat("c", 100_000)},
		{Type: wire.TypeFile, Meta: map[string]string{wire.MetaTransfer: "bbbbbb", wire.MetaParts: "99999999", wire.MetaSize: "1", wire.MetaSHA: "x"}},
	} {
		if err := a.Spool.AddItem(it); err != nil {
			t.Fatal(err)
		}
	}
	a.Spool.Add(wire.TypeReply, "an answer after the junk")
	upQueue(t, a, []byte("the good file"), "good.txt", "")
	r.WaitSent("an answer after the junk")
	r.Eventually("the good file", func() bool { return len(r.TG.Uploads()) == 1 })
	r.Eventually("the queue to drain", upEmptied(a))
	if string(r.TG.Uploads()[0].Data) != "the good file" {
		t.Fatal("wrong file")
	}
}

// While an agent is locked it sends no file. The parts wait in its spool and go when the machine is unlocked.
func TestAFileQueuedOnALockedMachineGoesWhenItIsUnlocked(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	a := r.Agents["laptop"]
	if err := os.WriteFile(a.LockFile, []byte("locked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.StartAll()
	data := upFileBytes(300_000, 10)
	upQueue(t, a, data, "held.bin", "")
	a.Spool.Add(wire.TypeReply, "replies still go")
	r.WaitSent("replies still go")
	r.Never("the file left a locked machine", 800*time.Millisecond, func() bool { return len(r.TG.Uploads()) > 0 })
	if a.Spool.Len() != 4 { // three parts and the closing item
		t.Fatalf("%d items in the spool", a.Spool.Len())
	}
	if err := os.Remove(a.LockFile); err != nil {
		t.Fatal(err)
	}
	r.Eventually("the upload after the unlock", func() bool { return len(r.TG.Uploads()) == 1 })
	r.Eventually("the queue to drain", upEmptied(a))
	if !bytes.Equal(r.TG.Uploads()[0].Data, data) {
		t.Fatal("the file is not what was queued")
	}
}

// A reply to the message that carries a file goes to the agent the file came from.
func TestAReplyToAFileGoesToTheAgentItCameFrom(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "desk"}, Hub: func(c *hubsvc.Config) { c.DefaultAgent = "laptop" }})
	r.StartAll()
	desk := r.Agents["desk"]
	r.Agents["laptop"].Spool.Add(wire.TypeReply, "marker")
	r.WaitSent("marker")
	marker := r.MessageID("marker")
	upQueue(t, desk, []byte("from the desk"), "d.txt", "")
	r.Eventually("the upload", func() bool { return len(r.TG.Uploads()) == 1 })
	r.SayReplyTo("got it, thanks", marker+1) // the upload is the next message the fake chat numbered
	r.Eventually("the reply to arrive on the desk", func() bool { return len(desk.Exec.Texts()) == 1 })
	if len(r.Agents["laptop"].Exec.Texts()) != 0 {
		t.Fatal("the reply went to the wrong machine")
	}
}
