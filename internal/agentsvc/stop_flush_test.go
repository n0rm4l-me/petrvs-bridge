package agentsvc

import (
	"context"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// When the agent is told to stop (the machine is shutting down, the service is restarted), the answer that a hook queued a
// moment before must reach the bus, not wait in the spool for the next start.

// messagesOut counts the sealed messages that reached the hub's topic; the hellos of the handshake do not count.
func (r *upRig) messagesOut() (n int) {
	for _, p := range r.mem.Published(r.names.ToHub()) {
		if p.Attrs[wire.AttrKind] == wire.KindMsg {
			n++
		}
	}
	return n
}

func stoppedContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestWhatIsQueuedGoesOutWhenTheAgentStops(t *testing.T) {
	r := newUpRig(t)
	r.add(t, wire.TypeReply, "the answer of the last turn")
	r.a.flushSpool(stoppedContext())
	if got := r.messagesOut(); got != 1 {
		t.Fatalf("%d messages reached the bus, want 1", got)
	}
	if got := r.inFlight(); got != 1 {
		t.Fatalf("%d messages wait for a receipt, want 1: the file must stay until the hub confirms it", got)
	}
	if r.sp.Len() != 1 {
		t.Fatal("the file was removed before the hub's receipt")
	}
}

func TestStoppingDoesNotSendAgainWhatIsOutAlready(t *testing.T) {
	r := newUpRig(t)
	r.add(t, wire.TypeReply, "already sent")
	r.a.spoolTick(context.Background())
	r.now = r.now.Add(time.Second) // well within the receipt timeout
	r.add(t, wire.TypeReply, "queued a moment ago")
	r.a.flushSpool(stoppedContext())
	if got := r.messagesOut(); got != 2 {
		t.Fatalf("%d messages reached the bus, want 2 (each once)", got)
	}
}

func TestStoppingIgnoresThePaceOfTheTurns(t *testing.T) {
	r := newUpRig(t)
	r.a.cfg.MinTurn = time.Hour
	r.add(t, wire.TypeReply, "one")
	r.a.spoolTick(context.Background())
	r.add(t, wire.TypeReply, "two, queued right after the batch")
	r.a.spoolTick(context.Background())
	if got := r.messagesOut(); got != 1 {
		t.Fatalf("the pace let %d messages out, want 1", got)
	}
	r.a.flushSpool(stoppedContext())
	if got := r.messagesOut(); got != 2 {
		t.Fatalf("%d messages reached the bus after stopping, want 2: nothing will be sent after this pass", got)
	}
}

func TestStoppingWithoutASessionSendsNothing(t *testing.T) {
	r := newUpRig(t)
	r.a.sess = nil
	r.add(t, wire.TypeReply, "no one to tell")
	r.a.flushSpool(stoppedContext())
	if got := r.messagesOut(); got != 0 {
		t.Fatalf("%d messages were sent with no session", got)
	}
	if r.sp.Len() != 1 {
		t.Fatal("the message must stay queued")
	}
}

func TestTheFlushCanBeTurnedOff(t *testing.T) {
	r := newUpRig(t)
	r.a.cfg.StopFlush = -1
	r.add(t, wire.TypeReply, "stays")
	r.a.flushSpool(stoppedContext())
	if got := r.messagesOut(); got != 0 {
		t.Fatalf("%d messages were sent with the flush off", got)
	}
}

// Run itself makes the pass when its context ends, whatever the timers of the loops are doing.
func TestRunFlushesTheSpoolWhenItsContextEnds(t *testing.T) {
	r := newUpRig(t)
	r.mem.Bind(r.names.ToAgent("laptop"), r.names.AgentSub("laptop"))
	r.a.cfg.SpoolEvery = time.Hour // no tick of the loop may do the work
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.a.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let the loops start
	r.add(t, wire.TypeReply, "the last words")
	if got := r.messagesOut(); got != 0 {
		t.Fatalf("%d messages went out before the stop", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if got := r.messagesOut(); got != 1 {
		t.Fatalf("%d messages reached the bus when Run stopped, want 1", got)
	}
}
