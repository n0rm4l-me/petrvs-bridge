package agentsvc

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The pace of the spool. The hub takes a limited number of new keys a second (ratchet.DHStepBurst and DHStepPerSecond); a
// batch of messages and the receipts for it cost it one, so the agent sends a batch at most once in MinTurn, as much as
// fits in the window, and the messages of a batch are all sealed before the first is published.

// confirmN plays the hub's receipt for the n oldest messages that were sent.
func (r *upRig) confirmN(t *testing.T, n int) {
	t.Helper()
	entries, err := r.sp.List()
	if err != nil {
		t.Fatal(err)
	}
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for _, e := range entries {
		if n == 0 {
			return
		}
		if w, ok := r.a.awaiting["sp-"+e.Name]; ok {
			delete(r.a.awaiting, "sp-"+e.Name)
			r.sp.Remove(w.file)
			n--
		}
	}
}

func (r *upRig) inFlight() int {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	return len(r.a.awaiting)
}

func TestABatchGoesOutOnlyOnceInMinTurn(t *testing.T) {
	r := newUpRig(t)
	r.a.cfg.MinTurn = 250 * time.Millisecond
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		r.add(t, wire.TypeReply, fmt.Sprint("answer ", i))
	}
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != 3 {
		t.Fatalf("the first batch sent %d", got)
	}
	r.confirm(t)
	r.add(t, wire.TypeReply, "one more")
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != 0 {
		t.Fatalf("a second batch went out at once (%d sent): the hub's budget takes one turn in a quarter of a second", got)
	}
	r.now = r.now.Add(249 * time.Millisecond)
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != 0 {
		t.Fatalf("a batch went out 249ms after the one before")
	}
	r.now = r.now.Add(2 * time.Millisecond)
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != 1 {
		t.Fatalf("the batch did not go out when its turn came: %d", got)
	}
}

// The wake-up for a batch that waits for its turn is a timer, so that it does not wait for the next tick of the loop.
func TestAThrottledPassWakesItselfWhenTheTurnIsOpen(t *testing.T) {
	r := newUpRig(t)
	r.a.cfg.MinTurn = 60 * time.Millisecond
	r.a.cfg.Now = time.Now // the real clock, which the timer measures
	ctx := context.Background()
	r.add(t, wire.TypeReply, "first")
	r.a.spoolTick(ctx)
	r.confirm(t)
	r.add(t, wire.TypeReply, "second")
	r.a.spoolTick(ctx) // throttled
	deadline := time.After(2 * time.Second)
	for r.inFlight() == 0 {
		select {
		case <-r.a.spoolWake: // what the loop of the agent does with it: a pass
			r.a.spoolTick(ctx)
		case <-deadline:
			t.Fatal("nothing woke the spool pass although a message waits for its turn")
		}
	}
}

func TestABatchWaitsUntilHalfOfTheWindowIsFreeOrEverythingFits(t *testing.T) {
	r := newUpRig(t)
	r.a.cfg.MinTurn = 100 * time.Millisecond
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		r.add(t, wire.TypePart, fmt.Sprint("part ", i))
	}
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != spoolWindow {
		t.Fatalf("the first batch sent %d, want the whole window of %d", got, spoolWindow)
	}
	// the receipts come in a burst; a pass that starts at the first of them must not use up the turn for one message
	r.now = r.now.Add(time.Second)
	r.confirmN(t, 1)
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != spoolWindow-1 {
		t.Fatalf("a batch of one went out after the first receipt (%d in flight)", got)
	}
	r.confirmN(t, 6) // seven in all: still under half the window
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != spoolWindow-7 {
		t.Fatalf("a batch went out with less than half of the window free (%d in flight)", got)
	}
	r.confirmN(t, 1) // eight
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != spoolWindow {
		t.Fatalf("the batch of eight did not go out (%d in flight)", got)
	}
	// what is left fits in what is free, so it is not held back for half a window
	r.confirm(t)
	r.now = r.now.Add(time.Second)
	for r.sp.Len() > 3 {
		entries, _ := r.sp.List()
		r.sp.Remove(entries[0].Name)
	}
	r.a.spoolTick(ctx)
	if got := r.inFlight(); got != 3 {
		t.Fatalf("the last three were not sent (%d)", got)
	}
}

// hookTransport runs a function after every publish to the hub.
type hookTransport struct {
	transport.Transport
	mu   sync.Mutex
	hook func(n int)
	n    int
}

func (h *hookTransport) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	err := h.Transport.Publish(ctx, topic, data, attrs)
	h.mu.Lock()
	h.n++
	n, hook := h.n, h.hook
	h.mu.Unlock()
	if err == nil && hook != nil {
		hook(n)
	}
	return err
}

// The ratchet sends with a new key each time it has received one. The hub answers at once, so an answer can arrive while a
// batch is still going out; if the messages were sealed one by one each would carry whatever key the ratchet had by then,
// and the hub would take a step of its budget for each. All of a batch must carry one key.
func TestAllTheMessagesOfABatchCarryOneRatchetKeyEvenIfTheHubAnswersMeanwhile(t *testing.T) {
	r := newUpRig(t)
	ctx := context.Background()
	hubName := r.a.cfg.Hub.Name
	// a real pair of ratchets instead of the one of the rig
	var sk [32]byte
	rand.Read(sk[:])
	hubKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentR, err := ratchet.NewInitiator(sk, hubKey.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	hubR := ratchet.NewResponder(sk, hubKey)
	sid := [8]byte{1, 2, 3}
	r.a.sess = &session{sid: sid, r: agentR}
	name := r.a.cfg.Identity.Name
	hook := &hookTransport{Transport: r.mem}
	hook.hook = func(n int) {
		// the hub reads what it got and answers (with a key of its own that is new to the agent after the first message)
		published := r.mem.Published(r.names.ToHub())
		last := published[len(published)-1]
		if _, err := hubR.Decrypt(last.Data[9:], wire.AD(name, hubName, sid)); err != nil {
			t.Errorf("the hub could not open message %d: %v", n, err)
			return
		}
		reply, err := hubR.Encrypt([]byte("{}"), wire.AD(hubName, name, sid))
		if err != nil {
			t.Errorf("hub: %v", err)
			return
		}
		if _, err := agentR.Decrypt(reply, wire.AD(hubName, name, sid)); err != nil {
			t.Errorf("the agent could not open the hub's answer: %v", err)
		}
	}
	r.a.cfg.Transport = hook
	for i := 0; i < 6; i++ {
		r.add(t, wire.TypeReply, fmt.Sprint("answer ", i))
	}
	r.a.spoolTick(ctx)
	published := r.mem.Published(r.names.ToHub())
	if len(published) != 6 {
		t.Fatalf("%d messages went out", len(published))
	}
	keys := map[string]int{}
	for _, p := range published {
		keys[string(p.Data[9:41])]++
	}
	if len(keys) != 1 {
		t.Fatalf("the six messages of one batch carry %d different ratchet keys: the hub would take a step of its budget for each", len(keys))
	}
}

// A file that the lock holds back, a bad file, an error of the bus: whatever happens to a batch, what was not sent is not
// registered as waiting for a receipt.
func TestAMessageThatCouldNotBePublishedIsNotWaitedFor(t *testing.T) {
	r := newUpRig(t)
	hook := &failingTransport{Transport: r.mem, failAt: 3}
	r.a.cfg.Transport = hook
	for i := 0; i < 5; i++ {
		r.add(t, wire.TypeReply, fmt.Sprint("answer ", i))
	}
	r.a.spoolTick(context.Background())
	if got := r.inFlight(); got != 2 {
		t.Fatalf("%d messages are waited for; two were published before the bus failed", got)
	}
	entries, _ := r.sp.List()
	if len(entries) != 5 {
		t.Fatalf("%d messages left in the spool", len(entries))
	}
}

type failingTransport struct {
	transport.Transport
	n, failAt int
}

func (f *failingTransport) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	f.n++
	if f.n >= f.failAt {
		return errors.New("bus down")
	}
	return f.Transport.Publish(ctx, topic, data, attrs)
}

// The closing item of a file waits for the receipts of its own parts, not for every message that is on its way: a pass
// comes a few times a second now, and a closing item that waited for all of them would take a pass for each file.
func TestTheClosingItemsOfSeveralFilesGoOutInOnePass(t *testing.T) {
	r := newUpRig(t)
	ctx := context.Background()
	add := func(typ wire.Type, tr string) {
		if err := r.sp.AddItem(spool.Item{Type: typ, Body: "x", Meta: map[string]string{wire.MetaTransfer: tr}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, tr := range []string{"a", "b", "c"} {
		add(wire.TypePart, tr)
		add(wire.TypeFile, tr)
	}
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "part,part,part," {
		t.Fatalf("the first pass sent %q; the closing items wait for their parts", got)
	}
	r.confirm(t)
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "file,file,file," {
		t.Fatalf("the second pass sent %q; every closing item was free to go", got)
	}
}

func TestAClosingItemIsHeldBackOnlyByItsOwnTransfer(t *testing.T) {
	r := newUpRig(t)
	r.a.mu.Lock()
	r.a.awaiting["sp-other.json"] = awaitingReceipt{file: "other.json", sentAt: r.now, transfer: "someone else"}
	r.a.mu.Unlock()
	if err := r.sp.AddItem(spool.Item{Type: wire.TypeFile, Body: "x", Meta: map[string]string{wire.MetaTransfer: "mine"}}); err != nil {
		t.Fatal(err)
	}
	r.a.spoolTick(context.Background())
	if got := types(r.sent(t)...); !strings.Contains(got, "file") {
		t.Fatalf("a closing item was held back by the parts of another transfer: %q", got)
	}
}

// The spool refuses an item the hub would refuse; a bad file on disk is set aside when the pass reads it and does not take
// one of the places of the window, however many there are.
func TestBadFilesInTheSpoolDoNotTakeThePlacesOfTheWindow(t *testing.T) {
	r := newUpRig(t)
	for i := 0; i < spoolWindow+4; i++ { // more than the window: twelve such files used to stop all replies
		bad := fmt.Sprintf(`{"type":"notice","body":"b","buttons":[{"l":%q,"k":"1"}]}`, strings.Repeat("x", 41))
		r.writeSpoolFile(t, fmt.Sprintf("%020d-1-aaaa%04d.json", i, i), bad)
	}
	r.add(t, wire.TypeReply, "the answer the user waits for")
	r.a.spoolTick(context.Background())
	if got := types(r.sent(t)...); got != "reply," {
		t.Fatalf("the reply did not go out behind the bad files: %q", got)
	}
	if n := r.sp.Len(); n != 1 {
		t.Fatalf("%d files are left in the queue, want only the reply", n)
	}
}
