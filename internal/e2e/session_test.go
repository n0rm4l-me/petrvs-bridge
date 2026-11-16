package e2e

import (
	"bytes"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The commands that waited in the agent's queue for an earlier one belong to a session; when the session is replaced their
// numbers mean nothing and they are gone. The user is told, once for each, instead of waiting for them for ever.
func TestCommandsWaitingWhenTheSessionIsReplacedAreReportedAsDropped(t *testing.T) {
	r, a := one(t, testrig.Options{Heartbeat: time.Hour, OfflineAfter: time.Hour, ReorderWindow: time.Minute, SilenceLimit: 2 * time.Second})
	// The agent starts a new session after SilenceLimit without a word from the hub, which is what this test waits for after the
	// release. The limit must be longer than the time the messages are held, or a hello of that new session lands among them.
	sub := r.Names.AgentSub("laptop")
	settled(t, r)
	r.Mem.Hold(sub)
	for _, s := range []string{"a", "b", "c"} {
		r.Say(s)
	}
	r.Eventually("the three commands relayed", func() bool { return r.Hub.Stats().Relayed == 3 })
	r.Mem.Release(sub, func(n int) []int {
		if n != 3 {
			t.Errorf("%d messages were held", n)
		}
		return []int{1, 2} // "a" is lost: "b" and "c" wait for it in the queue
	})
	// the hub says nothing more, the agent sets up a new session by itself and drops what it was holding
	r.Eventually("both waiting commands reported", func() bool {
		return count(r, "a command that was waiting was dropped: the connection was re-established; send it again") == 2
	})
	time.Sleep(300 * time.Millisecond)
	if n := count(r, "a command that was waiting was dropped"); n != 2 {
		t.Fatalf("%d reports for two commands: %q", n, r.SentTexts())
	}
	if len(a.Exec.Texts()) != 0 {
		t.Fatalf("a command of the old session was typed: %q", a.Exec.Texts())
	}
	if a.Agent.Stats().Handshakes < 2 {
		t.Fatal("the session was never replaced")
	}
	// and the new session works
	r.Say("after")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
}

func count(r *testrig.Rig, sub string) int {
	n := 0
	for _, s := range r.SentTexts() {
		if bytes.Contains([]byte(s), []byte(sub)) {
			n++
		}
	}
	return n
}

// The agent reports a gap in the hub's numbers as a lost command. When the hub itself could not publish that command and has
// told the user, the report is noise: the hub leaves it out.
func TestTheAgentsReportOfACommandTheHubCouldNotPublishIsNotNews(t *testing.T) {
	r, a := one(t, testrig.Options{FaultyBus: true, Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour, ReorderWindow: 300 * time.Millisecond})
	toAgent := r.Names.ToAgent("laptop")
	var attempts atomic.Int32
	r.Faulty.FailPublish(func(topic string) error {
		if topic == toAgent && attempts.Add(1) <= 3 { // the three tries of the first command
			return errors.New("bus down")
		}
		return nil
	})
	r.Say("first")
	r.WaitSent("The message bus did not accept the message")
	r.Say("second")
	r.Eventually("the second command typed", func() bool { return len(a.Exec.Texts()) == 1 })
	if a.Agent.Stats().Lost != 1 {
		t.Fatalf("the agent found %d gaps, want the one of the first command", a.Agent.Stats().Lost)
	}
	r.Never("the agent's report of the gap in the chat", 600*time.Millisecond, func() bool { return r.Sent("did not arrive in time") })
	if n := count(r, "did not accept"); n != 1 {
		t.Fatalf("the failure was said %d times: %q", n, r.SentTexts())
	}
}

// A gap that the hub knows nothing about is still reported.
func TestALostCommandIsStillReportedWhenTheHubDidNotFailToPublishIt(t *testing.T) {
	r, a := one(t, testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour, ReorderWindow: 300 * time.Millisecond})
	n := 0
	r.Mem.SetDrop(func(p transportPublished) bool {
		if p.Topic == r.Names.ToAgent("laptop") && p.Attrs[wire.AttrKind] == wire.KindMsg {
			n++
			return n == 1 // the bus loses the first command after it was published
		}
		return false
	})
	r.Say("lost")
	r.Say("kept")
	r.Eventually("the second typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.WaitSent("command #1 did not arrive in time")
}

// The agent's pass over the spool is woken by the hub's receipts and by its own timer when a batch waits for its turn; the
// tick of the loop (half a second here) is not what paces a transfer.
func TestReceiptsAndTheTurnTimerDriveTheBatchesNotTheTickOfTheLoop(t *testing.T) {
	const tick = 500 * time.Millisecond
	r, a := one(t, testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour, MinTurn: 40 * time.Millisecond,
		Agent: func(name string, c *agentsvc.Config) { c.SpoolEvery = tick }})
	// 64 parts of one byte each: four batches of a window of sixteen, with nothing to slow them but the pace itself
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i)
	}
	toHub := r.Names.ToHub()
	before := len(r.Mem.Published(toHub)) // before the queueing: on a slow disk a tick of the loop can come while it goes on
	tr := filetx.NewID()
	for i := range data {
		if err := a.Spool.AddItem(spool.Item{Type: wire.TypePart, Body: filetx.EncodePart(data[i : i+1]), Meta: map[string]string{wire.MetaTransfer: tr, wire.MetaIndex: strconv.Itoa(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Spool.AddItem(spool.Item{Type: wire.TypeFile, Meta: map[string]string{wire.MetaTransfer: tr, wire.MetaParts: "64", wire.MetaName: "tiny.bin",
		wire.MetaSize: "64", wire.MetaSHA: filetx.SHA(data), wire.MetaKind: "document"}}); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(r.TG.Uploads()) == 0 {
		if first.IsZero() && len(r.Mem.Published(toHub)) > before {
			first = time.Now() // the first batch is out, at the first tick
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(r.TG.Uploads()) != 1 || first.IsZero() {
		t.Fatalf("the file did not arrive: %d uploads", len(r.TG.Uploads()))
	}
	// Four batches and the closing item take four turns of 40ms and the time of the receipts. If each had to wait for the tick
	// of the loop it would be a second and a half.
	if took := time.Since(first); took > 2*tick {
		t.Fatalf("the rest of the file took %v after the first batch: the batches waited for the tick of the loop", took)
	}
	if !bytes.Equal(r.TG.Uploads()[0].Data, data) {
		t.Fatal("the file is not what was queued")
	}
}
