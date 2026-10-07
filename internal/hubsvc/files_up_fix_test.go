package hubsvc

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// A sender that takes a file and keeps nothing of it, so that a test can measure what the hub itself holds.
type forgetfulSender struct {
	*telegram.Fake
	sent atomic.Int32
}

func (s *forgetfulSender) SendFile(context.Context, int64, telegram.OutFile) ([]int64, error) {
	s.sent.Add(1)
	return []int64{1}, nil
}

func TestFinishedFilesAreNotKeptInMemoryByTheTimerThatWouldFreeThem(t *testing.T) {
	f := newUpFixture(t, nil)
	sender := &forgetfulSender{Fake: f.tg}
	f.h.cfg.Telegram = sender
	const files, size = 24, 4 << 20
	for i := 0; i < files; i++ {
		data := upRandom(size + i) // another file each time, gone from the test's own memory after the call
		if !f.send(t, "laptop", fmt.Sprintf("t%010d", i), data, "big.bin", "") {
			t.Fatalf("file %d was not dealt with", i)
		}
	}
	if int(sender.sent.Load()) != files || len(f.h.up.ready) != 0 {
		t.Fatalf("%d uploaded, %d kept for a retry", sender.sent.Load(), len(f.h.up.ready))
	}
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// 96 MiB were sent in all. The timers of the files (ten minutes each) kept every one of them until they fired.
	if held := m.HeapAlloc >> 20; held > 48 {
		t.Fatalf("the hub holds %d MiB after it has sent %d MiB and kept nothing", held, files*size>>20)
	}
}

func TestTheTimerOfAFinishedFileIsStoppedWhenTheFileIsDone(t *testing.T) {
	f := newUpFixture(t, nil)
	f.h.up.keepReady("laptop/t1", []byte("data"), f.clock())
	f.h.up.mu.Lock()
	entry := f.h.up.ready["laptop/t1"]
	f.h.up.finish("laptop/t1")
	f.h.up.mu.Unlock()
	if entry.timer == nil || entry.timer.Stop() {
		t.Fatal("the timer of a file that is finished is still running")
	}
	// and the one of an entry that is replaced
	f.h.up.keepReady("laptop/t2", []byte("one"), f.clock())
	f.h.up.mu.Lock()
	first := f.h.up.ready["laptop/t2"]
	f.h.up.mu.Unlock()
	f.h.up.keepReady("laptop/t2", []byte("two"), f.clock())
	if first.timer.Stop() {
		t.Fatal("the timer of a replaced entry is still running")
	}
}

func TestAFinishedFileIsFreedByItsOwnTimerWhenNothingSweepsIt(t *testing.T) {
	f := newUpFixture(t, nil)
	f.h.up.mu.Lock()
	f.h.up.readyTTL = 50 * time.Millisecond
	f.h.up.mu.Unlock()
	f.h.up.keepReady("laptop/t1", make([]byte, 1000), f.clock())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.h.up.mu.Lock()
		n := len(f.h.up.ready)
		f.h.up.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the finished file was never freed")
}

// Items that waited for the slot may each leave a finished file behind (an upload that did not work); the room that was
// there when they arrived is gone by the time it is their turn.
func TestFinishedFilesCannotPassTheirBoundWhileItemsWaitForTheSlot(t *testing.T) {
	var g *upGatedSender
	f := newUpFixture(t, func(c *Config) {
		g = &upGatedSender{Fake: c.Telegram.(*telegram.Fake), gate: make(chan struct{}), started: make(chan struct{}, 8)}
		c.Telegram = g
	})
	f.tg.SetFileErr(errors.New("no network")) // every upload fails, so every finished file stays
	datas := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for i, d := range datas {
		tr := fmt.Sprintf("t%010d", i+1)
		f.h.upPart("laptop", upPartPayload("p"+tr, tr, 0, d))
	}
	var wg sync.WaitGroup
	results := make([]bool, len(datas))
	for i, d := range datas {
		tr := fmt.Sprintf("t%010d", i+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = f.h.upFile(context.Background(), "laptop", upFilePayload("f"+tr, tr, 1, d, fmt.Sprintf("f%d.txt", i), ""))
		}()
		if i == 0 {
			<-g.started // the first holds the slot inside its upload
		}
	}
	time.Sleep(150 * time.Millisecond) // the other two have passed the test for room and wait for the slot
	close(g.gate)
	wg.Wait()
	for i, done := range results {
		if done {
			t.Errorf("item %d was confirmed although Telegram took nothing", i)
		}
	}
	f.h.up.mu.Lock()
	ready := len(f.h.up.ready)
	f.h.up.mu.Unlock()
	if ready != upReadyMax {
		t.Fatalf("%d finished files are kept, want at most %d", ready, upReadyMax)
	}
	if n, _ := f.held(); n != 1 {
		t.Fatalf("the third file should wait as parts: %d transfers held", n)
	}
}

// ---- a give-up is said before it is confirmed ----

// A verdict is said to the chat before the agent is told to forget the file: when the chat cannot take text, the item is not
// confirmed, the agent sends it again, and the next try says it.
func TestAGiveUpIsConfirmedOnlyWhenTheChatHasHeardIt(t *testing.T) {
	data := upRandom(300_000)
	chunks := filetx.Chunks(data)
	three := func(f *upFixture, tr string) {
		for i, c := range chunks {
			f.h.upPart("laptop", upPartPayload(fmt.Sprintf("p%d", i), tr, i, c))
		}
	}
	const tr = "t0000000001"
	for _, c := range []struct {
		name string
		mut  func(*Config)
		prep func(f *upFixture)
		item func() *wire.Payload
		say  string
		// again says that a closing item for the same transfer after the verdict is answered without saying it a second time
		// (the transfer is remembered as finished); a damaged description and a file above the limit have no record to go by.
		again bool
	}{
		{name: "a damaged description",
			item: func() *wire.Payload {
				p := upFilePayload("f", tr, 3, data, "a.bin", "")
				delete(p.Meta, wire.MetaSHA)
				return p
			}, say: "damaged description"},
		{name: "a file above the limit", mut: func(c *Config) { c.MaxFileBytes = 1 << 20 },
			item: func() *wire.Payload { return upFilePayload("f", tr, 9, upRandom(1<<20+1), "huge.bin", "") }, say: "larger than this hub takes"},
		{name: "a transfer that grew past the limit", mut: func(c *Config) { c.MaxFileBytes = 250_000 },
			prep: func(f *upFixture) { three(f, tr) },
			item: func() *wire.Payload { return upFilePayload("f", tr, 3, data[:240_000], "big.bin", "") }, say: "could not be taken", again: true},
		{name: "a hub that cannot upload", mut: func(c *Config) { c.Telegram = struct{ telegram.Client }{c.Telegram} },
			prep: func(f *upFixture) { three(f, tr) },
			item: func() *wire.Payload { return upFilePayload("f", tr, 3, data, "a.bin", "") }, say: "cannot upload files", again: true},
		{name: "a wrong hash",
			prep: func(f *upFixture) { three(f, tr) },
			item: func() *wire.Payload {
				p := upFilePayload("f", tr, 3, data, "a.bin", "")
				p.Meta[wire.MetaSHA] = filetx.SHA([]byte("other"))
				return p
			}, say: "arrived damaged", again: true},
		{name: "a refusal of Telegram",
			prep: func(f *upFixture) {
				three(f, tr)
				f.tg.SetFileErr(&telegram.APIError{Method: "sendDocument", Code: 400, Desc: "Bad Request: file must be non-empty"})
			},
			item: func() *wire.Payload { return upFilePayload("f", tr, 3, data, "a.bin", "") }, say: "Telegram refused the file a.bin", again: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpFixture(t, c.mut)
			if c.prep != nil {
				c.prep(f)
			}
			f.tg.SetSendErr(errors.New("the chat is down"))
			if f.h.upFile(context.Background(), "laptop", c.item()) {
				t.Fatal("confirmed although nobody could be told: the agent would delete the file and nobody would know")
			}
			if f.said(c.say) != 0 {
				t.Fatalf("said although the chat was down: %q", f.texts())
			}
			f.h.up.mu.Lock()
			_, finished := f.h.up.done["laptop/"+tr]
			f.h.up.mu.Unlock()
			if finished {
				t.Fatal("the transfer was finished before the chat heard of it")
			}
			f.tg.SetSendErr(nil)
			f.tg.SetFileErr(nil) // the resend does not depend on Telegram taking the file now
			if !f.h.upFile(context.Background(), "laptop", c.item()) {
				t.Fatal("the resend was not dealt with")
			}
			if f.said(c.say) != 1 {
				t.Fatalf("the resend did not say it, or said it twice: %q", f.texts())
			}
			if len(f.tg.Uploads()) != 0 {
				t.Fatalf("the file was uploaded on the retry: a verdict given once stands (%d uploads)", len(f.tg.Uploads()))
			}
			if c.again && (!f.h.upFile(context.Background(), "laptop", c.item()) || f.said(c.say) != 1) {
				t.Fatalf("a third try said it again or was not confirmed: %q", f.texts())
			}
		})
	}
}

func TestPartsMissingAreNotGivenUpUntilTheChatCanBeTold(t *testing.T) {
	f := newUpFixture(t, nil)
	data := upRandom(300_000)
	chunks := filetx.Chunks(data)
	f.h.upPart("laptop", upPartPayload("p0", "t0000000001", 0, chunks[0]))
	fp := upFilePayload("f", "t0000000001", 3, data, "scan.pdf", "")
	for i := 1; i < upMissGiveUp; i++ {
		f.advance(30 * time.Second)
		if f.h.upFile(context.Background(), "laptop", fp) {
			t.Fatalf("try %d: given up too early", i)
		}
	}
	f.tg.SetSendErr(errors.New("the chat is down"))
	f.advance(30 * time.Second)
	if f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("given up and confirmed although the chat could not be told")
	}
	f.tg.SetSendErr(nil)
	f.advance(30 * time.Second)
	if !f.h.upFile(context.Background(), "laptop", fp) || f.said("did not arrive whole (1 of 3 parts)") != 1 {
		t.Fatalf("%q", f.texts())
	}
}

// When Telegram has refused the upload several times and the chat cannot take words either, the file is still there: the
// next resend tries the upload once more, and a file that goes through then is in the chat with no word of failure.
func TestAnUploadGiveUpWhoseWordsCouldNotBeSaidTriesTheUploadAgain(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("hello")
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	fp := upFilePayload("f", "t0000000001", 1, data, "a.txt", "")
	f.tg.SetFileErr(telegram.ErrRateLimited{After: time.Second})
	for i := 1; i < upFailGiveUp; i++ {
		if f.h.upFile(context.Background(), "laptop", fp) {
			t.Fatalf("given up at try %d", i)
		}
		f.advance(30 * time.Second)
	}
	f.tg.SetSendErr(errors.New("the chat is down too"))
	if f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("given up and confirmed although the chat could not be told")
	}
	f.h.up.mu.Lock()
	kept := len(f.h.up.ready)
	f.h.up.mu.Unlock()
	if kept != 1 {
		t.Fatal("the file was dropped although nobody was told")
	}
	// Telegram works again by the time the agent sends the item once more
	f.tg.SetFileErr(nil)
	f.tg.SetSendErr(nil)
	f.advance(30 * time.Second)
	if !f.h.upFile(context.Background(), "laptop", fp) || len(f.tg.Uploads()) != 1 {
		t.Fatalf("the retry did not upload the file (%d uploads)", len(f.tg.Uploads()))
	}
	if f.said("Telegram did not take the file") != 0 {
		t.Fatalf("a failure was reported for a file that arrived: %q", f.texts())
	}
}

// ---- parts and the limits ----

// A closing item that comes before its parts makes the record of the transfer. The parts that follow are still a new
// transfer in the Assembler, and the limit on those applies to them.
func TestPartsOfATransferWhoseClosingItemCameFirstStillRespectTheLimit(t *testing.T) {
	f := newUpFixture(t, nil)
	for i := 1; i <= upLiveMax; i++ {
		if !f.h.upPart("laptop", upPartPayload("p", fmt.Sprintf("t%010d", i), 0, []byte("x"))) {
			t.Fatalf("transfer %d was not taken", i)
		}
	}
	over := fmt.Sprintf("t%010d", upLiveMax+1)
	data := []byte("late")
	if f.h.upFile(context.Background(), "laptop", upFilePayload("f", over, 1, data, "a.txt", "")) {
		t.Fatal("a closing item without parts was confirmed")
	}
	if f.h.upPart("laptop", upPartPayload("late", over, 0, data)) {
		t.Fatal("the part was confirmed although the hub could not keep it: the file could never be whole")
	}
	if n, _ := f.held(); n != upLiveMax {
		t.Fatalf("held %d transfers, want %d", n, upLiveMax)
	}
	// room comes, and the resend of the part is taken
	f.advance(upRecordTTL + time.Minute)
	if !f.h.upPart("laptop", upPartPayload("late", over, 0, data)) {
		t.Fatal("the resent part was not taken once there was room")
	}
	if !f.h.upFile(context.Background(), "laptop", upFilePayload("f", over, 1, data, "a.txt", "")) || len(f.tg.Uploads()) != 1 {
		t.Fatalf("uploads %d", len(f.tg.Uploads()))
	}
}

// Whatever the records say, a part that the Assembler cannot take for lack of room is left to the resend, not thrown away.
func TestAPartTheAssemblerHasNoRoomForIsLeftToItsResend(t *testing.T) {
	f := newUpFixture(t, nil)
	now := f.clock()
	for i := 0; i < 16; i++ { // as many as the Assembler takes, with no record of them
		if err := f.h.up.asm.AddPart(fmt.Sprintf("other%d", i), 0, []byte("x"), now); err != nil {
			t.Fatal(err)
		}
	}
	const tr = "t0000000001"
	if f.h.upPart("laptop", upPartPayload("p", tr, 0, []byte("y"))) {
		t.Fatal("confirmed a part that was not stored")
	}
	f.h.up.mu.Lock()
	records := len(f.h.up.records)
	f.h.up.mu.Unlock()
	if records != 0 {
		t.Fatalf("a record was left for a part that was not taken: %d", records)
	}
	f.h.up.asm.Forget("other0")
	if !f.h.upPart("laptop", upPartPayload("p", tr, 0, []byte("y"))) {
		t.Fatal("not taken once there was room")
	}
	if n, _ := f.held(); n != 16 {
		t.Fatalf("held %d", n)
	}
}

func TestAPartThatIsBadForEverIsStillConfirmedAndDropped(t *testing.T) {
	f := newUpFixture(t, nil)
	huge := make([]byte, wire.MaxChunk+1)
	p := upPartPayload("p", "t0000000001", 0, []byte("x"))
	p.Body = filetx.EncodePart(huge) // a chunk above the limit of a part
	if !f.h.upPart("laptop", p) {
		t.Fatal("a part that can never be stored was left to be resent")
	}
	if n := len(f.h.up.records); n != 0 {
		t.Fatalf("%d records", n)
	}
}
