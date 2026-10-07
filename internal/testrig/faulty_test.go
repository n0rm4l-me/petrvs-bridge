package testrig

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
)

// receiveTwo starts Receive on a bus with two messages waiting and returns how many handlers were inside the handler
// together at the most. A handler leaves only when a second one has entered or the wait is over.
func receiveTwo(t *testing.T, set func(b *FaultyBus)) int32 {
	t.Helper()
	mem := transport.NewMem()
	mem.Bind("topic", "sub")
	b := &FaultyBus{Transport: mem, closed: make(chan struct{})}
	defer b.Stop()
	var in, most atomic.Int32
	done := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		b.Receive(ctx, "sub", func(_ context.Context, m *transport.Message) {
			n := in.Add(1)
			for {
				old := most.Load()
				if n <= old || most.CompareAndSwap(old, n) {
					break
				}
			}
			deadline := time.Now().Add(300 * time.Millisecond)
			for in.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			in.Add(-1)
			m.Ack()
			done <- struct{}{}
		})
	}()
	time.Sleep(50 * time.Millisecond) // Receive is running before the setting is made, as in a rig that is already started
	set(b)
	for i := 0; i < 2; i++ {
		if err := mem.Publish(ctx, "topic", []byte("x"), nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a message was not handled")
		}
	}
	return most.Load()
}

func TestConcurrentSetAfterReceiveStartedTakesEffect(t *testing.T) {
	if got := receiveTwo(t, func(b *FaultyBus) { b.SetConcurrent(true) }); got != 2 {
		t.Fatalf("%d handlers at a time: the setting made after Receive started was ignored", got)
	}
	if got := receiveTwo(t, func(b *FaultyBus) {}); got != 1 {
		t.Fatalf("%d handlers at a time on a bus that is not concurrent", got)
	}
}

func TestOverlapCountsHandlersThatRanTogether(t *testing.T) {
	mem := transport.NewMem()
	mem.Bind("topic", "sub")
	b := &FaultyBus{Transport: mem, closed: make(chan struct{})}
	defer b.Stop()
	b.SetConcurrent(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen atomic.Int32
	go b.Receive(ctx, "sub", func(_ context.Context, m *transport.Message) {
		seen.Add(1)
		time.Sleep(100 * time.Millisecond)
		m.Ack()
	})
	for i := 0; i < 3; i++ {
		mem.Publish(ctx, "topic", []byte("x"), nil)
	}
	deadline := time.Now().Add(5 * time.Second)
	for seen.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got := b.Overlap(); got != 3 {
		t.Fatalf("overlap %d, want 3", got)
	}
}

func TestLatencyDelaysDeliveryButKeepsOrder(t *testing.T) {
	mem := transport.NewMem()
	mem.Bind("topic", "sub")
	b := &FaultyBus{Transport: mem, closed: make(chan struct{})}
	defer b.Stop()
	b.SetLatency(80 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 10)
	go b.Receive(ctx, "sub", func(_ context.Context, m *transport.Message) { got <- string(m.Data); m.Ack() })
	start := time.Now()
	for _, s := range []string{"a", "b", "c"} {
		if err := b.Publish(ctx, "topic", []byte(s), nil); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > 40*time.Millisecond {
		t.Fatalf("Publish waited %v: the sender must not be held by the latency", took)
	}
	for _, want := range []string{"a", "b", "c"} {
		select {
		case s := <-got:
			if s != want {
				t.Fatalf("got %q, want %q", s, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("nothing arrived")
		}
	}
	if took := time.Since(start); took < 70*time.Millisecond {
		t.Fatalf("the messages arrived after %v, before the latency", took)
	}
}
