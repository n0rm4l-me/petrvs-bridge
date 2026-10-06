package transport

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// emu connects to the Pub/Sub emulator (PUBSUB_EMULATOR_HOST) and skips the test when there is none.
func emu(t *testing.T) *PubSub {
	t.Helper()
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("PUBSUB_EMULATOR_HOST not set; run scripts/with-emulator.sh go test ./...")
	}
	ctx := context.Background()
	p, err := NewPubSub(ctx, "cb-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func names(t *testing.T) (topic, sub string) {
	base := strings.ToLower(unsafeName.ReplaceAllString(t.Name(), "-"))
	if len(base) > 40 {
		base = base[:40]
	}
	n := time.Now().UnixNano() % 1_000_000_000
	return fmt.Sprintf("%s-t-%d", base, n), fmt.Sprintf("%s-s-%d", base, n)
}

func setup(t *testing.T, p *PubSub, subs ...string) (topic string, subNames []string) {
	t.Helper()
	topic, first := names(t)
	ctx := context.Background()
	if err := p.EnsureTopic(ctx, topic); err != nil {
		t.Fatal(err)
	}
	if len(subs) == 0 {
		subs = []string{first}
	} else {
		for i := range subs {
			subs[i] = fmt.Sprintf("%s-%s", first, subs[i])
		}
	}
	for _, s := range subs {
		if err := p.EnsureSubscription(ctx, topic, s, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { p.Delete(context.Background(), topic, subs...) })
	return topic, subs
}

func TestPubSubPublishReceiveRoundTrip(t *testing.T) {
	p := emu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	data := []byte{0, 1, 2, 255, 254}
	if err := p.Publish(ctx, topic, data, map[string]string{"kind": "msg", "agent": "laptop"}); err != nil {
		t.Fatal(err)
	}
	got := make(chan *Message, 1)
	go p.Receive(ctx, subs[0], func(_ context.Context, m *Message) { m.Ack(); got <- m })
	select {
	case m := <-got:
		if string(m.Data) != string(data) || m.Attrs["kind"] != "msg" || m.Attrs["agent"] != "laptop" || m.ID == "" {
			t.Fatalf("%+v", m)
		}
	case <-ctx.Done():
		t.Fatal("no message")
	}
}

func TestPubSubFanOutToTwoSubscriptions(t *testing.T) {
	p := emu(t)
	topic, subs := setup(t, p, "a", "b")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p.Publish(ctx, topic, []byte("x"), nil)
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			cctx, c := context.WithCancel(ctx)
			defer c()
			p.Receive(cctx, s, func(_ context.Context, m *Message) { m.Ack(); c() })
		}(s)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("not every subscription received the message")
	}
}

func TestPubSubNackRedelivers(t *testing.T) {
	p := emu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p.Publish(ctx, topic, []byte("retry"), nil)
	var mu sync.Mutex
	seen := 0
	done := make(chan struct{})
	go p.Receive(ctx, subs[0], func(_ context.Context, m *Message) {
		mu.Lock()
		seen++
		n := seen
		mu.Unlock()
		if n == 1 {
			m.Nack()
			return
		}
		m.Ack()
		close(done)
	})
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("a nacked message was not redelivered")
	}
}

func TestPubSubLargeMessage(t *testing.T) {
	p := emu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i)
	}
	if err := p.Publish(ctx, topic, big, nil); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go p.Receive(ctx, subs[0], func(_ context.Context, m *Message) { m.Ack(); got <- m.Data })
	select {
	case d := <-got:
		if len(d) != len(big) || d[1000] != big[1000] {
			t.Fatal("large message corrupted")
		}
	case <-ctx.Done():
		t.Fatal("no message")
	}
}

func TestPubSubPublishToMissingTopicFails(t *testing.T) {
	p := emu(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Publish(ctx, "no-such-topic-xyz", []byte("x"), nil); err == nil {
		t.Fatal("publishing to a missing topic succeeded")
	}
}

func TestPubSubReceiveStopsOnCancel(t *testing.T) {
	p := emu(t)
	_, subs := setup(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Receive(ctx, subs[0], func(context.Context, *Message) {}) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Receive did not stop after cancel")
	}
}

func TestPubSubEnsureIsIdempotent(t *testing.T) {
	p := emu(t)
	topic, subs := setup(t, p)
	ctx := context.Background()
	if err := p.EnsureTopic(ctx, topic); err != nil {
		t.Fatalf("second EnsureTopic: %v", err)
	}
	if err := p.EnsureSubscription(ctx, topic, subs[0], 10*time.Second); err != nil {
		t.Fatalf("second EnsureSubscription: %v", err)
	}
}
