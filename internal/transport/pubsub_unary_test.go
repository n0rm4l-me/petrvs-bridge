package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// unaryEmu is the emulator connection that reads with short calls.
func unaryEmu(t *testing.T) *PubSub {
	t.Helper()
	p := emu(t)
	p.SetPullMode(PullUnary)
	return p
}

func TestUnaryPullRoundTrip(t *testing.T) {
	p := unaryEmu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data := []byte{0, 1, 2, 255, 254}
	if err := p.Publish(ctx, topic, data, map[string]string{"kind": "msg", "agent": "laptop"}); err != nil {
		t.Fatal(err)
	}
	got := make(chan *Message, 1)
	go p.Receive(ctx, subs[0], func(_ context.Context, m *Message) { m.Ack(); got <- m })
	select {
	case m := <-got:
		if string(m.Data) != string(data) || m.Attrs["kind"] != "msg" || m.Attrs["agent"] != "laptop" || m.ID == "" || m.Attempt != 1 || m.PublishedAt.IsZero() {
			t.Fatalf("%+v", m)
		}
	case <-ctx.Done():
		t.Fatal("no message")
	}
}

func TestUnaryPullDeliversAMessagePublishedWhileItWaits(t *testing.T) {
	p := unaryEmu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := make(chan struct{}, 1)
	go p.Receive(ctx, subs[0], func(_ context.Context, m *Message) { m.Ack(); got <- struct{}{} })
	time.Sleep(time.Second) // the call is waiting by now
	if err := p.Publish(ctx, topic, []byte("late"), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal("a message published while the call waited was not delivered")
	}
}

func TestUnaryPullNackRedeliversAtOnce(t *testing.T) {
	p := unaryEmu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second) // the ack deadline of the subscription is 10 s
	defer cancel()
	p.Publish(ctx, topic, []byte("retry"), nil)
	var seen atomic.Int32
	done := make(chan struct{})
	go p.Receive(ctx, subs[0], func(_ context.Context, m *Message) {
		if seen.Add(1) == 1 {
			m.Nack()
			return
		}
		m.Ack()
		close(done)
	})
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("a nacked message was not given back before its lease ran out")
	}
}

func TestUnaryPullManyMessagesEachOnceAndAckedWhenStopped(t *testing.T) {
	p := unaryEmu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const n = 100 // more than the messages held at once
	for i := 0; i < n; i++ {
		if err := p.Publish(ctx, topic, []byte(fmt.Sprint(i)), nil); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- p.Receive(rctx, subs[0], func(_ context.Context, m *Message) {
			time.Sleep(20 * time.Millisecond)
			m.Ack()
			mu.Lock()
			seen[string(m.Data)]++
			c := len(seen)
			mu.Unlock()
			if c == n {
				stop()
			}
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("got %d of %d", len(seen), n)
	}
	mu.Lock()
	for k, v := range seen {
		if v != 1 {
			t.Errorf("message %s delivered %d times", k, v)
		}
	}
	mu.Unlock()
	// The acknowledgements were sent on the way out: a new reader finds nothing left to take.
	again := make(chan *Message, 1)
	c2, stop2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop2()
	p.Receive(c2, subs[0], func(_ context.Context, m *Message) { m.Ack(); again <- m })
	select {
	case m := <-again:
		t.Fatalf("message %q came back after it was acknowledged", m.Data)
	default:
	}
}

func TestUnaryPullStopsOnCancelAndWaitsForTheHandler(t *testing.T) {
	p := unaryEmu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p.Publish(ctx, topic, []byte("x"), nil)
	rctx, stop := context.WithCancel(ctx)
	started := make(chan struct{})
	var finished atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- p.Receive(rctx, subs[0], func(_ context.Context, m *Message) {
			close(started)
			time.Sleep(500 * time.Millisecond)
			finished.Store(true)
			m.Ack()
		})
	}()
	<-started
	stop()
	select {
	case err := <-done:
		if err != nil || !finished.Load() {
			t.Fatalf("err=%v finished=%v: Receive returned before the handler did", err, finished.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not stop after cancel")
	}
}

func TestUnaryPullReportsAMissingSubscription(t *testing.T) {
	p := unaryEmu(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Receive(ctx, "no-such-subscription-xyz", func(context.Context, *Message) {}); err == nil {
		t.Fatal("reading a missing subscription ended without an error")
	}
}

// The leases need no bus.

func TestLeasesSendAcksAndNacksAndExtendOnlyWhatIsHeld(t *testing.T) {
	l := &leases{inflight: map[string]time.Time{}}
	l.add("a")
	l.add("b")
	l.add("c")
	l.ack("a")
	l.nack("b")
	var mu sync.Mutex
	var acked, nacked, extended []string
	ack := func(_ context.Context, ids []string) error {
		mu.Lock()
		acked = append(acked, ids...)
		mu.Unlock()
		return nil
	}
	modify := func(_ context.Context, ids []string, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		if d == 0 {
			nacked = append(nacked, ids...)
		} else {
			extended = append(extended, ids...)
		}
		return nil
	}
	l.send(context.Background(), ack, modify)
	l.extend(context.Background(), modify)
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(acked) != "[a]" || fmt.Sprint(nacked) != "[b]" || fmt.Sprint(extended) != "[c]" {
		t.Fatalf("acked %v nacked %v extended %v", acked, nacked, extended)
	}
}

func TestLeasesRetryAFailedAckUntilItIsTooOld(t *testing.T) {
	l := &leases{inflight: map[string]time.Time{}}
	l.add("a")
	l.ack("a")
	fail := func(context.Context, []string) error { return errors.New("network") }
	modify := func(context.Context, []string, time.Duration) error { return nil }
	l.send(context.Background(), fail, modify)
	if len(l.acks) != 1 {
		t.Fatalf("a failed acknowledgement was not kept: %v", l.acks)
	}
	l.acks[0].since = time.Now().Add(-2 * unaryAckGiveUp)
	l.send(context.Background(), fail, modify)
	if len(l.acks) != 0 {
		t.Fatalf("an acknowledgement older than its lease was kept: %v", l.acks)
	}
}

func TestLeasesStopExtendingAHandlerThatNeverAnswers(t *testing.T) {
	l := &leases{inflight: map[string]time.Time{"old": time.Now().Add(-2 * unaryMaxLease), "new": time.Now()}}
	var got []string
	l.extend(context.Background(), func(_ context.Context, ids []string, _ time.Duration) error { got = append(got, ids...); return nil })
	if fmt.Sprint(got) != "[new]" {
		t.Fatalf("extended %v", got)
	}
}
