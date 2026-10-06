package transport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func collect(t *testing.T, tr Transport, sub string, n int) ([]*Message, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var got []*Message
	done := make(chan struct{}, n)
	go tr.Receive(ctx, sub, func(_ context.Context, m *Message) {
		m.Ack()
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		done <- struct{}{}
	})
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatalf("timed out after %d of %d messages", i, n)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]*Message(nil), got...), cancel
}

func TestMemDeliversInOrderWithAttributes(t *testing.T) {
	m := NewMem()
	m.Bind("t", "s")
	for _, s := range []string{"a", "b", "c"} {
		if err := m.Publish(context.Background(), "t", []byte(s), map[string]string{"k": s}); err != nil {
			t.Fatal(err)
		}
	}
	got, cancel := collect(t, m, "s", 3)
	defer cancel()
	for i, want := range []string{"a", "b", "c"} {
		if string(got[i].Data) != want || got[i].Attrs["k"] != want || got[i].Attempt != 1 {
			t.Fatalf("message %d: %+v", i, got[i])
		}
	}
}

func TestMemFansOutToEverySubscription(t *testing.T) {
	m := NewMem()
	m.Bind("t", "s1")
	m.Bind("t", "s2")
	m.Publish(context.Background(), "t", []byte("x"), nil)
	a, c1 := collect(t, m, "s1", 1)
	b, c2 := collect(t, m, "s2", 1)
	c1()
	c2()
	if string(a[0].Data) != "x" || string(b[0].Data) != "x" {
		t.Fatal("fan-out failed")
	}
}

func TestMemUnknownTopicAndSubscription(t *testing.T) {
	m := NewMem()
	if err := m.Publish(context.Background(), "nope", nil, nil); !errors.Is(err, ErrNoTopic) {
		t.Fatalf("got %v", err)
	}
	if err := m.Receive(context.Background(), "nope", nil); err == nil {
		t.Fatal("receive on an unknown subscription succeeded")
	}
}

func TestMemDataIsCopied(t *testing.T) {
	m := NewMem()
	m.Bind("t", "s")
	buf := []byte("original")
	m.Publish(context.Background(), "t", buf, map[string]string{"k": "v"})
	copy(buf, "mutated!")
	got, cancel := collect(t, m, "s", 1)
	defer cancel()
	if string(got[0].Data) != "original" {
		t.Fatalf("publisher's buffer leaked into the queue: %q", got[0].Data)
	}
}

func TestMemNackRedelivers(t *testing.T) {
	m := NewMem()
	m.Bind("t", "s")
	m.Publish(context.Background(), "t", []byte("retry me"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := make(chan int, 10)
	go m.Receive(ctx, "s", func(_ context.Context, msg *Message) {
		attempts <- msg.Attempt
		if msg.Attempt < 3 {
			msg.Nack()
			return
		}
		msg.Ack()
	})
	for want := 1; want <= 3; want++ {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("attempt %d, want %d", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no attempt %d", want)
		}
	}
}

func TestMemDropInjectAndRecord(t *testing.T) {
	m := NewMem()
	m.Bind("t", "s")
	m.SetDrop(func(p Published) bool { return string(p.Data) == "lost" })
	m.Publish(context.Background(), "t", []byte("lost"), nil)
	m.Publish(context.Background(), "t", []byte("kept"), nil)
	m.Inject("s", []byte("forged"), map[string]string{"agent": "laptop"})
	got, cancel := collect(t, m, "s", 2)
	defer cancel()
	if string(got[0].Data) != "kept" || string(got[1].Data) != "forged" {
		t.Fatalf("%q %q", got[0].Data, got[1].Data)
	}
	if rec := m.Published("t"); len(rec) != 2 || string(rec[0].Data) != "lost" {
		t.Fatalf("recording wrong: %+v", rec)
	}
}

func TestMemHoldAndReorder(t *testing.T) {
	m := NewMem()
	m.Bind("t", "s")
	m.Hold("s")
	for _, s := range []string{"1", "2", "3"} {
		m.Publish(context.Background(), "t", []byte(s), nil)
	}
	m.Release("s", func(n int) []int { return []int{2, 0, 1} })
	got, cancel := collect(t, m, "s", 3)
	defer cancel()
	if string(got[0].Data)+string(got[1].Data)+string(got[2].Data) != "312" {
		t.Fatalf("reordering failed: %q%q%q", got[0].Data, got[1].Data, got[2].Data)
	}
}

func TestMemReceiveStopsOnCancel(t *testing.T) {
	m := NewMem()
	m.Bind("t", "s")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Receive(ctx, "s", func(context.Context, *Message) {}) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Receive did not stop")
	}
}

func TestMessageAckNackWithoutCallbacks(t *testing.T) {
	m := NewMessage("id", nil, nil, nil, nil)
	m.Ack()
	m.Nack() // must not panic
	acked := false
	m = NewMessage("id", nil, nil, func() { acked = true }, nil)
	m.Ack()
	if !acked {
		t.Fatal("ack callback not called")
	}
}
