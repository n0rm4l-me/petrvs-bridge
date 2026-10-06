// Package testrig builds a hub, agents, a bus, a fake chat and a fake tmux for
// end-to-end tests. It is used by tests only.
package testrig

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claudebridge/internal/agentsvc"
	"claudebridge/internal/alarm"
	"claudebridge/internal/approval"
	"claudebridge/internal/hubsvc"
	"claudebridge/internal/identity"
	"claudebridge/internal/spool"
	"claudebridge/internal/telegram"
	"claudebridge/internal/transport"
	"claudebridge/internal/wire"
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
	// Prefix names the topics; required when Transport is shared, defaults to "cb".
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

	hubCfg hubsvc.Config
	cancel context.CancelFunc
	done   chan struct{}
	mult   time.Duration
}

func quietLog() *slog.Logger {
	if os.Getenv("CB_TEST_LOG") != "" {
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
		r.Names.Prefix = "cb"
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
		if r.Clock != nil {
			ar.cfg.Now = r.Clock.Now
		}
		r.Agents[name] = ar
	}
	r.hubCfg = hubsvc.Config{
		Identity: r.HubID, Agents: pubs, Transport: r.Bus, Names: r.Names, Telegram: r.TG, ChatID: ChatID, UserID: UserID, Log: quietLog(),
		OfflineAfter: 500 * time.Millisecond * r.mult, WatchEvery: 40 * time.Millisecond * r.mult, HelloMinInterval: 20 * time.Millisecond,
		PollTimeout: 150 * time.Millisecond,
	}
	if o.OfflineAfter > 0 {
		r.hubCfg.OfflineAfter = o.OfflineAfter
	}
	if o.HubLockFile {
		r.hubCfg.LockFile = filepath.Join(t.TempDir(), "hub-lock")
	}
	if r.Clock != nil {
		r.hubCfg.Now = r.Clock.Now
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

// Say types a message as the authorised user.
func (r *Rig) Say(text string) {
	r.TG.Push(telegram.Update{ID: nextID(), ChatID: ChatID, UserID: UserID, Text: text})
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
	p, err := transport.NewPubSub(ctx, "cb-test")
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
