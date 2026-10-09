package transport

import (
	"context"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tcpProxy stands between the client and the emulator and does to the connection what a flaky tunnel does: it can cut it, refuse a
// new one for a while, or hold the bytes back without closing anything (the connection looks alive and nothing arrives).
type tcpProxy struct {
	ln     net.Listener
	target string

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	refusal time.Time     // new connections are closed at once until then
	flow    chan struct{} // closed while bytes pass, open while they are held
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}, flow: make(chan struct{})}
	close(p.flow)
	t.Cleanup(func() { ln.Close(); p.closeAll() })
	go p.accept()
	return p
}

func (p *tcpProxy) Addr() string { return p.ln.Addr().String() }

func (p *tcpProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := time.Now().Before(p.refusal)
		p.mu.Unlock()
		if refuse {
			c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go p.pipe(up, c)
		go p.pipe(c, up)
	}
}

func (p *tcpProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			p.mu.Lock()
			flow := p.flow
			p.mu.Unlock()
			<-flow // held here while the tunnel is stalled
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	dst.Close()
	src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

func (p *tcpProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
}

// Cut closes every connection and refuses new ones for d.
func (p *tcpProxy) Cut(d time.Duration) {
	p.mu.Lock()
	p.refusal = time.Now().Add(d)
	p.mu.Unlock()
	p.closeAll()
}

// Stall holds the bytes back; Release lets them through.
func (p *tcpProxy) Stall() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.flow:
		p.flow = make(chan struct{})
	default:
	}
}

func (p *tcpProxy) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.flow:
	default:
		close(p.flow)
	}
}

// throughProxy returns a unary reader whose connection runs through a proxy in front of the emulator, and a direct client
// for the test to set up and publish with.
func throughProxy(t *testing.T) (direct *PubSub, reader *PubSub, proxy *tcpProxy) {
	t.Helper()
	direct = emu(t)
	proxy = newTCPProxy(t, os.Getenv("PUBSUB_EMULATOR_HOST"))
	t.Setenv("PUBSUB_EMULATOR_HOST", proxy.Addr())
	r, err := NewPubSub(context.Background(), "pb-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	r.SetPullMode(PullUnary)
	return direct, r, proxy
}

// readAsTheAgentDoes keeps reading the way the agent's receive does: when Receive returns an error, wait a moment and call it again.
func readAsTheAgentDoes(ctx context.Context, p *PubSub, sub string, handler func(context.Context, *Message)) {
	go func() {
		for ctx.Err() == nil {
			p.Receive(ctx, sub, handler)
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}()
}

func waitFor(t *testing.T, ch <-chan string, want string, within time.Duration, what string) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("%s: %q did not arrive within %v", what, want, within)
		}
	}
}

// A handler that takes longer than the ack deadline of the subscription (10 s here) keeps its message: the lease is extended,
// so the service does not hand it out again, and the answer still counts.
func TestUnaryPullKeepsTheLeaseOfASlowHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 14 s")
	}
	p := unaryEmu(t)
	topic, subs := setup(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	p.Publish(ctx, topic, []byte("slow"), nil)
	var deliveries atomic.Int32
	finished := make(chan struct{})
	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Receive(rctx, subs[0], func(_ context.Context, m *Message) {
			deliveries.Add(1)
			if deliveries.Load() > 1 {
				m.Ack()
				return
			}
			time.Sleep(14 * time.Second)
			m.Ack()
			close(finished)
		})
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("the handler never finished")
	}
	time.Sleep(2 * time.Second) // a late redelivery would show here
	stop()
	<-done
	if n := deliveries.Load(); n != 1 {
		t.Fatalf("the message was delivered %d times while its handler held it", n)
	}
}

// The network is cut and comes back: reading resumes by itself and the message published during the cut arrives.
func TestUnaryPullResumesAfterTheConnectionIsCut(t *testing.T) {
	direct, reader, proxy := throughProxy(t)
	topic, subs := setup(t, direct)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got := make(chan string, 16)
	readAsTheAgentDoes(ctx, reader, subs[0], func(_ context.Context, m *Message) { m.Ack(); got <- string(m.Data) })

	direct.Publish(ctx, topic, []byte("before"), nil)
	waitFor(t, got, "before", 10*time.Second, "before the cut")

	proxy.Cut(4 * time.Second)
	time.Sleep(500 * time.Millisecond)
	direct.Publish(ctx, topic, []byte("during"), nil)
	waitFor(t, got, "during", 30*time.Second, "after the cut")

	direct.Publish(ctx, topic, []byte("after"), nil)
	waitFor(t, got, "after", 10*time.Second, "once the connection is back")
}

// The tunnel holds the bytes back without closing anything, as a stalled one does with a stream: nothing arrives while it holds,
// and when it lets go the message arrives.
func TestUnaryPullDeliversWhenAStalledConnectionIsReleased(t *testing.T) {
	direct, reader, proxy := throughProxy(t)
	topic, subs := setup(t, direct)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got := make(chan string, 16)
	readAsTheAgentDoes(ctx, reader, subs[0], func(_ context.Context, m *Message) { m.Ack(); got <- string(m.Data) })

	direct.Publish(ctx, topic, []byte("before"), nil)
	waitFor(t, got, "before", 10*time.Second, "before the stall")

	proxy.Stall()
	time.Sleep(300 * time.Millisecond)
	direct.Publish(ctx, topic, []byte("held"), nil)
	select {
	case m := <-got:
		t.Fatalf("%q came through a stalled connection", m)
	case <-time.After(2 * time.Second):
	}
	proxy.Release()
	waitFor(t, got, "held", 20*time.Second, "after the release")
}

// A stall longer than one call (25 s): the call is given up and made again, and what was published meanwhile is read once the
// bytes flow.
func TestUnaryPullSurvivesAStallLongerThanACall(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 45 s")
	}
	direct, reader, proxy := throughProxy(t)
	topic, subs := setup(t, direct)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	got := make(chan string, 16)
	readAsTheAgentDoes(ctx, reader, subs[0], func(_ context.Context, m *Message) { m.Ack(); got <- string(m.Data) })

	direct.Publish(ctx, topic, []byte("before"), nil)
	waitFor(t, got, "before", 10*time.Second, "before the stall")

	proxy.Stall()
	time.Sleep(300 * time.Millisecond)
	direct.Publish(ctx, topic, []byte("held"), nil)
	time.Sleep(unaryPullDeadline + 3*time.Second)
	proxy.Release()
	waitFor(t, got, "held", 40*time.Second, "after the long stall")
	direct.Publish(ctx, topic, []byte("after"), nil)
	waitFor(t, got, "after", 20*time.Second, "once the bytes flow again")
}
