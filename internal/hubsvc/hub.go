// Package hubsvc is the server: it talks to the chat service, holds one ratchet
// session per agent, and relays between them over the message bus.
//
// The hub sees plaintext, because the chat service does. It is therefore kept
// small, and it cannot approve anything on an agent's behalf: approval secrets
// live on the agent.
package hubsvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claudebridge/internal/handshake"
	"claudebridge/internal/identity"
	"claudebridge/internal/ratchet"
	"claudebridge/internal/telegram"
	"claudebridge/internal/transport"
	"claudebridge/internal/wire"
)

// Config holds everything a hub needs.
type Config struct {
	Identity  *identity.Identity
	Agents    []identity.Public // pinned at enrolment; nobody else can connect
	Transport transport.Transport
	Names     wire.Names
	Telegram  telegram.Client
	// Only messages from this chat and this user are obeyed.
	ChatID, UserID int64
	Now            func() time.Time
	Log            *slog.Logger

	OfflineAfter     time.Duration // default 3m
	WatchEvery       time.Duration // default 15s
	TextTTL          time.Duration // default 10m
	KeyTTL           time.Duration // default 1m
	HelloMinInterval time.Duration // default 3s per agent
	PollTimeout      time.Duration // default 25s
	MaxReplyBytes    int           // default 20000
	// LockFile, if set, keeps a lockdown across restarts of the hub: it exists while the hub is locked down.
	LockFile string
}

// maxPending is how many unproven sessions the hub keeps per agent. An agent repeats its hello until it is answered,
// so more than one may be in flight; only one of them will be used.
const maxPending = 4

// Stats counts events without recording content.
type Stats struct {
	Unauthorized, Rejected, Hellos, HellosRefused, Relayed uint64
}

type sess struct {
	sid [8]byte
	r   *ratchet.Ratchet
	seq uint64 // last command number given in this session
}

type agentState struct {
	pub           identity.Public
	cur           *sess
	pending       []*sess // offered, not yet proved by a message from the agent
	lastHello     time.Time
	lastSeen      time.Time
	seenIDs       map[string]struct{}
	seenRing      []string
	offlineNotice bool
	info          map[string]string
	everConnected bool
}

// Hub is the server service.
type Hub struct {
	cfg   Config
	log   *slog.Logger
	guard *handshake.Guard

	mu     sync.Mutex
	agents map[string]*agentState
	active string
	locked bool

	lastUnauthLog    time.Time
	unauthSuppressed int

	unauthorized, rejected, hellos, hellosRefused, relayed atomic.Uint64
}

// New validates the configuration and returns a hub.
func New(cfg Config) (*Hub, error) {
	if cfg.Identity == nil || cfg.Transport == nil || cfg.Telegram == nil || len(cfg.Agents) == 0 {
		return nil, errors.New("hubsvc: identity, transport, telegram and at least one agent are required")
	}
	if cfg.ChatID == 0 || cfg.UserID == 0 {
		return nil, errors.New("hubsvc: chat id and user id are required")
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
	def(&cfg.OfflineAfter, 3*time.Minute)
	def(&cfg.WatchEvery, 15*time.Second)
	def(&cfg.TextTTL, 10*time.Minute)
	def(&cfg.KeyTTL, time.Minute)
	def(&cfg.HelloMinInterval, 3*time.Second)
	def(&cfg.PollTimeout, 25*time.Second)
	if cfg.MaxReplyBytes <= 0 {
		cfg.MaxReplyBytes = 20000
	}
	h := &Hub{cfg: cfg, log: cfg.Log.With("role", "hub"), guard: handshake.NewGuardPerSender(4096, 128), agents: map[string]*agentState{}}
	if cfg.LockFile != "" {
		if _, err := os.Stat(cfg.LockFile); err == nil {
			h.locked = true // a lockdown survives a restart; only /unlock ends it
		}
	}
	for _, p := range cfg.Agents {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, dup := h.agents[p.Name]; dup {
			return nil, fmt.Errorf("hubsvc: agent %q pinned twice", p.Name)
		}
		h.agents[p.Name] = &agentState{pub: p, seenIDs: map[string]struct{}{}}
	}
	names := h.agentNames()
	h.active = names[0]
	return h, nil
}

func (h *Hub) agentNames() []string {
	names := make([]string, 0, len(h.agents))
	for n := range h.agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Stats returns the counters.
func (h *Hub) Stats() Stats {
	return Stats{h.unauthorized.Load(), h.rejected.Load(), h.hellos.Load(), h.hellosRefused.Load(), h.relayed.Load()}
}

// Online reports whether the agent has an established session and was heard recently.
func (h *Hub) Online(agent string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.agents[agent]
	now := h.cfg.Now()
	return st != nil && st.cur != nil && !st.reconnecting(now) && now.Sub(st.lastSeen) < h.cfg.OfflineAfter
}

// reconnecting is true while a new session has been offered but has not yet proved itself. Commands sealed
// for the old session would be lost in that window, so none are sent.
func (st *agentState) reconnecting(now time.Time) bool {
	return len(st.pending) > 0 && now.Sub(st.lastHello) < 15*time.Second
}

// Run serves until ctx ends.
func (h *Hub) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- h.cfg.Transport.Receive(ctx, h.cfg.Names.HubSub(), h.onBus) }()
	go h.telegramLoop(ctx)
	go h.watchLoop(ctx)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func (h *Hub) say(ctx context.Context, text string) { h.sayErr(ctx, text) }

func (h *Hub) sayErr(ctx context.Context, text string) error {
	if len(text) > h.cfg.MaxReplyBytes {
		text = text[:h.cfg.MaxReplyBytes] + fmt.Sprintf("\n[cut: %d more bytes]", len(text)-h.cfg.MaxReplyBytes)
	}
	err := h.cfg.Telegram.Send(ctx, h.cfg.ChatID, text)
	if err != nil && ctx.Err() == nil {
		h.log.Warn("could not send to chat", "err", err)
	}
	return err
}

func (h *Hub) tag(name string) string {
	if len(h.agents) > 1 {
		return "[" + name + "] "
	}
	return ""
}

// ---- chat side ----

func (h *Hub) telegramLoop(ctx context.Context) {
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := h.cfg.Telegram.GetUpdates(ctx, offset, h.cfg.PollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			h.log.Warn("polling failed", "err", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			if u.ID >= offset {
				offset = u.ID + 1
			}
			if u.Text == "" {
				continue
			}
			if u.ChatID != h.cfg.ChatID || u.UserID != h.cfg.UserID {
				h.noteUnauthorized(u.ChatID, u.UserID)
				continue
			}
			h.handle(ctx, u.Text, u.Date)
		}
	}
}

// noteUnauthorized counts a message from someone who is not the configured user, and logs it at most once a minute so
// that a stranger cannot fill the log.
func (h *Hub) noteUnauthorized(chat, user int64) {
	h.unauthorized.Add(1)
	now := h.cfg.Now()
	h.mu.Lock()
	quiet := now.Sub(h.lastUnauthLog) < time.Minute
	skipped := h.unauthSuppressed
	if quiet {
		h.unauthSuppressed++
	} else {
		h.lastUnauthLog, h.unauthSuppressed = now, 0
	}
	h.mu.Unlock()
	if !quiet {
		h.log.Warn("ignored a message from an unauthorised sender", "chat", chat, "user", user, "also_since_last_log", skipped)
	}
}

var keyCommands = map[string]string{"/enter": "Enter", "/esc": "Escape", "/up": "Up", "/down": "Down", "/tab": "Tab"}

// HandleText interprets one message from the authorised user, as if it had just been typed.
func (h *Hub) HandleText(ctx context.Context, text string) { h.handle(ctx, text, time.Time{}) }

// stale reports whether a message typed at sentAt is older than ttl. Telegram keeps undelivered messages for a day,
// and a hub that was away must not act on yesterday's "/enter" or "yes" when it comes back. An unknown date is not stale.
func (h *Hub) stale(sentAt time.Time, ttl time.Duration) bool {
	return !sentAt.IsZero() && h.cfg.Now().Sub(sentAt) > ttl
}

func (h *Hub) handle(ctx context.Context, text string, sentAt time.Time) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "@") {
		name, rest, _ := strings.Cut(text[1:], " ")
		rest = strings.TrimSpace(rest)
		if _, ok := h.lookup(name); !ok || rest == "" {
			h.say(ctx, "Use @name text, with the name of a known agent. /agents lists them.")
			return
		}
		h.deliver(ctx, name, wire.TypeText, rest, h.cfg.TextTTL, sentAt)
		return
	}
	if !strings.HasPrefix(text, "/") {
		h.deliver(ctx, h.activeName(), wire.TypeText, text, h.cfg.TextTTL, sentAt)
		return
	}
	cmd, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)
	cmd = strings.ToLower(cmd)
	if key, ok := keyCommands[cmd]; ok {
		h.deliver(ctx, h.activeName(), wire.TypeKey, key, h.cfg.KeyTTL, sentAt)
		return
	}
	switch cmd {
	case "/key":
		h.deliver(ctx, h.activeName(), wire.TypeKey, arg, h.cfg.KeyTTL, sentAt)
	case "/agents", "/status":
		h.say(ctx, h.statusText())
	case "/agent":
		if h.stale(sentAt, h.cfg.KeyTTL) {
			h.say(ctx, "⚠️ Ignored an old /agent command.")
			return
		}
		if _, ok := h.lookup(arg); !ok {
			h.say(ctx, "Unknown agent. /agents lists them.")
			return
		}
		h.mu.Lock()
		h.active = arg
		h.mu.Unlock()
		h.say(ctx, "Active agent: "+arg)
	case "/alarm":
		if arg == "" {
			arg = "status"
		}
		h.deliver(ctx, h.activeName(), wire.TypeAlarm, arg, h.cfg.KeyTTL, sentAt)
	case "/lockdown": // never stale: locking is always the safe direction
		h.lockdown(ctx)
	case "/unlock":
		if h.stale(sentAt, h.cfg.KeyTTL) {
			h.say(ctx, "⚠️ Ignored an old /unlock. Send it again if you still want it.")
			return
		}
		h.mu.Lock()
		h.locked = false
		h.mu.Unlock()
		if h.cfg.LockFile != "" {
			if err := os.Remove(h.cfg.LockFile); err != nil && !os.IsNotExist(err) {
				h.log.Warn("could not remove the hub's lock file", "err", err)
			}
		}
		h.say(ctx, "The hub relays again. Agents that were locked stay locked until someone unlocks them on their own machine.")
	default:
		h.say(ctx, "Unknown command. Known: /agents /agent NAME /enter /esc /up /down /tab /key N /alarm on|off|status /lockdown /unlock, or @name text.")
	}
}

func (h *Hub) lookup(name string) (*agentState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.agents[name]
	return st, ok
}

func (h *Hub) activeName() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

func (h *Hub) statusText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.cfg.Now()
	var b strings.Builder
	for _, n := range h.agentNames() {
		st := h.agents[n]
		mark := " "
		if n == h.active {
			mark = "*"
		}
		state := "never connected"
		switch {
		case st.cur != nil && now.Sub(st.lastSeen) < h.cfg.OfflineAfter:
			state = "online"
		case st.everConnected:
			state = "offline, last seen " + now.Sub(st.lastSeen).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(&b, "%s %s: %s", mark, n, state)
		var keys []string
		for k := range st.info {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, ", %s=%s", k, st.info[k])
		}
		b.WriteByte('\n')
	}
	if h.locked {
		b.WriteString("Hub is in lockdown: nothing is relayed.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// deliver seals a command for one agent and publishes it. The command is valid for ttl from the moment the user typed
// it (sentAt, or now when that is unknown), so time spent in the chat's queue counts against it.
func (h *Hub) deliver(ctx context.Context, agent string, t wire.Type, body string, ttl time.Duration, sentAt time.Time) {
	now := h.cfg.Now()
	if sentAt.IsZero() || sentAt.After(now) {
		sentAt = now
	}
	if now.Sub(sentAt) > ttl {
		h.say(ctx, "⚠️ Ignored a message that was already older than its validity ("+ttl.String()+"). Send it again if you still want it.")
		return
	}
	h.mu.Lock()
	locked := h.locked
	st := h.agents[agent]
	var s *sess
	var silent time.Duration
	var reconnecting bool
	if st != nil {
		s, silent, reconnecting = st.cur, now.Sub(st.lastSeen), st.reconnecting(now)
	}
	h.mu.Unlock()
	switch {
	case locked:
		h.say(ctx, "Lockdown is on: nothing is sent. /unlock turns the hub's relay back on.")
		return
	case s == nil:
		h.say(ctx, "⚠️ "+agent+" is not connected.")
		return
	case reconnecting:
		h.say(ctx, "⚠️ "+agent+" is reconnecting. Send it again in a moment.")
		return
	case silent >= h.cfg.OfflineAfter:
		// A command that waits for a machine that is away would arrive stale, so say so now instead of queueing it.
		h.say(ctx, "⚠️ "+agent+" is offline (last heard "+silent.Round(time.Second).String()+" ago). Not sent.")
		return
	}
	p := &wire.Payload{ID: newID(), Type: t, Body: body, TS: now.Unix(), Exp: sentAt.Add(ttl).Unix()}
	if t == wire.TypeText || t == wire.TypeKey {
		// Commands are numbered so the agent can run them in the order they were typed even if the bus reorders them.
		h.mu.Lock()
		s.seq++
		p.Meta = map[string]string{wire.MetaSeq: strconv.FormatUint(s.seq, 10)}
		h.mu.Unlock()
	}
	data, err := wire.Seal(s.r, h.cfg.Identity.Name, agent, s.sid, p)
	switch {
	case errors.Is(err, ratchet.ErrNoSendChain):
		h.say(ctx, "⚠️ "+agent+" is still connecting, try again in a moment.")
		return
	case err != nil:
		h.say(ctx, "⚠️ Could not prepare the message.")
		return
	}
	if err := h.cfg.Transport.Publish(ctx, h.cfg.Names.ToAgent(agent), data, wire.Attrs(wire.KindMsg, agent)); err != nil {
		h.log.Warn("publish failed", "agent", agent, "err", err)
		h.say(ctx, "⚠️ The message bus did not accept the message.")
		return
	}
	h.relayed.Add(1)
}

func (h *Hub) lockdown(ctx context.Context) {
	keptNote := ""
	if h.cfg.LockFile != "" {
		err := os.MkdirAll(filepath.Dir(h.cfg.LockFile), 0o700)
		if err == nil {
			err = os.WriteFile(h.cfg.LockFile, []byte("lockdown\n"), 0o600)
		}
		if err != nil {
			h.log.Warn("could not keep the lockdown across restarts", "err", err)
			keptNote = " It will not survive a restart of the hub (the lock file could not be written)."
		}
	}
	h.mu.Lock()
	h.locked = true
	type target struct {
		name string
		s    *sess
	}
	var ts []target
	for n, st := range h.agents {
		if st.cur != nil {
			ts = append(ts, target{n, st.cur})
		}
	}
	h.mu.Unlock()
	now := h.cfg.Now()
	for _, t := range ts {
		h.sendLockdown(ctx, t.name, t.s, now)
	}
	h.say(ctx, fmt.Sprintf("Lockdown: the hub stopped relaying and told %d agent(s) to refuse text and keys until unlocked on their own machine.%s", len(ts), keptNote))
}

func (h *Hub) sendLockdown(ctx context.Context, agent string, s *sess, now time.Time) {
	p := &wire.Payload{ID: newID(), Type: wire.TypeLockdown, TS: now.Unix(), Exp: now.Add(time.Hour).Unix()}
	if data, err := wire.Seal(s.r, h.cfg.Identity.Name, agent, s.sid, p); err == nil {
		h.cfg.Transport.Publish(ctx, h.cfg.Names.ToAgent(agent), data, wire.Attrs(wire.KindMsg, agent))
	}
}

// ---- bus side ----

func (h *Hub) reject(m *transport.Message, why string) {
	h.rejected.Add(1)
	h.log.Debug("rejected message", "why", why, "bytes", len(m.Data))
	m.Ack()
}

func (h *Hub) onBus(ctx context.Context, m *transport.Message) {
	if len(m.Data) == 0 || len(m.Data) > wire.MaxData {
		h.reject(m, "size")
		return
	}
	switch m.Attrs[wire.AttrKind] {
	case wire.KindHello:
		h.onHello(ctx, m)
	case wire.KindMsg:
		h.onMsg(ctx, m)
	default:
		h.reject(m, "kind")
	}
}

func (h *Hub) onHello(ctx context.Context, m *transport.Message) {
	hello, err := handshake.Decode(m.Data)
	if err != nil {
		h.reject(m, "hello format")
		return
	}
	h.mu.Lock()
	st := h.agents[hello.From]
	if st == nil {
		h.mu.Unlock()
		h.reject(m, "unknown agent")
		return
	}
	pub := st.pub
	h.mu.Unlock()
	now := h.cfg.Now()
	// Cheap checks first: the signature proves the hello is the pinned agent's, and only then is it worth counting
	// against that agent's rate or spending a key agreement on it. Forged hellos cost one signature check and nothing else.
	if err := handshake.VerifyHello(h.cfg.Identity, pub, hello, now); err != nil {
		h.hellosRefused.Add(1)
		h.reject(m, "hello refused")
		return
	}
	h.mu.Lock()
	tooSoon := now.Sub(st.lastHello) < h.cfg.HelloMinInterval
	h.mu.Unlock()
	if tooSoon {
		h.hellosRefused.Add(1)
		h.reject(m, "hello rate limit")
		return
	}
	ack, res, err := handshake.Respond(h.cfg.Identity, pub, hello, now, h.guard)
	if err != nil {
		h.hellosRefused.Add(1)
		h.reject(m, "hello refused")
		return
	}
	h.mu.Lock()
	st.lastHello = now
	st.pending = append(st.pending, &sess{sid: res.SessionID, r: ratchet.NewResponder(res.SessionKey, res.OwnRatchetKey)})
	if len(st.pending) > maxPending {
		st.pending = st.pending[len(st.pending)-maxPending:]
	}
	h.mu.Unlock()
	data, err := ack.Encode()
	if err != nil {
		h.reject(m, "encode")
		return
	}
	if err := h.cfg.Transport.Publish(ctx, h.cfg.Names.ToAgent(hello.From), data, wire.Attrs(wire.KindAck, hello.From)); err != nil {
		h.log.Warn("could not publish ack", "err", err)
		m.Nack() // the agent will also retry, but a transient bus failure deserves another attempt
		return
	}
	h.hellos.Add(1)
	m.Ack()
}

// find returns the agent whose current or pending session has this id.
func (h *Hub) find(sid [8]byte) (name string, s *sess, pending bool) {
	for n, st := range h.agents {
		if st.cur != nil && st.cur.sid == sid {
			return n, st.cur, false
		}
		for _, p := range st.pending {
			if p.sid == sid {
				return n, p, true
			}
		}
	}
	return "", nil, false
}

func (h *Hub) onMsg(ctx context.Context, m *transport.Message) {
	sid, err := wire.SessionID(m.Data)
	if err != nil {
		h.reject(m, "no session id")
		return
	}
	h.mu.Lock()
	name, s, isPending := h.find(sid)
	h.mu.Unlock()
	if s == nil {
		h.reject(m, "unknown session")
		return
	}
	p, err := wire.Open(s.r, name, h.cfg.Identity.Name, sid, m.Data, true)
	if err != nil {
		h.reject(m, "did not authenticate")
		return
	}
	m.Ack()
	now := h.cfg.Now()
	h.mu.Lock()
	st := h.agents[name]
	promoted := false
	if isPending { // the new session proved itself: it replaces the old one and the other offers are dead
		for _, p := range st.pending {
			if p == s {
				st.cur, st.pending, promoted = s, nil, true // seen ids are kept: an agent resends unconfirmed messages in the new session
				break
			}
		}
	}
	locked := h.locked
	st.lastSeen = now
	comeback := st.offlineNotice
	st.offlineNotice = false
	first := !st.everConnected
	st.everConnected = true
	_, dup := st.seenIDs[p.ID]
	h.mu.Unlock()
	if promoted && locked {
		// A lockdown reaches only the sessions that existed when it was ordered. A machine that reconnects later,
		// for example after the hub restarted, must hear it again.
		h.sendLockdown(ctx, name, s, now)
	}
	if dup {
		// Already shown. The agent is asking again because it never got the receipt: confirm, do not repeat.
		if p.Type == wire.TypeReply || p.Type == wire.TypeNotice || p.Type == wire.TypeAlert {
			h.receipt(ctx, name, s, p.ID)
		}
		return
	}
	switch p.Type {
	case wire.TypeReady:
		if comeback || first {
			h.say(ctx, "✅ "+name+" is connected.")
		}
		h.ackHeartbeat(ctx, name, s) // tells the agent the session works, so it stops repeating "ready"
	case wire.TypeHeartbeat:
		h.mu.Lock()
		st.info = p.Meta
		h.mu.Unlock()
		if comeback {
			h.say(ctx, "✅ "+name+" is back.")
		}
		h.ackHeartbeat(ctx, name, s)
	case wire.TypeReply, wire.TypeNotice, wire.TypeAlert:
		// Confirm only after the chat accepted it; if it did not, stay silent so the agent sends it again.
		text := h.tag(name) + p.Body
		if p.Type == wire.TypeAlert {
			text = "🚨 ALARM " + text
		}
		if err := h.sayErr(ctx, text); err == nil {
			h.markSeen(st, p.ID)
			h.receipt(ctx, name, s, p.ID)
		}
		return
	case wire.TypeDropped:
		h.say(ctx, "⚠️ "+h.tag(name)+p.Body)
	}
	h.markSeen(st, p.ID)
}

func (h *Hub) markSeen(st *agentState, id string) {
	h.mu.Lock()
	h.remember(st, id)
	h.mu.Unlock()
}

func (h *Hub) receipt(ctx context.Context, agent string, s *sess, id string) {
	now := h.cfg.Now()
	p := &wire.Payload{ID: newID(), Type: wire.TypeReceipt, Body: id, TS: now.Unix(), Exp: now.Add(time.Minute).Unix()}
	data, err := wire.Seal(s.r, h.cfg.Identity.Name, agent, s.sid, p)
	if err != nil {
		return
	}
	h.cfg.Transport.Publish(ctx, h.cfg.Names.ToAgent(agent), data, wire.Attrs(wire.KindMsg, agent))
}

func (h *Hub) remember(st *agentState, id string) bool {
	if _, dup := st.seenIDs[id]; dup {
		return true
	}
	st.seenIDs[id] = struct{}{}
	st.seenRing = append(st.seenRing, id)
	if len(st.seenRing) > 1024 {
		delete(st.seenIDs, st.seenRing[0])
		st.seenRing = st.seenRing[1:]
	}
	return false
}

func (h *Hub) ackHeartbeat(ctx context.Context, agent string, s *sess) {
	now := h.cfg.Now()
	p := &wire.Payload{ID: newID(), Type: wire.TypeHBAck, TS: now.Unix(), Exp: now.Add(time.Minute).Unix()}
	data, err := wire.Seal(s.r, h.cfg.Identity.Name, agent, s.sid, p)
	if err != nil {
		return
	}
	h.cfg.Transport.Publish(ctx, h.cfg.Names.ToAgent(agent), data, wire.Attrs(wire.KindMsg, agent))
}

// ---- liveness ----

func (h *Hub) watchLoop(ctx context.Context) {
	t := time.NewTicker(h.cfg.WatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.checkOffline(ctx)
		}
	}
}

func (h *Hub) checkOffline(ctx context.Context) {
	var notify []string
	h.mu.Lock()
	now := h.cfg.Now()
	for n, st := range h.agents {
		if st.everConnected && !st.offlineNotice && now.Sub(st.lastSeen) > h.cfg.OfflineAfter {
			st.offlineNotice = true
			msg := fmt.Sprintf("⚠️ %s went offline (last heard %s ago).", n, now.Sub(st.lastSeen).Round(time.Second))
			if st.info["alarm"] == "on" {
				msg = "🚨 " + msg + " Its alarm was armed."
			}
			notify = append(notify, msg)
		}
	}
	h.mu.Unlock()
	sort.Strings(notify)
	for _, text := range notify {
		h.say(ctx, text)
	}
}
