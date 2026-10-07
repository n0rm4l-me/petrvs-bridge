package hubsvc_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The hub paces its batches of parts by its own clock, so a test that moves the clock moves the pace, and the time a test
// spends waiting is not what decides.
func TestTheBatchesOfPartsFollowTheClockOfTheHub(t *testing.T) {
	clock := testrig.NewClock()
	o := fdQuiet()
	o.Clock = clock
	f := fdNew(t, o)
	f.StartAll()
	g := f.fdGate()               // counts what the hub sends to the agent; the agent answers at once
	size := 39*wire.MaxChunk + 10 // forty parts: 16, 16 and 8
	data := fdBytes(size, 3)
	f.TG.Serve("big", data)
	base := g.sent.Load()
	f.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: int64(size)}, "clocked")

	f.Eventually("the first batch", func() bool { return g.sent.Load()-base == 16 })
	f.Never("a second batch although the clock did not move", 700*time.Millisecond, func() bool { return g.sent.Load()-base > 16 })
	clock.Advance(100 * time.Millisecond) // a turn is 250ms
	f.Never("a second batch 100ms into the turn", 300*time.Millisecond, func() bool { return g.sent.Load()-base > 16 })
	clock.Advance(200 * time.Millisecond)
	f.Eventually("the second batch", func() bool { return g.sent.Load()-base == 32 })
	f.Never("a third batch although the clock did not move", 500*time.Millisecond, func() bool { return g.sent.Load()-base > 32 })
	clock.Advance(300 * time.Millisecond)

	_, caption, got := fdTypedLine(t, f, 1)
	if caption != "clocked" || !bytes.Equal(got, data) {
		t.Fatalf("%q, %d bytes", caption, len(got))
	}
	if n := g.sent.Load() - base; n != 41 {
		t.Fatalf("%d messages to the agent", n)
	}
}

// A turn belongs to the agent, not to the transfer: two files on their way to one agent take turns.
func TestTwoTransfersToOneAgentShareTheTurns(t *testing.T) {
	clock := testrig.NewClock()
	o := fdQuiet()
	o.Clock = clock
	f := fdNew(t, o)
	f.StartAll()
	g := f.fdGate()
	a, b := fdBytes(5*wire.MaxChunk, 4), fdBytes(5*wire.MaxChunk, 5)
	f.TG.Serve("a", a)
	f.TG.Serve("b", b)
	base := g.sent.Load()
	f.SayMedia(telegram.FileRef{ID: "a", Name: "a.bin", Size: int64(len(a))}, "a")
	f.SayMedia(telegram.FileRef{ID: "b", Name: "b.bin", Size: int64(len(b))}, "b")
	f.Eventually("the parts of one file", func() bool { return g.sent.Load()-base >= 5 })
	f.Never("the other file's parts in the same turn", 600*time.Millisecond, func() bool { return g.sent.Load()-base > 6 }) // 5 parts and at most the closing command
	clock.Advance(300 * time.Millisecond)
	f.Eventually("both files typed", func() bool { return len(f.Agents["laptop"].Exec.Texts()) == 2 })
}
