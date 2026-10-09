package agentsvc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// flakyBus fails Receive a given number of times, then waits for ctx like a healthy subscription.
type flakyBus struct {
	failures int32
	calls    atomic.Int32
}

func (b *flakyBus) Publish(context.Context, string, []byte, map[string]string) error { return nil }
func (b *flakyBus) Close() error                                                     { return nil }
func (b *flakyBus) Receive(ctx context.Context, _ string, _ func(context.Context, *transport.Message)) error {
	if b.calls.Add(1) <= b.failures {
		return errors.New("rpc error: code = Unauthenticated desc = credentials: lookup sts.mtls.googleapis.com: no such host")
	}
	<-ctx.Done()
	return ctx.Err()
}

func newReceiver(bus transport.Transport) *Agent {
	return &Agent{
		cfg: Config{Transport: bus, Names: wire.Names{Prefix: "t"}, Identity: &identity.Identity{Name: "laptop"}, ReceiveRetry: 5 * time.Millisecond},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The agent runs on a laptop that sleeps and moves between networks, and nothing restarts it: a failed subscription call is
// not the end of it. When a token refresh cannot resolve a name, Receive returns the error, and without a retry the agent
// would stop for good.
func TestAFailedSubscriptionIsReadAgainAndTheAgentGoesOn(t *testing.T) {
	bus := &flakyBus{failures: 3}
	a := newReceiver(bus)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.receive(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for bus.calls.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d tries", bus.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("receive returned %v while the bus is up again", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a stop is not an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receive did not return after the stop")
	}
}

func TestTheWaitBetweenTriesGrowsButIsCutShortByAStop(t *testing.T) {
	bus := &flakyBus{failures: 1 << 20}
	a := newReceiver(bus)
	a.cfg.ReceiveRetry = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.receive(ctx) }()
	time.Sleep(400 * time.Millisecond) // waits of 50, 100, 200 ms: about four tries, not a busy loop
	if n := bus.calls.Load(); n < 3 || n > 6 {
		t.Fatalf("%d tries in 400ms with doubling waits", n)
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a stop did not end the wait")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("a stop waited for the retry timer")
	}
}
