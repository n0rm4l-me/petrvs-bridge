package hubsvc_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// What the hub tells a probe about itself, and how it stops.

// beacon records what the hub says about its health.
type beacon struct {
	mu      sync.Mutex
	alive   int
	ready   bool
	readies []bool
}

func (b *beacon) Alive() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.alive++
	return nil
}

func (b *beacon) Ready(ok bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ready = ok
	b.readies = append(b.readies, ok)
	return nil
}

func (b *beacon) state() (alive int, ready bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alive, b.ready
}

func (b *beacon) history() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.readies...)
}

// pollChat is the fake Telegram whose polling can be made to fail, or to ignore the end of its context.
type pollChat struct {
	*telegram.Fake
	err     atomic.Pointer[error]
	stuck   atomic.Bool   // GetUpdates ignores ctx and waits for release
	release chan struct{} // closed by the test
}

func (c *pollChat) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]telegram.Update, error) {
	if c.stuck.Load() {
		<-c.release
		return nil, ctx.Err()
	}
	if e := c.err.Load(); e != nil {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
		}
		return nil, *e
	}
	return c.Fake.GetUpdates(ctx, offset, timeout)
}

func healthRig(t *testing.T) (*testrig.Rig, *beacon, *pollChat) {
	t.Helper()
	b := &beacon{}
	c := &pollChat{release: make(chan struct{})}
	r := testrig.New(t, testrig.Options{
		WrapTelegram: func(f *telegram.Fake) telegram.Client { c.Fake = f; return c },
		Hub: func(cfg *hubsvc.Config) {
			cfg.Health, cfg.HealthEvery, cfg.HealthSlack = b, 20*time.Millisecond, 150*time.Millisecond
			cfg.PollTimeout, cfg.ShutdownTimeout = 100*time.Millisecond, 400*time.Millisecond
		},
	})
	t.Cleanup(func() { close(c.release) })
	return r, b, c
}

func TestTheHubIsAliveAndReadyOnceItPollsAndListens(t *testing.T) {
	r, b, _ := healthRig(t)
	r.StartAll()
	r.Eventually("the hub to say that it is alive and ready", func() bool {
		alive, ready := b.state()
		return alive > 0 && ready
	})
}

func TestTheHubIsNotReadyWhileTelegramFailsButItIsAliveAndBecomesReadyAgain(t *testing.T) {
	r, b, c := healthRig(t)
	r.StartAll()
	r.Eventually("ready at first", func() bool { _, ready := b.state(); return ready })

	boom := errors.New("telegram is down")
	c.err.Store(&boom)
	r.Eventually("not ready once the last good poll is old", func() bool { _, ready := b.state(); return !ready })
	before, _ := b.state()
	r.Eventually("still alive while it waits to try again", func() bool { alive, _ := b.state(); return alive > before+3 })

	c.err.Store(nil)
	r.Eventually("ready again after a poll that works", func() bool { _, ready := b.state(); return ready })
}

func TestTheHubIsNotAliveWhenItsTelegramLoopIsStuck(t *testing.T) {
	r, b, c := healthRig(t)
	r.StartAll()
	r.Eventually("alive at first", func() bool { a, _ := b.state(); return a > 0 })
	c.stuck.Store(true) // the next poll never returns
	r.Eventually("a stuck loop to stop the beats", func() bool {
		a1, _ := b.state()
		time.Sleep(3 * (3*100*time.Millisecond + 3*150*time.Millisecond)) // longer than the liveness window
		a2, _ := b.state()
		return a1 == a2
	})
	if _, ready := b.state(); ready {
		t.Fatal("a stuck loop is ready")
	}
}

func TestStoppingTheHubSaysNotReadyAtOnceAndEndsInTime(t *testing.T) {
	r, b, _ := healthRig(t)
	r.StartAll()
	r.Eventually("ready", func() bool { _, ready := b.state(); return ready })
	start := time.Now()
	r.StopHub()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the hub took %v to stop", took)
	}
	h := b.history()
	if len(h) == 0 || h[len(h)-1] {
		t.Fatalf("the last thing the hub said was %v: it must say not ready when it stops", h)
	}
}

func TestAWorkThatIgnoresTheStopCannotHoldTheHubLongerThanTheShutdownTimeout(t *testing.T) {
	r, _, c := healthRig(t)
	r.StartAll()
	r.Eventually("the hub to be up", func() bool { return r.Hub != nil && r.Hub.Online("laptop") })
	c.stuck.Store(true)
	time.Sleep(200 * time.Millisecond) // the Telegram loop is now inside a poll that ignores ctx
	start := time.Now()
	r.StopHub()
	took := time.Since(start)
	if took < 300*time.Millisecond {
		t.Fatalf("the hub did not wait for the work in flight: %v", took)
	}
	if took > 2*time.Second {
		t.Fatalf("the hub waited %v for work that ignores the stop (limit 400ms)", took)
	}
}
