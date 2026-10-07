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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/alarm"
	"github.com/n0rm4l-me/petrvs-bridge/internal/approval"
	"github.com/n0rm4l-me/petrvs-bridge/internal/handshake"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
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
	// Screen reads the session's screen and starts it; nil turns off what needs that. Ext holds the settings of the
	// file transfer, the status card, auto-heal and the stuck-request check.
	Screen Screen
	Ext    Extras

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
	// ReorderWindow is how long a command waits for an earlier one that has not arrived yet. The bus does not keep
	// order, the hub numbers the commands, and the agent runs them in that order; when the earlier one has not come
	// after this long it is reported as lost and the later ones go ahead.
	ReorderWindow time.Duration // default 2s
	// MinTurn is the least time between two batches of spooled messages (default 250ms). Every batch and the hub's receipts
	// for it cost the hub's ratchet one new key, and it takes a limited number of them a second; a sender that goes as
	// fast as the receipts come would have messages refused and lost. Negative turns the pacing off (tests with a clock
	// that does not move).
	MinTurn time.Duration
}

// Stats counts what the agent has seen; the counters never include message content.
type Stats struct {
	Handshakes, Executed, Rejected, Duplicates, Expired, Refused uint64
	// Lost counts commands that never arrived in order; Late counts ones that arrived after their turn had passed.
	Lost, Late uint64
}

type session struct {
	sid [8]byte
	r   *ratchet.Ratchet
}

type awaitingReceipt struct {
	file     string
	sentAt   time.Time
	transfer string // the file transfer a part or closing item belongs to ("" for other messages)
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
	heard    bool      // the hub has answered in the current session
	seenIDs  map[string]struct{}
	seenRing []string
	awaiting map[string]awaitingReceipt // payload id -> spool file

	// spoolMu makes one pass over the spool at a time (a pass is also started by a receipt, not only by the timer), and
	// spoolWake asks for one.
	spoolMu   sync.Mutex
	spoolWake chan struct{}
	// heldByLock names the file items that a pass found held back by the lock; they are not read again until it is lifted.
	// Guarded by spoolMu, like the two below.
	heldByLock map[string]struct{}
	// turnAt is when the last batch of spooled messages went out, and turnTimer the wake-up that is set while the next
	// batch waits for its turn.
	turnAt    time.Time
	turnTimer *time.Timer

	// The last report of a message lost to the ratchet's budget, and how many were not reported since. Guarded by mu.
	lastRateLog time.Time
	rateSkipped int

	// The state of the parts beyond text and keys; each has its own file (agent_down.go, agent_ui.go, agent_mach.go).
	down downState
	ui   uiState
	mach machState

	// locked is sticky in memory. lockOnDisk says the lock file was written, so that removing it on this machine is
	// what unlocks; a lock that could not be written stays until the agent restarts.
	locked, lockOnDisk bool

	// Commands are run one at a time, in the hub's order.
	cmdNext  uint64
	cmdQ     map[uint64]*wire.Payload
	gapSince time.Time
	cmdWake  chan struct{}

	handshakes, executed, rejected, duplicates, expired, refused, lost, late atomic.Uint64
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
	def(&cfg.ReorderWindow, 2*time.Second)
	if cfg.MinTurn == 0 {
		cfg.MinTurn = 250 * time.Millisecond
	}
	a := &Agent{cfg: cfg, log: cfg.Log.With("agent", cfg.Identity.Name), pending: map[string]pendingHello{}, seenIDs: map[string]struct{}{},
		awaiting: map[string]awaitingReceipt{}, cmdNext: 1, cmdQ: map[uint64]*wire.Payload{}, cmdWake: make(chan struct{}, 1), spoolWake: make(chan struct{}, 1)}
	if cfg.Spool != nil {
		cfg.Spool.SetLog(a.log) // a file the spool sets aside is reported in the agent's log
	}
	a.initExt()
	return a, nil
}

// Stats returns the counters.
func (a *Agent) Stats() Stats {
	return Stats{a.handshakes.Load(), a.executed.Load(), a.rejected.Load(), a.duplicates.Load(), a.expired.Load(), a.refused.Load(), a.lost.Load(), a.late.Load()}
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

// lockedLocked answers whether text and keys are refused; the caller holds a.mu. It fails closed: a lock file that
// exists, or that cannot be checked, means locked. Only removing the file on this machine unlocks, and only a lock
// that was written to the file can be removed that way.
func (a *Agent) lockedLocked() bool {
	if a.cfg.LockFile == "" {
		return a.locked
	}
	_, err := os.Stat(a.cfg.LockFile)
	switch {
	case err == nil:
		a.locked, a.lockOnDisk = true, true
	case errors.Is(err, fs.ErrNotExist):
		if a.lockOnDisk {
			a.locked, a.lockOnDisk = false, false
		}
	default:
		a.locked = true // cannot tell, so stay safe
	}
	return a.locked
}

// Run connects, serves and heartbeats until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	// Run returns only after the loops of auto-heal and the stuck-request check have stopped, so that none of them is still
	// writing a notice to the spool when the caller tears the agent's directories down.
	defer func() {
		cancel()
		a.mach.wg.Wait()
	}()
	if a.cfg.LockFile != "" {
		a.mu.Lock()
		locked := a.lockedLocked()
		a.mu.Unlock()
		if locked {
			a.log.Warn("starting locked: text and keys are refused until the lock file is removed with `agent unlock`", "lock_file", a.cfg.LockFile)
		}
	}
	errc := make(chan error, 1)
	go func() { errc <- a.cfg.Transport.Receive(ctx, a.cfg.Names.AgentSub(a.cfg.Identity.Name), a.handle) }()
	go a.loop(ctx, a.cfg.HandshakeEvery, true, a.handshakeTick)
	go a.loop(ctx, a.cfg.HeartbeatEvery, false, a.heartbeatTick)
	go a.executor(ctx)
	if a.cfg.Spool != nil {
		go a.loop(ctx, a.cfg.SpoolEvery, false, a.spoolTick)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-a.spoolWake:
					a.spoolTick(ctx)
				}
			}
		}()
	}
	a.startExtras(ctx)
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
	unanswered := a.sess != nil && !a.heard
	a.mu.Unlock()
	if unanswered && !need {
		// The hub starts using the new session when it hears from it. If the first message got lost it would wait
		// for the next heartbeat; say it again instead.
		a.send(ctx, &wire.Payload{Type: wire.TypeReady})
		return
	}
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
	a.addFacts(ctx, meta)
	a.send(ctx, &wire.Payload{Type: wire.TypeHeartbeat, Meta: meta})
}

// spoolWindow is how many spooled messages may be out without a receipt. The hub opens the agent's messages with a
// ratchet that skips at most ratchet.MaxSkip keys when the bus reorders them, so a file of a hundred parts must not go
// out all at once; a receipt starts the next pass at once, so the window does not slow a transfer to a tick per window.
// Sixteen leave room for the receipts and the rest of the traffic within that limit, and halve the number of turns of the
// ratchet a file costs compared to a window of eight.
const spoolWindow = 16

// turnPoll is the longest the agent sleeps while the next batch waits for its turn, so that a clock that was moved is noticed.
const turnPoll = 50 * time.Millisecond

// spooled is a message of the spool that a pass is about to send.
type spooled struct {
	id, name string
	it       spool.Item
}

func (a *Agent) spoolTick(ctx context.Context) {
	a.spoolMu.Lock()
	defer a.spoolMu.Unlock()
	s := a.session()
	if s == nil {
		return
	}
	a.sendBatch(ctx, s, a.nextBatch())
}

// sendBatch seals every message of the batch before it publishes the first. The ratchet changes the key it sends with
// each time it has received a new key from the other end, and every such change costs the other end a step of its budget
// (ratchet.DHStepBurst). The hub answers each message at once, and the answers come back while a batch is still going out
// (the publishing takes as long as the bus takes): sealed one by one, the messages of a batch would carry a new key each
// time an answer had been read, and a window of sixteen would cost the hub sixteen steps instead of one.
func (a *Agent) sendBatch(ctx context.Context, s *session, batch []spooled) {
	type ready struct {
		spooled
		data []byte
	}
	sealed := make([]ready, 0, len(batch))
	for _, b := range batch {
		data, err := a.seal(s, &wire.Payload{ID: b.id, Type: b.it.Type, Body: b.it.Body, Meta: b.it.Meta, Buttons: b.it.Buttons})
		if err != nil {
			a.log.Warn("could not seal a spooled message", "err", err)
			break
		}
		sealed = append(sealed, ready{b, data})
	}
	for _, r := range sealed {
		// Registered before the publish: the receipt may come before Publish returns, and would find nothing to confirm.
		// The file stays until the hub confirms it: a bus that accepted the message may still lose it (for example if the
		// hub restarts and the session it was sealed for is gone).
		a.mu.Lock()
		a.awaiting[r.id] = awaitingReceipt{file: r.name, sentAt: a.cfg.Now(), transfer: r.it.Meta[wire.MetaTransfer]}
		a.mu.Unlock()
		if err := a.publishSealed(ctx, r.data); err != nil {
			a.mu.Lock()
			delete(a.awaiting, r.id)
			a.mu.Unlock()
			if ctx.Err() == nil {
				a.log.Warn("could not send a spooled message", "err", err)
			}
			return
		}
		a.noteSpooled(r.it)
	}
}

// nextBatch decides which messages of the spool go out now, under the window, the lock and the pace of the turns.
func (a *Agent) nextBatch() (batch []spooled) {
	// Only the names are listed: a transfer of a hundred parts is megabytes, and a pass is started by every receipt. The
	// body of a message is read when it is about to be sent (or judged), never for those that are out or beyond the window.
	names, err := a.cfg.Spool.Names()
	if err != nil {
		a.log.Warn("could not read the spool", "err", err)
		return nil
	}
	locked := a.Locked()
	if !locked {
		a.heldByLock = nil
	}
	inFlight := 0
	sent := false
	for i, name := range names {
		id := "sp-" + name
		a.mu.Lock()
		w, waiting := a.awaiting[id]
		stillWaiting := waiting && a.cfg.Now().Sub(w.sentAt) < a.cfg.ReceiptTimeout
		a.mu.Unlock()
		if stillWaiting {
			inFlight++
			continue
		}
		if inFlight >= spoolWindow {
			return batch // the rest waits for receipts
		}
		if _, held := a.heldByLock[name]; held {
			continue // a file item that the lock keeps back: not read again until the lock is lifted
		}
		it, ok, err := a.cfg.Spool.Read(name)
		if err != nil {
			a.log.Warn("could not read a spooled message", "err", err)
			return batch
		}
		if !ok {
			continue
		}
		// The closing item of a file waits for the receipts of the parts of its transfer (holdInSpool); what this pass has
		// gathered is not registered as sent yet, so it counts here.
		if a.holdInSpool(it) || (it.Type == wire.TypeFile && batchHas(batch, it.Meta[wire.MetaTransfer])) {
			if locked && (it.Type == wire.TypePart || it.Type == wire.TypeFile) {
				if a.heldByLock == nil {
					a.heldByLock = map[string]struct{}{}
				}
				a.heldByLock[name] = struct{}{}
			}
			continue
		}
		if !sent {
			// A batch is one turn of the hub's ratchet, and the hub takes a limited number of turns a second: the first
			// message of a pass starts a turn, and a turn is allowed only a minTurn after the one before. The receipts that
			// make room come in a burst, so a pass that starts at the first of them would send one message and use up the
			// turn: it waits until half the window is free (or everything that waits fits).
			if left := a.cfg.MinTurn - a.cfg.Now().Sub(a.turnAt); left > 0 {
				a.wakeSpoolIn(min(left, turnPoll))
				return nil
			}
			if free := spoolWindow - inFlight; inFlight > 0 && free < spoolWindow/2 && len(names)-i > free {
				return nil // each receipt starts a pass; the next one finds more room
			}
			a.turnAt, sent = a.cfg.Now(), true
		}
		batch = append(batch, spooled{id: id, name: name, it: it})
		inFlight++
	}
	return batch
}

// batchHas says whether a part or closing item of the transfer is in the batch.
func batchHas(batch []spooled, transfer string) bool {
	for _, b := range batch {
		if b.it.Meta[wire.MetaTransfer] == transfer {
			return true
		}
	}
	return false
}

// wakeSpoolIn asks for a pass over the spool after d, unless one is asked for already; the caller holds spoolMu.
func (a *Agent) wakeSpoolIn(d time.Duration) {
	if a.turnTimer != nil {
		return
	}
	a.turnTimer = time.AfterFunc(d, func() {
		a.spoolMu.Lock()
		a.turnTimer = nil
		a.spoolMu.Unlock()
		a.wakeSpool()
	})
}

// rateLogEvery is the least time between two reports of messages lost to the ratchet's budget: whoever can publish to the
// bus makes the ratchet refuse messages as fast as he likes, and must not fill the log.
const rateLogEvery = 10 * time.Second

// noteRateLimited reports a message that the ratchet refused because the hub offered new keys faster than the budget allows.
// The message is acknowledged to the bus and gone, so this line is the only trace that something was lost.
func (a *Agent) noteRateLimited() {
	now := a.cfg.Now()
	a.mu.Lock()
	quiet := now.Sub(a.lastRateLog) < rateLogEvery
	skipped := a.rateSkipped
	if quiet {
		a.rateSkipped++
	} else {
		a.lastRateLog, a.rateSkipped = now, 0
	}
	a.mu.Unlock()
	if !quiet {
		a.log.Warn("dropped a message: the ratchet refused new keys offered faster than its budget (ratchet.DHStepBurst, refilled per second); the hub goes too fast, or someone floods the bus",
			"also_since_last_log", skipped)
	}
}

// wakeSpool asks for a pass over the spool now, without waiting for the timer.
func (a *Agent) wakeSpool() {
	select {
	case a.spoolWake <- struct{}{}:
	default:
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
	data, err := a.seal(s, p)
	if err != nil {
		return err
	}
	return a.publishSealed(ctx, data)
}

// seal encrypts a payload for the hub in the session.
func (a *Agent) seal(s *session, p *wire.Payload) ([]byte, error) {
	now := a.cfg.Now()
	if p.ID == "" {
		p.ID = newID()
	}
	p.TS, p.Exp = now.Unix(), now.Add(10*time.Minute).Unix()
	return wire.Seal(s.r, a.cfg.Identity.Name, a.cfg.Hub.Name, s.sid, p)
}

func (a *Agent) publishSealed(ctx context.Context, data []byte) error {
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
	// The bus calls handlers concurrently and the hub answers a burst of queued hellos with several acks. The lookup,
	// the check and the installation of the session are one step: the first ack clears pending and the others find
	// nothing, so the agent cannot end up on another session than the one it tells the hub about.
	a.mu.Lock()
	p, ok := a.pending[ack.AckRef()]
	if !ok {
		a.mu.Unlock()
		a.reject(m, "ack for unknown hello")
		return
	}
	res, err := p.ini.Complete(ack, now)
	if err != nil {
		a.mu.Unlock()
		a.reject(m, "ack invalid")
		return
	}
	r, err := ratchet.NewInitiator(res.SessionKey, res.PeerRatchetPub)
	if err != nil {
		a.mu.Unlock()
		a.reject(m, "ratchet")
		return
	}
	a.sess = &session{sid: res.SessionID, r: r}
	a.lastHub, a.heard = now, false
	a.pending = map[string]pendingHello{}
	discarded := len(a.cmdQ)
	a.cmdNext, a.cmdQ, a.gapSince = 1, map[uint64]*wire.Payload{}, time.Time{} // the hub numbers commands per session
	a.seenIDs, a.seenRing = map[string]struct{}{}, nil
	a.awaiting = map[string]awaitingReceipt{} // everything unconfirmed is sent again in the new session
	a.mu.Unlock()
	a.handshakes.Add(1)
	m.Ack()
	a.log.Info("session established")
	if err := a.send(ctx, &wire.Payload{Type: wire.TypeReady}); err != nil {
		a.log.Warn("could not send ready", "err", err)
	}
	// The commands that waited in the queue belonged to the old session, whose numbers mean nothing in the new one, and they
	// are gone: say so, once for each (the queue holds 256 at the most), or the user waits for something that will never be typed.
	for i := 0; i < discarded; i++ {
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "a command that was waiting was dropped: the connection was re-established; send it again"})
	}
	if discarded > 0 {
		a.refused.Add(uint64(discarded))
		a.log.Warn("dropped the commands that were waiting when the session was replaced", "count", discarded)
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
		if errors.Is(err, ratchet.ErrRateLimited) {
			a.noteRateLimited()
		}
		a.reject(m, "did not authenticate")
		return
	}
	now := a.cfg.Now()
	a.mu.Lock()
	a.lastHub, a.heard = now, true
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
			a.wakeSpool() // room in the window: the next message of a transfer goes out now
		}
	case wire.TypeLockdown:
		a.lock(ctx)
	case wire.TypeAlarm:
		if p.Expired(now, a.cfg.SkewAllowance) {
			a.expired.Add(1)
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "alarm command expired before it arrived, not applied"})
			return
		}
		a.alarmCommand(ctx, p.Body)
	case wire.TypePart:
		a.onPart(ctx, p)
	case wire.TypeScreen:
		a.onScreen(ctx, p)
	default:
		if wire.IsCommand(p.Type) {
			a.enqueue(ctx, s, p)
		}
	}
}

// enqueue puts a command in its place in the hub's order. The bus may deliver out of order and the receive callbacks
// run concurrently, so nothing is run from here: the executor runs the queue, one command at a time.
func (a *Agent) enqueue(ctx context.Context, s *session, p *wire.Payload) {
	n, err := strconv.ParseUint(p.Meta[wire.MetaSeq], 10, 64)
	if err != nil || n == 0 {
		a.refused.Add(1)
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "refused: a command without a sequence number"})
		return
	}
	const window = 1000 // numbers ahead of the next one that are believed
	a.mu.Lock()
	switch {
	case a.sess != s:
		a.mu.Unlock() // the session was replaced meanwhile; the command belongs to a session that no longer exists
		return
	case n < a.cmdNext:
		a.mu.Unlock()
		a.late.Add(1)
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: fmt.Sprintf("command #%d arrived after its turn had passed, not executed", n)})
		return
	case n-a.cmdNext > window || len(a.cmdQ) >= 256:
		a.mu.Unlock()
		a.refused.Add(1)
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "refused: too many commands waiting"})
		return
	}
	a.cmdQ[n] = p
	a.mu.Unlock()
	select {
	case a.cmdWake <- struct{}{}:
	default:
	}
}

// executor runs queued commands in order until ctx ends.
func (a *Agent) executor(ctx context.Context) {
	every := a.cfg.ReorderWindow / 4
	if every < 5*time.Millisecond {
		every = 5 * time.Millisecond
	} else if every > 250*time.Millisecond {
		every = 250 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		a.runQueue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-a.cmdWake:
		case <-t.C:
		}
	}
}

// runQueue runs every command that is next in line. If the next number is missing while later ones wait, it gives the
// missing one ReorderWindow to turn up, and then reports it lost and goes on. The window is real time, not the
// configured clock: it measures how long this process has waited.
func (a *Agent) runQueue(ctx context.Context) {
	for ctx.Err() == nil {
		a.mu.Lock()
		p, ok := a.cmdQ[a.cmdNext]
		var missing uint64
		switch {
		case ok:
			delete(a.cmdQ, a.cmdNext)
			a.cmdNext++
			a.gapSince = time.Time{}
		case len(a.cmdQ) == 0:
			a.gapSince = time.Time{}
			a.mu.Unlock()
			return
		default:
			if a.gapSince.IsZero() {
				a.gapSince = time.Now()
			}
			if time.Since(a.gapSince) < a.cfg.ReorderWindow {
				a.mu.Unlock()
				return
			}
			missing = a.cmdNext
			a.cmdNext++ // several missing numbers in a row share one wait; a gap after a command that ran waits again
		}
		a.mu.Unlock()
		if missing != 0 {
			a.lost.Add(1)
			// The number goes along in Meta: the hub knows the commands whose publishing it failed, has told the user already,
			// and leaves the agent's report of the gap out.
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: fmt.Sprintf("command #%d did not arrive in time (lost or very late), not executed", missing),
				Meta: map[string]string{wire.MetaSeq: strconv.FormatUint(missing, 10)}})
			continue
		}
		a.runCommand(ctx, p)
	}
}

// runCommand applies the expiry and lock rules at the moment a command is about to run, not when it arrived.
func (a *Agent) runCommand(ctx context.Context, p *wire.Payload) {
	if p.Expired(a.cfg.Now(), a.cfg.SkewAllowance) {
		a.expired.Add(1)
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "expired before it could run, not executed"})
		return
	}
	if a.Locked() {
		a.refused.Add(1)
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "refused: this machine is locked"})
		return
	}
	a.execute(ctx, p)
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

// lock refuses text and keys from now on. The lock is kept in memory first, so it holds even if nothing can be
// written; the lock file makes it survive a restart and is what `agent unlock` removes. The hub is told which of the
// two it got.
func (a *Agent) lock(ctx context.Context) {
	// mu stays held across the write: a check that ran in between would find no file, after an `agent unlock`, and
	// take the lock back while this one is being made.
	a.mu.Lock()
	a.locked = true
	var werr error
	if a.cfg.LockFile != "" {
		if werr = os.MkdirAll(filepath.Dir(a.cfg.LockFile), 0o700); werr == nil {
			werr = os.WriteFile(a.cfg.LockFile, []byte("locked by hub request\n"), 0o600)
		}
	}
	a.lockOnDisk = a.cfg.LockFile != "" && werr == nil
	a.mu.Unlock()
	switch {
	case a.cfg.LockFile == "":
		a.log.Warn("locked by the hub (in memory only: no lock file is configured)")
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: "🔒 Locked, in memory only: no lock file is configured, so a restart of the agent unlocks it."})
	case werr != nil:
		a.log.Error("locked by the hub, but the lock file could not be written", "err", werr)
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: "🔒 Locked, in memory only: the lock file could not be written, so it holds until the agent restarts, and `agent unlock` cannot lift it before."})
	default:
		a.log.Warn("locked by the hub: text and keys are refused until unlocked on this machine")
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: "🔒 Locked: text and keys are refused until `agent unlock` is run on this machine."})
	}
}

func (a *Agent) execute(ctx context.Context, p *wire.Payload) {
	var err error
	if p.Type == wire.TypeText { // the stuck-request clock starts before typing: the reply may be spooled before this returns
		undo := a.stuckRequested(p.Body)
		defer func() {
			if err != nil {
				undo() // the text was not typed, so nothing will answer it
			}
		}()
	}
	switch p.Type {
	case wire.TypeText:
		if err = a.checkTextTarget(ctx); err == nil {
			err = a.cfg.Exec.SendText(ctx, p.Body)
		}
	case wire.TypeKey:
		if err = a.checkPrompt(ctx, p); err == nil {
			err = a.cfg.Exec.SendKey(ctx, p.Body)
		}
	case wire.TypeFile:
		err = a.executeFile(ctx, p)
	case wire.TypeStart:
		err = a.executeStart(ctx, p)
	}
	if err != nil {
		a.refused.Add(1)
		a.log.Warn("could not execute", "type", p.Type, "id", p.ID)
		msg := "could not type it into the session"
		var ue *userError
		if errors.As(err, &ue) {
			msg = ue.msg
		}
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: msg})
		return
	}
	a.executed.Add(1)
	if p.Type == wire.TypeKey { // a key name from the allow-list, not content: it is how a change of permission mode shows in the log
		a.log.Info("executed", "type", p.Type, "id", p.ID, "key", p.Body)
	} else {
		a.log.Info("executed", "type", p.Type, "id", p.ID, "bytes", len(p.Body))
	}
	// Tells the hub, which can then mark the user's message as delivered. Not spooled: if it is lost, only the mark is.
	if err := a.send(ctx, &wire.Payload{Type: wire.TypeDone, Body: p.ID}); err != nil {
		a.log.Debug("could not report a finished command", "id", p.ID, "err", err)
	}
	a.afterCommand(ctx, p)
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

// alarmStatus is the alarm's status plus, when no code protects disarming, the reminder that the chat alone decides.
func (a *Agent) alarmStatus() string {
	s := a.cfg.Alarm.Status()
	if a.cfg.AlarmCode == nil {
		s += ". No disarm code is configured on this machine: anyone who controls the chat can turn the alarm off"
	}
	return s
}

func (a *Agent) alarmCommand(ctx context.Context, body string) {
	if a.cfg.Alarm == nil {
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "this machine has no alarm configured"})
		return
	}
	cmd, arg, _ := strings.Cut(strings.TrimSpace(body), " ")
	switch cmd {
	case "on":
		already := a.cfg.Alarm.Armed()
		if err := a.cfg.Alarm.Arm(); err != nil {
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "could not arm the alarm: the state file could not be written"})
			return
		}
		head := "🚨 Alarm armed. Any touch of this machine will be reported. "
		if already {
			head = "🚨 The alarm was already armed, nothing changed. "
		}
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: head + a.alarmStatus()})
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
			a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "could not disarm the alarm: the state file could not be removed, so it stays armed"})
			return
		}
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: "Alarm disarmed."})
	case "status", "":
		a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: a.alarmStatus()})
	default:
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "unknown alarm command; use on, off or status"})
	}
}
