// Package agentsvc is the client: it runs next to the tmux session, talks only to
// the message bus, and types what the hub sends into the session.
//
// Nothing is trusted because of where it came from or what its attributes say.
// A message is acted on only if it authenticates under the ratchet session that
// this agent opened with the pinned hub, has not expired and has not been seen.
package agentsvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claudebridge/internal/alarm"
	"claudebridge/internal/approval"
	"claudebridge/internal/handshake"
	"claudebridge/internal/identity"
	"claudebridge/internal/ratchet"
	"claudebridge/internal/spool"
	"claudebridge/internal/transport"
	"claudebridge/internal/wire"
)

// Executor types into the session.
type Executor interface {
	SendText(ctx context.Context, text string) error
	SendKey(ctx context.Context, key string) error
}

// Config holds everything an agent needs.
type Config struct {
	Identity  *identity.Identity
	Hub       identity.Public
	Transport transport.Transport
	Names     wire.Names
	Exec      Executor
	// Spool, if set, is drained and its messages are sent to the hub.
	Spool *spool.Spool
	// LockFile, if set, keeps the lock across restarts; only deleting it on this machine unlocks.
	LockFile string
	Now      func() time.Time
	Log      *slog.Logger
	// Info adds facts (battery, load, ...) to each heartbeat.
	Info func() map[string]string

	HeartbeatEvery time.Duration // default 30s
	HandshakeEvery time.Duration // default 10s
	SilenceLimit   time.Duration // default 3 x HeartbeatEvery: no hub answer for this long means re-handshake
	SpoolEvery     time.Duration // default 2s
	SkewAllowance  time.Duration // default 30s
	// Alarm, if set, is armed and disarmed from the chat and reports touches of this machine.
	Alarm *alarm.Alarm
	// AlarmCode, if set, is required to disarm the alarm: "/alarm off 123456" with a current one-time code.
	AlarmCode *approval.Verifier
	// ReceiptTimeout is how long a spooled message waits for the hub's receipt before it is sent again.
	ReceiptTimeout time.Duration // default 30s
}

// Stats counts what the agent has seen; the counters never include message content.
type Stats struct {
	Handshakes, Executed, Rejected, Duplicates, Expired, Refused uint64
}

type session struct {
	sid [8]byte
	r   *ratchet.Ratchet
}

type awaitingReceipt struct {
	file   string
	sentAt time.Time
}

type pendingHello struct {
	ini *handshake.Initiator
	at  time.Time
}

// Agent is the client service.
type Agent struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	sess     *session
	pending  map[string]pendingHello
	lastHub  time.Time // last authenticated message from the hub
	locked   bool
	seenIDs  map[string]struct{}
	seenRing []string
	awaiting map[string]awaitingReceipt // payload id -> spool file

	handshakes, executed, rejected, duplicates, expired, refused atomic.Uint64
}

// New validates the configuration and returns an agent.
func New(cfg Config) (*Agent, error) {
	if cfg.Identity == nil || cfg.Transport == nil || cfg.Exec == nil {
		return nil, errors.New("agentsvc: identity, transport and executor are required")
	}
	if err := cfg.Hub.Validate(); err != nil {
		return nil, err
	}
	if cfg.Names.Prefix == "" {
		cfg.Names = wire.DefaultNames()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.HeartbeatEvery, 30*time.Second)
	def(&cfg.HandshakeEvery, 10*time.Second)
	def(&cfg.SilenceLimit, 3*cfg.HeartbeatEvery)
	def(&cfg.SpoolEvery, 2*time.Second)
	def(&cfg.SkewAllowance, 30*time.Second)
	def(&cfg.ReceiptTimeout, 30*time.Second)
	return &Agent{cfg: cfg, log: cfg.Log.With("agent", cfg.Identity.Name), pending: map[string]pendingHello{}, seenIDs: map[string]struct{}{}, awaiting: map[string]awaitingReceipt{}}, nil
}

// Stats returns the counters.
func (a *Agent) Stats() Stats {
	return Stats{a.handshakes.Load(), a.executed.Load(), a.rejected.Load(), a.duplicates.Load(), a.expired.Load(), a.refused.Load()}
}

// Connected reports whether a session is established and the hub answered recently.
func (a *Agent) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess != nil && a.cfg.Now().Sub(a.lastHub) < a.cfg.SilenceLimit
}

// Locked reports whether the agent currently refuses text and keys.
func (a *Agent) Locked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lockedLocked()
}

func (a *Agent) lockedLocked() bool {
	if a.cfg.LockFile != "" {
		_, err := os.Stat(a.cfg.LockFile)
		a.locked = err == nil // deleting the file on this machine is the unlock
		return a.locked
	}
	return a.locked
}

// Run connects, serves and heartbeats until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.cfg.LockFile != "" {
		a.mu.Lock()
		a.lockedLocked()
		a.mu.Unlock()
	}
	errc := make(chan error, 1)
	go func() { errc <- a.cfg.Transport.Receive(ctx, a.cfg.Names.AgentSub(a.cfg.Identity.Name), a.handle) }()
	go a.loop(ctx, a.cfg.HandshakeEvery, true, a.handshakeTick)
	go a.loop(ctx, a.cfg.HeartbeatEvery, false, a.heartbeatTick)
	if a.cfg.Spool != nil {
		go a.loop(ctx, a.cfg.SpoolEvery, false, a.spoolTick)
	}
	if a.cfg.Alarm != nil {
		a.cfg.Alarm.SetNotify(a.alert)
		go a.cfg.Alarm.Run(ctx)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func (a *Agent) loop(ctx context.Context, every time.Duration, immediately bool, f func(context.Context)) {
	if immediately {
		f(ctx)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f(ctx)
		}
	}
}

func (a *Agent) handshakeTick(ctx context.Context) {
	a.mu.Lock()
	need := a.sess == nil || a.cfg.Now().Sub(a.lastHub) >= a.cfg.SilenceLimit
	a.mu.Unlock()
	if !need {
		return
	}
	now := a.cfg.Now()
	ini, hello, err := handshake.NewInitiator(a.cfg.Identity, a.cfg.Hub, now)
	if err != nil {
		a.log.Error("handshake start failed", "err", err)
		return
	}
	data, err := hello.Encode()
	if err != nil {
		return
	}
	a.mu.Lock()
	for k, p := range a.pending { // keep the pending set small and recent
		if now.Sub(p.at) > 2*handshake.MaxSkew {
			delete(a.pending, k)
		}
	}
	if len(a.pending) >= 4 {
		for k := range a.pending {
			delete(a.pending, k)
			break
		}
	}
	a.pending[ini.ID()] = pendingHello{ini: ini, at: now}
	a.mu.Unlock()
	if err := a.cfg.Transport.Publish(ctx, a.cfg.Names.ToHub(), data, wire.Attrs(wire.KindHello, a.cfg.Identity.Name)); err != nil {
		a.log.Warn("could not publish hello", "err", err)
	}
}

func (a *Agent) heartbeatTick(ctx context.Context) {
	if a.session() == nil {
		return
	}
	meta := map[string]string{"locked": boolStr(a.Locked())}
	if a.cfg.Alarm != nil {
		meta["alarm"] = map[bool]string{true: "on", false: "off"}[a.cfg.Alarm.Armed()]
		if blind := a.cfg.Alarm.Blind(); len(blind) > 0 {
			meta["alarm_blind"] = strings.Join(blind, ",")
		}
	}
	if a.cfg.Info != nil {
		for k, v := range a.cfg.Info() {
			if len(meta) < 12 {
				meta[k] = v
			}
		}
	}
	a.send(ctx, &wire.Payload{Type: wire.TypeHeartbeat, Meta: meta})
}

func (a *Agent) spoolTick(ctx context.Context) {
	if a.session() == nil {
		return
	}
	entries, err := a.cfg.Spool.List()
	if err != nil {
		a.log.Warn("could not read the spool", "err", err)
		return
	}
	for _, e := range entries {
		id := "sp-" + e.Name
		a.mu.Lock()
		w, waiting := a.awaiting[id]
		stillWaiting := waiting && a.cfg.Now().Sub(w.sentAt) < a.cfg.ReceiptTimeout
		a.mu.Unlock()
		if stillWaiting {
			continue
		}
		// The file stays until the hub confirms it: a bus that accepted the message may still lose it
		// (for example if the hub restarts and the session it was sealed for is gone).
		if err := a.send(ctx, &wire.Payload{ID: id, Type: e.Item.Type, Body: e.Item.Body}); err != nil {
			if ctx.Err() == nil {
				a.log.Warn("could not send a spooled message", "err", err)
			}
			return
		}
		a.mu.Lock()
		a.awaiting[id] = awaitingReceipt{file: e.Name, sentAt: a.cfg.Now()}
		a.mu.Unlock()
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (a *Agent) session() *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// send seals and publishes a payload to the hub.
func (a *Agent) send(ctx context.Context, p *wire.Payload) error {
	s := a.session()
	if s == nil {
		return errors.New("agentsvc: no session")
	}
	now := a.cfg.Now()
	if p.ID == "" {
		p.ID = newID()
	}
	p.TS, p.Exp = now.Unix(), now.Add(10*time.Minute).Unix()
	data, err := wire.Seal(s.r, a.cfg.Identity.Name, a.cfg.Hub.Name, s.sid, p)
	if err != nil {
		return err
	}
	return a.cfg.Transport.Publish(ctx, a.cfg.Names.ToHub(), data, wire.Attrs(wire.KindMsg, a.cfg.Identity.Name))
}

func (a *Agent) reject(m *transport.Message, why string) {
	a.rejected.Add(1)
	a.log.Debug("rejected message", "why", why, "bytes", len(m.Data))
	m.Ack() // never redeliver junk
}

func (a *Agent) handle(ctx context.Context, m *transport.Message) {
	if len(m.Data) == 0 || len(m.Data) > wire.MaxData {
		a.reject(m, "size")
		return
	}
	switch m.Attrs[wire.AttrKind] {
	case wire.KindAck:
		a.onAck(ctx, m)
	case wire.KindMsg:
		a.onMsg(ctx, m)
	default:
		a.reject(m, "kind")
	}
}

func (a *Agent) onAck(ctx context.Context, m *transport.Message) {
	ack, err := handshake.Decode(m.Data)
	if err != nil {
		a.reject(m, "ack format")
		return
	}
	now := a.cfg.Now()
	a.mu.Lock()
	p, ok := a.pending[ack.AckRef()]
	a.mu.Unlock()
	if !ok {
		a.reject(m, "ack for unknown hello")
		return
	}
	res, err := p.ini.Complete(ack, now)
	if err != nil {
		a.reject(m, "ack invalid")
		return
	}
	r, err := ratchet.NewInitiator(res.SessionKey, res.PeerRatchetPub)
	if err != nil {
		a.reject(m, "ratchet")
		return
	}
	a.mu.Lock()
	a.sess = &session{sid: res.SessionID, r: r}
	a.lastHub = now
	a.pending = map[string]pendingHello{}
	a.seenIDs, a.seenRing = map[string]struct{}{}, nil
	a.awaiting = map[string]awaitingReceipt{} // everything unconfirmed is sent again in the new session
	a.mu.Unlock()
	a.handshakes.Add(1)
	m.Ack()
	a.log.Info("session established")
	if err := a.send(ctx, &wire.Payload{Type: wire.TypeReady}); err != nil {
		a.log.Warn("could not send ready", "err", err)
	}
}

func (a *Agent) onMsg(ctx context.Context, m *transport.Message) {
	s := a.session()
	if s == nil {
		a.reject(m, "no session")
		return
	}
	sid, err := wire.SessionID(m.Data)
	if err != nil || sid != s.sid {
		a.reject(m, "other session")
		return
	}
	p, err := wire.Open(s.r, a.cfg.Hub.Name, a.cfg.Identity.Name, s.sid, m.Data, false)
	if err != nil {
		a.reject(m, "did not authenticate")
		return
	}
	now := a.cfg.Now()
	a.mu.Lock()
	a.lastHub = now
	dup := a.remember(p.ID)
	a.mu.Unlock()
	m.Ack()
	if dup {
		a.duplicates.Add(1)
		return
	}
	switch p.Type {
	case wire.TypeHBAck, wire.TypePing:
		if p.Type == wire.TypePing {
			a.send(ctx, &wire.Payload{Type: wire.TypePong})
		}
	case wire.TypeReceipt:
		a.mu.Lock()
		w, ok := a.awaiting[p.Body]
		delete(a.awaiting, p.Body)
		a.mu.Unlock()
		if ok && a.cfg.Spool != nil {
			a.cfg.Spool.Remove(w.file)
		}
	case wire.TypeLockdown:
		a.lock()
	case wire.TypeAlarm:
		if p.Expired(now, a.cfg.SkewAllowance) {
			a.expired.Add(1)
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "alarm command expired before it arrived, not applied"})
			return
		}
		a.alarmCommand(ctx, p.Body)
	case wire.TypeText, wire.TypeKey:
		if p.Expired(now, a.cfg.SkewAllowance) {
			a.expired.Add(1)
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "expired before it arrived, not executed"})
			return
		}
		if a.Locked() {
			a.refused.Add(1)
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "refused: this machine is locked"})
			return
		}
		a.execute(ctx, p)
	}
}

// remember records an id and reports whether it was already there. The set is bounded.
func (a *Agent) remember(id string) bool {
	if _, dup := a.seenIDs[id]; dup {
		return true
	}
	a.seenIDs[id] = struct{}{}
	a.seenRing = append(a.seenRing, id)
	if len(a.seenRing) > 1024 {
		delete(a.seenIDs, a.seenRing[0])
		a.seenRing = a.seenRing[1:]
	}
	return false
}

func (a *Agent) lock() {
	a.mu.Lock()
	a.locked = true
	a.mu.Unlock()
	if a.cfg.LockFile != "" {
		os.WriteFile(a.cfg.LockFile, []byte("locked by hub request\n"), 0o600)
	}
	a.log.Warn("locked by the hub: text and keys are refused until unlocked on this machine")
}

func (a *Agent) execute(ctx context.Context, p *wire.Payload) {
	var err error
	switch p.Type {
	case wire.TypeText:
		err = a.cfg.Exec.SendText(ctx, p.Body)
	case wire.TypeKey:
		err = a.cfg.Exec.SendKey(ctx, p.Body)
	}
	if err != nil {
		a.refused.Add(1)
		a.log.Warn("could not execute", "type", p.Type, "id", p.ID)
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "could not type it into the session"})
		return
	}
	a.executed.Add(1)
	a.log.Info("executed", "type", p.Type, "id", p.ID, "bytes", len(p.Body))
}

// alert reports a touch of the machine. It goes through the spool when there is one, so it is confirmed by the hub and
// survives a hub restart or a network gap; a late alert still says when it happened.
func (a *Agent) alert(ctx context.Context, e alarm.Event, held int) {
	body := a.cfg.Now().Format("2006-01-02 15:04:05 -0700") + " " + e.Detail
	if held > 0 {
		body += fmt.Sprintf(" (and %d more since the last report)", held)
	}
	a.log.Warn("alarm", "sensor", e.Sensor, "held_back", held)
	if a.cfg.Spool != nil {
		if err := a.cfg.Spool.Add(wire.TypeAlert, body); err == nil {
			return
		}
	}
	a.send(ctx, &wire.Payload{Type: wire.TypeAlert, Body: body, Meta: map[string]string{"sensor": e.Sensor}})
}

func (a *Agent) alarmCommand(ctx context.Context, body string) {
	if a.cfg.Alarm == nil {
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "this machine has no alarm configured"})
		return
	}
	cmd, arg, _ := strings.Cut(strings.TrimSpace(body), " ")
	switch cmd {
	case "on":
		if err := a.cfg.Alarm.Arm(); err != nil {
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "could not arm the alarm"})
			return
		}
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: "🚨 Alarm armed. Any touch of this machine will be reported. " + a.cfg.Alarm.Status()})
	case "off":
		if a.Locked() {
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "refused: this machine is locked, so the alarm stays armed"})
			return
		}
		if a.cfg.AlarmCode != nil {
			if err := a.cfg.AlarmCode.Verify(strings.TrimSpace(arg), a.cfg.Now()); err != nil {
				a.refused.Add(1)
				a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "alarm not disarmed: a current one-time code is required (/alarm off 123456)"})
				return
			}
		}
		if err := a.cfg.Alarm.Disarm(); err != nil {
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "could not disarm the alarm"})
			return
		}
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: "Alarm disarmed."})
	case "status", "":
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: a.cfg.Alarm.Status()})
	default:
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "unknown alarm command; use on, off or status"})
	}
}
