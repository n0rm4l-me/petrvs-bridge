package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The pace of a transfer. The ratchet of each end takes a budget of new peer keys (ratchet.DHStepBurst, refilled at
// ratchet.DHStepPerSecond) and refuses a message that would overdraw it; the message is acknowledged to the bus and lost.
// A window of parts that are sent one by one as the answers come back makes the two ends take a new key for nearly every
// message. These tests move big files over the in-memory bus with the real ratchet and its real budget (nothing is
// overridden), with the latency of a network between the hub and the agent, and demand that nothing is refused.

const paceMaxFile = 64 << 20

// paceRig is a quiet rig (no heartbeats, which would spend the budget too) whose bus has the given one-way latency and
// whose hub and agent take files of up to paceMaxFile.
func paceRig(t *testing.T, latency time.Duration) (*testrig.Rig, string) {
	t.Helper()
	inbox := filepath.Join(t.TempDir(), "inbox")
	r := testrig.New(t, testrig.Options{FaultyBus: true, Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour,
		Hub: func(c *hubsvc.Config) { c.MaxFileBytes = paceMaxFile },
		Agent: func(name string, c *agentsvc.Config) {
			c.Ext.InboxDir = inbox
			c.Ext.MaxFileBytes = paceMaxFile
		}})
	r.Faulty.SetLatency(latency)
	r.StartAll()
	return r, inbox
}

// paceWait polls cond for up to d.
func paceWait(t *testing.T, r *testrig.Rig, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s\nchat so far: %q", what, r.SentTexts())
}

// paceQueue puts a file into the spool as `agent send-file` does: its parts, then the closing item.
func paceQueue(t *testing.T, sp *spool.Spool, name string, data []byte) {
	t.Helper()
	tr := filetx.NewID()
	chunks := filetx.Chunks(data)
	for i, c := range chunks {
		if err := sp.AddItem(spool.Item{Type: wire.TypePart, Body: filetx.EncodePart(c), Meta: map[string]string{wire.MetaTransfer: tr, wire.MetaIndex: strconv.Itoa(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	err := sp.AddItem(spool.Item{Type: wire.TypeFile, Meta: map[string]string{wire.MetaTransfer: tr, wire.MetaParts: strconv.Itoa(len(chunks)), wire.MetaName: name,
		wire.MetaSize: strconv.Itoa(len(data)), wire.MetaSHA: filetx.SHA(data), wire.MetaKind: filetx.Kind(name, int64(len(data)))}})
	if err != nil {
		t.Fatal(err)
	}
}

// paceUp moves a file of size bytes from the agent to the chat and checks that it arrived whole and that nothing was refused.
func paceUp(t *testing.T, r *testrig.Rig, size int, within time.Duration) {
	t.Helper()
	data := downBytes(size)
	a := r.Agents["laptop"]
	paceQueue(t, a.Spool, "dump.bin", data)
	paceWait(t, r, within, "the file in the chat", func() bool { return len(r.TG.Uploads()) >= 1 })
	if up := r.TG.Uploads()[0]; !bytes.Equal(up.Data, data) {
		t.Fatalf("the file in the chat differs from the one sent (%d bytes, want %d)", len(up.Data), len(data))
	}
	paceWait(t, r, 10*time.Second, "the queue of the agent empty", func() bool { return a.Spool.Len() == 0 })
}

// paceDown moves a file of size bytes from the chat to the agent's inbox and checks the saved bytes.
func paceDown(t *testing.T, r *testrig.Rig, id string, size int, within time.Duration) {
	t.Helper()
	data := downBytes(size)
	for i := range data {
		data[i] ^= byte(len(id)) // another file than the one going up
	}
	a := r.Agents["laptop"]
	r.TG.Serve(id, data)
	n := len(a.Exec.Texts())
	r.SayMedia(telegram.FileRef{ID: id, Name: id + ".bin", Size: int64(size)}, "")
	paceWait(t, r, within, "the line about the file", func() bool { return len(a.Exec.Texts()) > n })
	m := downLine.FindStringSubmatch(a.Exec.Texts()[n])
	if m == nil {
		t.Fatalf("not the line about a file: %q", a.Exec.Texts()[n])
	}
	got, err := os.ReadFile(m[2])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the saved file differs from the one sent (%d bytes, want %d)", len(got), len(data))
	}
}

// paceClean fails when any message was refused or any warning reached the chat.
func paceClean(t *testing.T, r *testrig.Rig) {
	t.Helper()
	if a, h := r.Agents["laptop"].Agent.Stats().Rejected, r.Hub.Stats().Rejected; a != 0 || h != 0 {
		t.Fatalf("messages were refused by the ratchets: the agent %d, the hub %d", a, h)
	}
	if r.Sent("⚠️") {
		t.Fatalf("the chat was told of a problem: %q", r.SentTexts())
	}
}

var paceLatencies = []time.Duration{0, 10 * time.Millisecond, 25 * time.Millisecond}

func TestFortyFiveMiBFromTheMachineToTheChatIsNeverRefusedByTheRatchet(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 45 MiB three times")
	}
	for _, lat := range paceLatencies {
		t.Run(fmt.Sprintf("latency %v", lat), func(t *testing.T) {
			r, _ := paceRig(t, lat)
			paceUp(t, r, 45<<20, 60*time.Second)
			paceClean(t, r)
		})
	}
}

func TestFortyFiveMiBFromTheChatToTheMachineIsNeverRefusedByTheRatchet(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 45 MiB three times")
	}
	for _, lat := range paceLatencies {
		t.Run(fmt.Sprintf("latency %v", lat), func(t *testing.T) {
			r, _ := paceRig(t, lat)
			paceDown(t, r, "big", 45<<20, 60*time.Second)
			paceClean(t, r)
		})
	}
}

func TestTwentyMiBEachWayAtTheSameTimeIsNeverRefusedByTheRatchet(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 40 MiB")
	}
	r, _ := paceRig(t, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		paceDown(t, r, "both", 20<<20, 60*time.Second)
	}()
	paceUp(t, r, 20<<20, 60*time.Second)
	<-done
	paceClean(t, r)
}

// The counts that were measured to fail: 150 and 160 parts (20 MiB) through the memory bus, in both directions.
func TestOneHundredFiftyAndOneHundredSixtyPartsAreNeverRefusedByTheRatchet(t *testing.T) {
	for _, parts := range []int{150, 160} {
		size := (parts-1)*wire.MaxChunk + 1000
		if got := len(filetx.Chunks(make([]byte, size))); got != parts {
			t.Fatalf("the test file has %d parts, want %d", got, parts)
		}
		t.Run(fmt.Sprintf("%d parts up", parts), func(t *testing.T) {
			r, _ := paceRig(t, 0)
			paceUp(t, r, size, 40*time.Second)
			paceClean(t, r)
		})
		t.Run(fmt.Sprintf("%d parts down", parts), func(t *testing.T) {
			r, _ := paceRig(t, 0)
			paceDown(t, r, "many", size, 40*time.Second)
			paceClean(t, r)
		})
	}
}

// The agent queues eight files before any of them is confirmed: all their parts go out before the first closing item, and
// the Assembler must take them all at once instead of leaving the later parts to the resend after a whole receipt timeout.
// The receipt timeout is a second here (the rig's own is 300ms) so that a resend cannot be mistaken for a slow pass.
func TestEightFilesQueuedAtOnceArriveWithoutAResend(t *testing.T) {
	r := testrig.New(t, testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour, MinTurn: 20 * time.Millisecond,
		Agent: func(name string, c *agentsvc.Config) { c.ReceiptTimeout = time.Second }})
	r.StartAll()
	a := r.Agents["laptop"]
	files := make([][]byte, 8)
	for i := range files {
		files[i] = downBytes(2*wire.MaxChunk + 100*(i+1)) // three parts each
		for j := range files[i] {
			files[i][j] ^= byte(i)
		}
	}
	toHub := r.Names.ToHub()
	before := len(r.Mem.Published(toHub))
	start := time.Now()
	for i, f := range files {
		paceQueue(t, a.Spool, fmt.Sprintf("f%d.bin", i), f)
	}
	paceWait(t, r, 10*time.Second, "all eight files in the chat", func() bool { return len(r.TG.Uploads()) == 8 })
	took := time.Since(start)
	// Three receipt timeouts would be three seconds; the parts of the later files used to wait for one each.
	if took > time.Second {
		t.Fatalf("the eight files took %v, more than a receipt timeout: a part had to be sent again", took)
	}
	paceWait(t, r, 5*time.Second, "the queue of the agent empty", func() bool { return a.Spool.Len() == 0 })
	// 8 files of 3 parts and a closing item each, every one sent once
	if n := len(r.Mem.Published(toHub)) - before; n != 8*4 {
		t.Fatalf("the agent published %d messages for 32 items: some were sent again", n)
	}
	got := map[string]bool{}
	for _, u := range r.TG.Uploads() {
		for i, f := range files {
			if u.Name == fmt.Sprintf("f%d.bin", i) && bytes.Equal(u.Data, f) {
				got[u.Name] = true
			}
		}
	}
	if len(got) != 8 {
		t.Fatalf("%d of the eight files arrived whole", len(got))
	}
	if r.Sent("⚠️") {
		t.Fatalf("%q", r.SentTexts())
	}
}
