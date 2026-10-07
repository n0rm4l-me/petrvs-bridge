// Package testrig builds a hub, agents, a bus, a fake chat and a fake tmux for
// end-to-end tests. It is used by tests only.
package testrig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/alarm"
	"github.com/n0rm4l-me/petrvs-bridge/internal/approval"
	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

const (
	ChatID = 1001
	UserID = 2002
)

// Clock is a clock tests can move.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at the current time.
func NewClock() *Clock { return &Clock{t: time.Now()} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// Exec is a fake tmux that records what was typed.
type Exec struct {
	mu    sync.Mutex
	texts []string
	keys  []string
	Err   error
}

func (e *Exec) SendText(_ context.Context, text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Err != nil {
		return e.Err
	}
	e.texts = append(e.texts, text)
	return nil
}

func (e *Exec) SendKey(_ context.Context, key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Err != nil {
		return e.Err
	}
	e.keys = append(e.keys, key)
	return nil
}

// Texts returns the texts typed so far.
func (e *Exec) Texts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.texts...)
}

// Keys returns the keys pressed so far.
func (e *Exec) Keys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.keys...)
}

// Options tune a rig.
type Options struct {
	Agents    []string // default: one agent named "laptop"
	Transport transport.Transport
	// Prefix names the topics; required when Transport is shared, defaults to "pb".
	Prefix string
	// Clock, if set, is used by the hub and every agent.
	Clock *Clock
	// SilenceLimit overrides the agents' silence limit (useful when the clock is moved).
	SilenceLimit time.Duration
	// OfflineAfter overrides the hub's offline threshold.
	OfflineAfter time.Duration
	// Slow uses relaxed timings for a bus with real latency.
	Slow bool
	// Alarm gives every agent an alarm with a sensor the test controls.
	Alarm bool
	// AlarmSecret, if set, makes disarming require a one-time code for this secret.
	AlarmSecret []byte
	// Heartbeat overrides the agents' heartbeat interval. A long one keeps the bus quiet, so a test can tell which
	// message is which by counting.
	Heartbeat time.Duration
	// ReorderWindow overrides how long an agent waits for a missing earlier command.
	ReorderWindow time.Duration
	// HubLockFile gives the hub a lock file, so a lockdown survives StartHub.
	HubLockFile bool
	// ReceivedReaction and DeliveredReaction are the hub's marks on the user's messages; empty means none.
	ReceivedReaction, DeliveredReaction string
	// FaultyBus puts a FaultyBus between the hub, the agents and the bus (Rig.Faulty), to fail publishes, delay them, or
	// deliver each message in a goroutine of its own.
	FaultyBus bool
	// WrapTelegram gives the hub a client built around the rig's fake (to gate or fail calls); Rig.TG stays the fake.
	WrapTelegram func(*telegram.Fake) telegram.Client
	// Hub changes the hub's configuration before it is built.
	Hub func(*hubsvc.Config)
	// Screen gives every agent a FakeScreen (AgentRig.Screen) showing an idle Claude Code.
	Screen bool
	// Agent changes an agent's configuration before it is built; it runs after the rig's own settings.
	Agent func(name string, c *agentsvc.Config)
	// MinTurn is the least time between two batches of a file's parts, for the hub and every agent (zero: the default of
	// the components, 250ms, which keeps a transfer within the budget of the real ratchet). A test that moves few parts
	// and does not care about the ratchet's budget may use a few milliseconds to go faster.
	MinTurn time.Duration
}

// FaultyBus wraps a bus to inject what a real one does. The in-memory bus calls its handlers one at a time, which hides
// every race between two messages; Concurrent gives each delivery its own goroutine, as Pub/Sub does.
type FaultyBus struct {
	transport.Transport
	// Concurrent gives each delivery its own goroutine. Receive reads it for every message, so it may be set before or
	// after the rig starts; a test that sets it while the rig runs must use SetConcurrent, which takes the lock.
	Concurrent bool

	mu         sync.Mutex
	fail       func(topic string) error
	after      func(topic string)
	latency    time.Duration
	lanes      map[string]chan delayed
	closed     chan struct{}
	closedOnce sync.Once
	active     int // handlers running now, and the most there were at the same time
	overlap    int
}

// delayed is a message on its way through a lane of the bus with a latency.
type delayed struct {
	due   time.Time
	data  []byte
	attrs map[string]string
}

// SetConcurrent is the setting of Concurrent for a rig that is already running.
func (b *FaultyBus) SetConcurrent(on bool) {
	b.mu.Lock()
	b.Concurrent = on
	b.mu.Unlock()
}

func (b *FaultyBus) concurrent() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Concurrent
}

// Overlap is the most handlers that ran at the same time so far: a test that says it runs on a concurrent bus can check
// that it really did.
func (b *FaultyBus) Overlap() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overlap
}

// run calls the handler and counts it among those running.
func (b *FaultyBus) run(handler func(context.Context, *transport.Message), c context.Context, m *transport.Message) {
	b.mu.Lock()
	b.active++
	b.overlap = max(b.overlap, b.active)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.active--
		b.mu.Unlock()
	}()
	handler(c, m)
}

// SetLatency makes every message published from now on reach the bus d later, in the order it was published (a long,
// steady network: the sender does not wait, the receiver hears of it after d). Zero turns it off.
func (b *FaultyBus) SetLatency(d time.Duration) {
	b.mu.Lock()
	b.latency = d
	b.mu.Unlock()
}

// Stop ends what the latency lanes still hold; the rig does it when the test ends.
func (b *FaultyBus) Stop() { b.closedOnce.Do(func() { close(b.closed) }) }

// lane returns the queue of one topic, with the goroutine that passes its messages on when they are due.
func (b *FaultyBus) lane(topic string) chan delayed {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l := b.lanes[topic]; l != nil {
		return l
	}
	if b.lanes == nil {
		b.lanes = map[string]chan delayed{}
	}
	l := make(chan delayed, 8192)
	b.lanes[topic] = l
	go func() {
		for {
			select {
			case <-b.closed:
				return
			case m := <-l:
				if wait := time.Until(m.due); wait > 0 {
					select {
					case <-time.After(wait):
					case <-b.closed:
						return
					}
				}
				b.Transport.Publish(context.Background(), topic, m.data, m.attrs)
			}
		}
	}()
	return l
}

// FailPublish makes Publish return the error f gives for a topic, without sending anything; nil from f lets it through.
// A nil f clears it.
func (b *FaultyBus) FailPublish(f func(topic string) error) {
	b.mu.Lock()
	b.fail = f
	b.mu.Unlock()
}

// AfterPublish calls f once the bus has taken a message and before Publish returns (a slow acknowledgement). A nil f clears it.
func (b *FaultyBus) AfterPublish(f func(topic string)) {
	b.mu.Lock()
	b.after = f
	b.mu.Unlock()
}

func (b *FaultyBus) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	b.mu.Lock()
	fail, after := b.fail, b.after
	b.mu.Unlock()
	if fail != nil {
		if err := fail(topic); err != nil {
			return err
		}
	}
	b.mu.Lock()
	latency := b.latency
	b.mu.Unlock()
	if latency > 0 {
		select {
		case b.lane(topic) <- delayed{due: time.Now().Add(latency), data: append([]byte(nil), data...), attrs: attrs}:
		default:
			return errors.New("testrig: the latency lane is full")
		}
		if after != nil {
			after(topic)
		}
		return nil
	}
	err := b.Transport.Publish(ctx, topic, data, attrs)
	if err == nil && after != nil {
		after(topic)
	}
	return err
}

func (b *FaultyBus) Receive(ctx context.Context, sub string, handler func(context.Context, *transport.Message)) error {
	// Decided for each message, so that the setting works whenever it is made (the rig's Start... calls begin Receive
	// before a test can touch the bus).
	var wg sync.WaitGroup
	err := b.Transport.Receive(ctx, sub, func(c context.Context, m *transport.Message) {
		if !b.concurrent() {
			b.run(handler, c, m)
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); b.run(handler, c, m) }()
	})
	wg.Wait()
	return err
}

// Sensor is an alarm sensor the test triggers by hand.
type Sensor struct {
	mu   sync.Mutex
	hits int
}

func (s *Sensor) Name() string { return "input" }

// Touch makes the next check report a touch.
func (s *Sensor) Touch() { s.mu.Lock(); s.hits++; s.mu.Unlock() }

func (s *Sensor) Check(context.Context) (alarm.Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hits > 0 {
		s.hits = 0
		return alarm.Event{Sensor: "input", Detail: "keyboard, mouse or trackpad was used"}, true, nil
	}
	return alarm.Event{}, false, nil
}

// AgentRig is one agent and its surroundings.
type AgentRig struct {
	ID       *identity.Identity
	Agent    *agentsvc.Agent
	Exec     *Exec
	Spool    *spool.Spool
	LockFile string
	// Screen is the fake screen of the agent's session (nil unless Options.Screen is set).
	Screen *FakeScreen
	// Sensor is the alarm's test sensor (nil when the rig has no alarm).
	Sensor     *Sensor
	AlarmState string
	Alarm      *alarm.Alarm
	cfg        agentsvc.Config
	mult       time.Duration
	cancel     context.CancelFunc
	done       chan struct{}
}

// Rig is a complete system under test.
type Rig struct {
	T      *testing.T
	Bus    transport.Transport
	Mem    *transport.Mem
	TG     *telegram.Fake
	Names  wire.Names
	HubID  *identity.Identity
	Hub    *hubsvc.Hub
	Agents map[string]*AgentRig
	Clock  *Clock
	// Faulty is the bus the hub and the agents use when Options.FaultyBus is set, nil otherwise.
	Faulty *FaultyBus

	hubCfg hubsvc.Config
	cancel context.CancelFunc
	done   chan struct{}
	mult   time.Duration
}

func quietLog() *slog.Logger {
	if os.Getenv("PB_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// New builds a rig without starting anything.
func New(t *testing.T, o Options) *Rig {
	t.Helper()
	if len(o.Agents) == 0 {
		o.Agents = []string{"laptop"}
	}
	r := &Rig{T: t, TG: telegram.NewFake(), Agents: map[string]*AgentRig{}, Clock: o.Clock, mult: 1}
	if o.Slow {
		r.mult = 4
	}
	r.Names = wire.Names{Prefix: o.Prefix}
	if r.Names.Prefix == "" {
		r.Names.Prefix = "pb"
	}
	var err error
	if r.HubID, err = identity.Generate("hub"); err != nil {
		t.Fatal(err)
	}
	if o.Transport != nil {
		r.Bus = o.Transport
	} else {
		r.Mem = transport.NewMem()
		r.Bus = r.Mem
		r.Mem.Bind(r.Names.ToHub(), r.Names.HubSub())
	}
	if o.FaultyBus {
		r.Faulty = &FaultyBus{Transport: r.Bus, closed: make(chan struct{})}
		t.Cleanup(r.Faulty.Stop)
		r.Bus = r.Faulty
	}
	var pubs []identity.Public
	for _, name := range o.Agents {
		id, err := identity.Generate(name)
		if err != nil {
			t.Fatal(err)
		}
		pubs = append(pubs, id.Public())
		if r.Mem != nil {
			r.Mem.Bind(r.Names.ToAgent(name), r.Names.AgentSub(name))
		}
		sp, err := spool.Open(filepath.Join(t.TempDir(), "spool-"+name))
		if err != nil {
			t.Fatal(err)
		}
		ar := &AgentRig{ID: id, Exec: &Exec{}, Spool: sp, LockFile: filepath.Join(t.TempDir(), "lock-"+name), mult: r.mult}
		if o.Alarm {
			ar.Sensor = &Sensor{}
			ar.AlarmState = filepath.Join(t.TempDir(), "alarm-"+name)
			if o.AlarmSecret != nil {
				ar.cfg.AlarmCode = approval.NewVerifier(o.AlarmSecret)
			}
		}
		m := r.mult
		code := ar.cfg.AlarmCode
		ar.cfg = agentsvc.Config{AlarmCode: code,
			Identity: id, Hub: r.HubID.Public(), Transport: r.Bus, Names: r.Names, Exec: ar.Exec, Spool: sp, LockFile: ar.LockFile,
			Log: quietLog(), HeartbeatEvery: 100 * time.Millisecond * m, HandshakeEvery: 150 * time.Millisecond * m,
			SilenceLimit: 600 * time.Millisecond * m, SpoolEvery: 40 * time.Millisecond * m, ReceiptTimeout: 300 * time.Millisecond * m,
		}
		if o.SilenceLimit > 0 {
			ar.cfg.SilenceLimit = o.SilenceLimit
		}
		if o.Heartbeat > 0 {
			ar.cfg.HeartbeatEvery = o.Heartbeat
		}
		if o.ReorderWindow > 0 {
			ar.cfg.ReorderWindow = o.ReorderWindow
		}
		ar.cfg.MinTurn = o.MinTurn
		if r.Clock != nil {
			ar.cfg.Now = r.Clock.Now
		}
		if o.Screen {
			ar.Screen = NewFakeScreen()
			ar.cfg.Screen = ar.Screen
		}
		if o.Agent != nil {
			o.Agent(name, &ar.cfg)
		}
		r.Agents[name] = ar
	}
	var chat telegram.Client = r.TG
	if o.WrapTelegram != nil {
		chat = o.WrapTelegram(r.TG)
	}
	r.hubCfg = hubsvc.Config{
		Identity: r.HubID, Agents: pubs, Transport: r.Bus, Names: r.Names, Telegram: chat, ChatID: ChatID, UserID: UserID, Log: quietLog(),
		OfflineAfter: 500 * time.Millisecond * r.mult, WatchEvery: 40 * time.Millisecond * r.mult, HelloMinInterval: 20 * time.Millisecond,
		PollTimeout: 150 * time.Millisecond, ReceivedReaction: o.ReceivedReaction, DeliveredReaction: o.DeliveredReaction,
	}
	if o.OfflineAfter > 0 {
		r.hubCfg.OfflineAfter = o.OfflineAfter
	}
	r.hubCfg.MinTurn = o.MinTurn
	if o.HubLockFile {
		r.hubCfg.LockFile = filepath.Join(t.TempDir(), "hub-lock")
	}
	if r.Clock != nil {
		r.hubCfg.Now = r.Clock.Now
	}
	if o.Hub != nil {
		o.Hub(&r.hubCfg)
	}
	t.Cleanup(r.StopAll)
	return r
}

// StartHub starts (or restarts, with fresh in-memory state) the hub.
func (r *Rig) StartHub() {
	r.T.Helper()
	r.StopHub()
	h, err := hubsvc.New(r.hubCfg)
	if err != nil {
		r.T.Fatal(err)
	}
	r.Hub = h
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() { h.Run(ctx); close(r.done) }()
}

// StopHub stops the hub and waits for it.
func (r *Rig) StopHub() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
		r.cancel = nil
	}
}

// StartAgent starts (or restarts, with fresh in-memory state) an agent.
func (r *Rig) StartAgent(name string) {
	r.T.Helper()
	r.StopAgent(name)
	ar := r.Agents[name]
	cfg := ar.cfg
	if ar.Sensor != nil { // a restarted agent reloads its alarm from the state file
		al, err := alarm.New(alarm.Config{Sensors: []alarm.Sensor{ar.Sensor}, Poll: 20 * time.Millisecond * ar.mult, Cooldown: 400 * time.Millisecond * ar.mult, StateFile: ar.AlarmState, Now: cfg.Now})
		if err != nil {
			r.T.Fatal(err)
		}
		ar.Alarm, cfg.Alarm = al, al
	}
	a, err := agentsvc.New(cfg)
	if err != nil {
		r.T.Fatal(err)
	}
	ar.Agent = a
	ctx, cancel := context.WithCancel(context.Background())
	ar.cancel, ar.done = cancel, make(chan struct{})
	go func() { a.Run(ctx); close(ar.done) }()
}

// StopAgent stops an agent and waits for it.
func (r *Rig) StopAgent(name string) {
	if ar := r.Agents[name]; ar != nil && ar.cancel != nil {
		ar.cancel()
		<-ar.done
		ar.cancel = nil
	}
}

// StopAll stops everything.
func (r *Rig) StopAll() {
	for n := range r.Agents {
		r.StopAgent(n)
	}
	r.StopHub()
}

// StartAll starts the hub and every agent and waits until all are connected.
func (r *Rig) StartAll() {
	r.T.Helper()
	r.StartHub()
	for n := range r.Agents {
		r.StartAgent(n)
	}
	for n := range r.Agents {
		r.WaitConnected(n)
	}
}

// Timeout is how long Eventually waits.
func (r *Rig) Timeout() time.Duration { return 6 * time.Second * r.mult }

// Eventually polls cond until it is true or the timeout passes.
func (r *Rig) Eventually(what string, cond func() bool) {
	r.T.Helper()
	deadline := time.Now().Add(r.Timeout())
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.T.Fatalf("timed out waiting for: %s\nchat so far: %q", what, r.SentTexts())
}

// Never asserts that cond stays false for the given time.
func (r *Rig) Never(what string, d time.Duration, cond func() bool) {
	r.T.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			r.T.Fatalf("this must not happen: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WaitConnected waits until the hub and the agent both consider the session up.
func (r *Rig) WaitConnected(name string) {
	r.T.Helper()
	r.Eventually(name+" to connect", func() bool { return r.Agents[name].Agent.Connected() && r.Hub.Online(name) })
}

// Say types a message as the authorised user and returns the id the chat gave it.
func (r *Rig) Say(text string) int64 {
	id := nextID()
	r.TG.Push(telegram.Update{ID: id, ChatID: ChatID, UserID: UserID, MessageID: 100000 + id, Text: text})
	return 100000 + id
}

// SayReplyTo types a message as a reply to an earlier message of the chat (a swipe-reply).
func (r *Rig) SayReplyTo(text string, replyTo int64) {
	r.TG.Push(telegram.Update{ID: nextID(), ChatID: ChatID, UserID: UserID, Text: text, ReplyToID: replyTo})
}

// Press taps an inline button that carries the given data, as the authorised user.
func (r *Rig) Press(data string) {
	r.TG.Push(telegram.Update{ID: nextID(), ChatID: ChatID, UserID: UserID, CallbackID: fmt.Sprintf("cb%d", nextID()), CallbackData: data})
}

// MessageID returns the id the chat gave to the first message the hub sent that contains the substring, 0 if none.
func (r *Rig) MessageID(sub string) int64 {
	for _, s := range r.TG.Sent() {
		if strings.Contains(s.Text, sub) {
			return s.ID
		}
	}
	return 0
}

// SayAt types a message that Telegram received at the given time, which may be long ago (a backlog after an outage).
func (r *Rig) SayAt(text string, at time.Time) {
	r.TG.Push(telegram.Update{ID: nextID(), ChatID: ChatID, UserID: UserID, Text: text, Date: at})
}

// SetLockFile changes where an agent keeps its lock; call it before StartAgent.
func (a *AgentRig) SetLockFile(path string) {
	a.LockFile = path
	a.cfg.LockFile = path
}

// SayAs types a message as someone else.
func (r *Rig) SayAs(chat, user int64, text string) {
	r.TG.Push(telegram.Update{ID: nextID(), ChatID: chat, UserID: user, Text: text})
}

var (
	idMu sync.Mutex
	idN  int64
)

func nextID() int64 { idMu.Lock(); defer idMu.Unlock(); idN++; return idN }

// SentTexts returns what the hub has written to the chat.
func (r *Rig) SentTexts() []string {
	var out []string
	for _, s := range r.TG.Sent() {
		out = append(out, s.Text)
	}
	return out
}

// Sent reports whether some message to the chat contains the substring.
func (r *Rig) Sent(sub string) bool {
	for _, s := range r.SentTexts() {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// WaitSent waits for a chat message containing the substring.
func (r *Rig) WaitSent(sub string) {
	r.T.Helper()
	r.Eventually(fmt.Sprintf("a chat message containing %q", sub), func() bool { return r.Sent(sub) })
}

// Emulator connects to the Pub/Sub emulator and creates the topics and subscriptions a rig needs. It skips the
// test when PUBSUB_EMULATOR_HOST is not set. The returned prefix is unique to the test.
func Emulator(t *testing.T, agents ...string) (*transport.PubSub, string) {
	t.Helper()
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("PUBSUB_EMULATOR_HOST not set; run scripts/with-emulator.sh go test ./...")
	}
	if len(agents) == 0 {
		agents = []string{"laptop"}
	}
	ctx := context.Background()
	p, err := transport.NewPubSub(ctx, "pb-test")
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("e%d", time.Now().UnixNano()%1_000_000_000)
	names := wire.Names{Prefix: prefix}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.EnsureTopic(ctx, names.ToHub()))
	must(p.EnsureSubscription(ctx, names.ToHub(), names.HubSub(), 10*time.Second))
	t.Cleanup(func() { p.Delete(context.Background(), names.ToHub(), names.HubSub()) })
	for _, a := range agents {
		must(p.EnsureTopic(ctx, names.ToAgent(a)))
		must(p.EnsureSubscription(ctx, names.ToAgent(a), names.AgentSub(a), 10*time.Second))
		a := a
		t.Cleanup(func() { p.Delete(context.Background(), names.ToAgent(a), names.AgentSub(a)) })
	}
	t.Cleanup(func() { p.Close() })
	return p, prefix
}

// Tap records everything published to a topic through a subscription of its own, for replay tests.
type Tap struct {
	mu   sync.Mutex
	msgs []*transport.Message
}

// NewTap starts recording a topic.
func NewTap(t *testing.T, p *transport.PubSub, topic string) *Tap {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub := topic + "-tap"
	if err := p.EnsureSubscription(ctx, topic, sub, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	tap := &Tap{}
	done := make(chan struct{})
	go func() {
		p.Receive(ctx, sub, func(_ context.Context, m *transport.Message) {
			tap.mu.Lock()
			tap.msgs = append(tap.msgs, m)
			tap.mu.Unlock()
			m.Ack()
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done; p.Delete(context.Background(), "", sub) })
	return tap
}

// Messages returns what was recorded so far.
func (tp *Tap) Messages() []*transport.Message {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]*transport.Message(nil), tp.msgs...)
}

// FakeScreen stands in for the tmux session an agent types into: a screen that the test sets, the program in the
// foreground, and a record of the sessions the agent started.
type FakeScreen struct {
	mu       sync.Mutex
	screen   string
	command  string
	missing  bool
	started  []string
	startErr error
	captures int
}

// IdleScreen is what Claude Code shows when it waits for the next request.
const IdleScreen = "⏺ Done.\n\n────────────────────────────────────────────────────────────────────────────────\n ❯ \n────────────────────────────────────────────────────────────────────────────────\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"

// NewFakeScreen shows an idle Claude Code in auto mode.
func NewFakeScreen() *FakeScreen { return &FakeScreen{screen: IdleScreen, command: "claude"} }

// Set changes what the screen shows.
func (f *FakeScreen) Set(screen string) { f.mu.Lock(); f.screen = screen; f.mu.Unlock() }

// SetCommand changes the program in the foreground ("claude", "zsh", ...).
func (f *FakeScreen) SetCommand(c string) { f.mu.Lock(); f.command = c; f.mu.Unlock() }

// SetMissing makes the session disappear (true) or come back (false).
func (f *FakeScreen) SetMissing(m bool) { f.mu.Lock(); f.missing = m; f.mu.Unlock() }

// SetStartErr makes Start fail with err until it is called again with nil.
func (f *FakeScreen) SetStartErr(err error) { f.mu.Lock(); f.startErr = err; f.mu.Unlock() }

// Started lists the sessions that were started, as "workdir|command".
func (f *FakeScreen) Started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// Captures counts how often the screen was read.
func (f *FakeScreen) Captures() int { f.mu.Lock(); defer f.mu.Unlock(); return f.captures }

// Capture returns the screen, or screen.ErrNoSession when the session is missing.
func (f *FakeScreen) Capture(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures++
	if f.missing {
		return "", screen.ErrNoSession
	}
	return f.screen, nil
}

// PaneCommand returns the program in the foreground, or screen.ErrNoSession when the session is missing.
func (f *FakeScreen) PaneCommand(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing {
		return "", screen.ErrNoSession
	}
	return f.command, nil
}

// Start records the start and brings the session back, running the first word of the command.
func (f *FakeScreen) Start(_ context.Context, workdir, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, workdir+"|"+command)
	f.missing = false
	f.command, _, _ = strings.Cut(command, " ")
	f.screen = IdleScreen
	return nil
}

var _ agentsvc.Screen = (*FakeScreen)(nil)
