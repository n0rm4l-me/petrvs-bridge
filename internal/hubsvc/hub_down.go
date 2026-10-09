package hubsvc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Files from the chat to a machine: a photo or document of the user is fetched from Telegram and sent on, part by part,
// to the agent, followed by the command that makes the agent save it.

const (
	// partWindow is how many parts may be out without the agent's answer. The agent's ratchet skips at most ratchet.MaxSkip
	// (50) message keys when the bus reorders messages, and two transfers plus the rest of the traffic share that: a
	// hundred parts sent at once would be rejected. Sixteen keep well within it (the answers and the rest of the traffic
	// add a few), and halve the number of turns a file takes compared to eight.
	partWindow = 16
	// defaultMinTurn is the least time between two batches of parts to one agent. Every batch and the answers to it make
	// each end of the ratchet take a step with a new key, and a receiver spends from a budget of 20 such steps, refilled at
	// 5 a second (ratchet.DHStepBurst): a sender that goes as fast as the answers come would be refused ("too many new keys
	// offered") and the parts it sent lost. Four turns a second stay under the refill.
	defaultMinTurn = 250 * time.Millisecond
	// turnPoll is the longest the sender sleeps while it waits for its turn, so that it notices a clock that was moved.
	turnPoll = 50 * time.Millisecond
	// maxTransfers is how many files are on their way at the same time.
	maxTransfers = 2
	// partAckWait is how long the hub waits for the answer to a part before it gives up on the file.
	partAckWait = 10 * time.Second
	// maxCaptionBytes bounds the caption sent on; Telegram allows 1024 characters, which may be more bytes.
	maxCaptionBytes = 1000
	// minBudget is the least time a transfer gets, however little of the command's validity is left.
	minBudget = 5 * time.Second
	// goodbyeWait is how long the hub spends telling the chat that a transfer was cut short by its own shutdown.
	goodbyeWait = 5 * time.Second
)

type downState struct {
	mu      sync.Mutex
	running int
	// waiting maps a part that is out ("agent/payload id") to the transfer that waits for its answer.
	waiting map[string]chan<- struct{}
	ackWait time.Duration
	minTurn time.Duration
	turnAt  map[string]time.Time // when the last batch of parts went to each agent (the hub's clock)
	gates   map[string]*agentGate

	lastRateLog time.Time // when a message lost to the ratchet's budget was last reported, and how many were not since
	rateSkipped int

	unsent  map[unsentKey]struct{} // numbers of commands the bus did not take, oldest last in unsentQ
	unsentQ []unsentKey
}

// unsentKey names a command by the session it was numbered in and its number.
type unsentKey struct {
	sid [8]byte
	n   uint64
}

// maxUnsent bounds how many unsent commands are remembered.
const maxUnsent = 64

func (d *downState) init(h *Hub) {
	d.waiting = map[string]chan<- struct{}{}
	d.ackWait = partAckWait
	d.minTurn = h.cfg.MinTurn
	if d.minTurn == 0 {
		d.minTurn = defaultMinTurn
	}
	d.turnAt = map[string]time.Time{}
	d.gates = map[string]*agentGate{}
	d.unsent = map[unsentKey]struct{}{}
}

// noteUnsent remembers that command n of the session could not be published and that the chat has been told so. The agent
// will find a gap at that number and report it as lost; the user knows already (wasUnsent).
func (h *Hub) noteUnsent(s *sess, n uint64) {
	d := &h.down
	k := unsentKey{s.sid, n}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.unsent[k]; ok {
		return
	}
	d.unsent[k] = struct{}{}
	d.unsentQ = append(d.unsentQ, k)
	if len(d.unsentQ) > maxUnsent {
		delete(d.unsent, d.unsentQ[0])
		d.unsentQ = d.unsentQ[1:]
	}
}

// wasUnsent says whether a report of the agent is about a command that the hub failed to publish, by the number the agent
// names in it (the agent names it only in its report of a gap).
func (h *Hub) wasUnsent(s *sess, num string) bool {
	n, ok := upDigits(num, 12)
	if !ok {
		return false
	}
	d := &h.down
	d.mu.Lock()
	defer d.mu.Unlock()
	_, known := d.unsent[unsentKey{s.sid, uint64(n)}]
	return known
}

// start claims one of the places for a transfer; false when they are taken.
func (d *downState) start() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running >= maxTransfers {
		return false
	}
	d.running++
	return true
}

func (d *downState) finish() {
	d.mu.Lock()
	d.running--
	d.mu.Unlock()
}

func (d *downState) wait() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ackWait
}

// fileMax is the largest file taken from the chat or sent to it.
func (h *Hub) fileMax() int64 {
	if h.cfg.MaxFileBytes > 0 {
		return h.cfg.MaxFileBytes
	}
	return filetx.DefaultMaxBytes
}

func megabytes(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// onMedia receives a photo or document that the authorised user sent to the bot. The checks that cost nothing come first;
// the fetching and the sending happen in a goroutine of their own, since the chat's update loop must go on.
func (h *Hub) onMedia(ctx context.Context, u telegram.Update) {
	ref := u.Media
	if ref == nil {
		return
	}
	fetcher, ok := h.cfg.Telegram.(telegram.FileFetcher)
	if !ok {
		h.say(ctx, "⚠️ This hub cannot take files from the chat.")
		return
	}
	limit := h.fileMax()
	if ref.Size > limit {
		h.say(ctx, fmt.Sprintf("⚠️ The file is too large (%s; the limit is %s). Not sent.", megabytes(ref.Size), megabytes(limit)))
		return
	}
	if h.stale(u.Date, h.cfg.TextTTL) { // a backlog from a time the hub was away: the file would be stale on arrival
		h.say(ctx, "⚠️ Ignored an old file (it was sent more than "+h.cfg.TextTTL.String()+" ago). Send it again if you still want it.")
		return
	}
	h.mu.Lock()
	locked := h.locked
	h.mu.Unlock()
	if locked {
		h.say(ctx, "Lockdown is on: nothing is sent. /unlock turns the hub's relay back on.")
		return
	}
	agent, ok := h.destination(ctx, h.replyOf(u.ReplyToID))
	if !ok {
		return
	}
	if why := h.cannotTake(agent); why != "" {
		h.sayAs(ctx, agent, "⚠️ "+agent+" "+why+". The file was not sent.")
		return
	}
	if !h.down.start() {
		h.say(ctx, "⚠️ The hub is busy with other files, try again in a moment.")
		return
	}
	// The place in line is taken now, in the order the chat delivered the messages: what the user types after the file
	// must be typed after it, however long the transfer takes (see agentGate).
	res := h.gateReserve(agent)
	if res == nil {
		h.down.finish()
		h.sayAs(ctx, agent, "⚠️ Too many messages are waiting for the file that is still on its way to "+agent+". This file was not sent; send it again in a moment.")
		return
	}
	h.goBg(func() {
		defer h.down.finish()
		// The deferred calls run in the reverse order, also when the transfer panics: the place in line is given up if the
		// closing command did not take it, and then the messages that waited behind the file are sent.
		defer h.gateRelease(ctx, agent, res)
		defer func() {
			if r := recover(); r != nil {
				h.log.Error("a file transfer from the chat panicked", "agent", agent, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				h.failFile(ctx, agent, "The file could not be sent because of an error in the hub. Send it again.")
			}
		}()
		h.transferFile(ctx, fetcher, agent, u, limit, res)
	})
}

// cannotTake says why an agent cannot take a file now, or "" when it can: not connected, reconnecting, offline, or locked
// (as its last heartbeat said; the agent itself refuses in any case).
func (h *Hub) cannotTake(agent string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.agents[agent]
	now := h.cfg.Now()
	switch {
	case st == nil || st.cur == nil:
		return "is not connected"
	case st.reconnecting(now):
		return "is reconnecting, try again in a moment"
	case now.Sub(st.lastSeen) >= h.cfg.OfflineAfter:
		return "is offline (last heard " + now.Sub(st.lastSeen).Round(time.Second).String() + " ago)"
	case st.info["locked"] == "true":
		return "is locked (unlock it on its machine)"
	}
	return ""
}

// photoName is the name given to a photo, which Telegram sends without one.
func photoName(at time.Time) string { return "photo-" + at.UTC().Format("20060102-150405") + ".jpg" }

// mimeExt gives a document that came without a name an extension from its type, so that the session can tell a picture or
// a PDF when it looks at the file.
var mimeExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
	"application/pdf": ".pdf", "text/plain": ".txt"}

// fileNameFor is the name the file is sent under: a safe one, whatever the sender called it.
func fileNameFor(ref telegram.FileRef, at time.Time) string {
	if ref.Photo {
		return photoName(at)
	}
	name := filetx.SafeName(ref.Name)
	if strings.TrimSpace(ref.Name) == "" {
		name += mimeExt[strings.ToLower(ref.Mime)]
	}
	return name
}

// cutBytes cuts s to at most n bytes without splitting a character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// failFile tells the chat that a file did not go. It does not depend on the transfer's own context, which may have run
// out or been cancelled by the hub's shutdown: the user should hear about a file that was lost either way.
func (h *Hub) failFile(parent context.Context, agent, text string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), goodbyeWait)
	defer cancel()
	h.sayAs(ctx, agent, "⚠️ "+text)
}

// transferFile fetches the file and sends it to the agent. It runs in its own goroutine and ends with the closing `file`
// command, or with a message to the chat saying what went wrong.
func (h *Hub) transferFile(parent context.Context, fetcher telegram.FileFetcher, agent string, u telegram.Update, limit int64, res *gateEntry) {
	ref := *u.Media
	ctx, cancel := context.WithTimeout(parent, h.budget(u.Date))
	defer cancel()

	data, err := fetcher.Fetch(ctx, ref, limit)
	switch {
	case parent.Err() != nil:
		h.failFile(parent, agent, "The hub was stopped while it fetched your file, so it was not sent. Send it again.")
		return
	case errors.Is(err, telegram.ErrFileTooLarge):
		h.failFile(parent, agent, "The file is too large to fetch (this hub takes up to "+megabytes(limit)+", and Telegram lets a bot download at most 20 MB). Not sent.")
		return
	case err != nil:
		var rl telegram.ErrRateLimited
		if errors.As(err, &rl) {
			h.failFile(parent, agent, "Telegram asks to slow down, so the file could not be fetched. Try again in a moment.")
		} else {
			h.failFile(parent, agent, "Could not fetch the file from Telegram. Send it again.")
		}
		h.log.Warn("could not fetch a file from the chat", "agent", agent, "err", err)
		return
	case len(data) == 0:
		h.failFile(parent, agent, "The file is empty. Not sent.")
		return
	case int64(len(data)) > limit:
		h.failFile(parent, agent, "The file is too large (the limit is "+megabytes(limit)+"). Not sent.")
		return
	}

	at := u.Date
	if at.IsZero() {
		at = h.cfg.Now()
	}
	name := fileNameFor(ref, at)
	chunks := filetx.Chunks(data)
	t := filetx.NewID()
	if err := h.sendParts(ctx, agent, t, chunks); err != nil {
		if parent.Err() != nil {
			h.failFile(parent, agent, "The hub was stopped while it sent your file to "+agent+", so it did not arrive. Send it again.")
		} else {
			h.failFile(parent, agent, err.Error())
		}
		return
	}
	meta := map[string]string{
		wire.MetaTransfer: t, wire.MetaParts: strconv.Itoa(len(chunks)), wire.MetaName: name,
		wire.MetaSize: strconv.Itoa(len(data)), wire.MetaSHA: filetx.SHA(data), wire.MetaKind: filetx.Kind(name, int64(len(data))),
	}
	h.log.Info("file from the chat sent on", "agent", agent, "kind", meta[wire.MetaKind], "bytes", len(data), "parts", len(chunks))
	// The closing command is numbered with the text and keys and carries the chat message, so that the delivered mark
	// goes on the user's file message once the agent has told its session. It goes out from the place in line that was
	// taken for it, ahead of everything typed since (gateRelease, when this returns).
	h.gateFill(res, heldCall{agent: agent, t: wire.TypeFile, body: cutBytes(u.Text, maxCaptionBytes), meta: meta, ttl: h.cfg.TextTTL, sentAt: u.Date, msgID: u.MessageID})
}

// budget is how long a transfer may take: what is left of the validity of the command it ends with.
func (h *Hub) budget(sentAt time.Time) time.Duration {
	left := h.cfg.TextTTL
	if !sentAt.IsZero() {
		left -= h.cfg.Now().Sub(sentAt)
	}
	return max(left, minBudget)
}

// sendParts sends the chunks with a sliding window, at a pace the ratchet of the agent can follow: at most partWindow are
// out at the same time, and parts go out in batches, one batch per minTurn to an agent (whatever transfers are on their
// way to it), each as large as the free part of the window, which is at least half of it unless little is left to send.
// The answers to a batch come back in one burst, so a batch and its answers are one turn of the ratchet at each end, and a
// window of sixteen halves the turns of a window of eight. It returns an error whose text is for the chat.
func (h *Hub) sendParts(ctx context.Context, agent, t string, chunks [][]byte) error {
	d := &h.down
	acks := make(chan struct{}, partWindow+1)
	var registered []string
	defer func() {
		d.mu.Lock()
		for _, k := range registered {
			delete(d.waiting, k)
		}
		d.mu.Unlock()
	}()
	register := func(id string) {
		k := agent + "/" + id
		d.mu.Lock()
		d.waiting[k] = acks
		d.mu.Unlock()
		registered = append(registered, k)
	}
	wait := d.wait()
	out, acked, next := 0, 0, 0
	last := time.Now() // the last sign that the agent is taking the parts: an answer, or a batch into an empty window
	for acked < len(chunks) {
		var turn, patience <-chan time.Time
		var timers []*time.Timer
		if free := partWindow - out; next < len(chunks) && free >= min(partWindow/2, len(chunks)-next) {
			left := h.takeTurn(agent)
			if left == 0 {
				// The whole batch is sealed before the first part goes out: see sealPart.
				n := min(free, len(chunks)-next)
				batch := make([]sealedPart, 0, n)
				for k := 0; k < n; k++ {
					meta := map[string]string{wire.MetaTransfer: t, wire.MetaIndex: strconv.Itoa(next + k)}
					sp, err := h.sealPart(agent, filetx.EncodePart(chunks[next+k]), meta)
					if err != nil {
						return err
					}
					register(sp.id) // the agent's answer names it, and may come before Publish returns
					batch = append(batch, sp)
				}
				for _, sp := range batch {
					if err := h.publishPart(ctx, agent, sp); err != nil {
						return err
					}
					if out == 0 {
						last = time.Now()
					}
					out++
					next++
				}
				continue
			}
			tm := time.NewTimer(min(left, turnPoll))
			timers = append(timers, tm)
			turn = tm.C
		}
		if out > 0 {
			tm := time.NewTimer(max(wait-time.Since(last), 0))
			timers = append(timers, tm)
			patience = tm.C
		}
		var err error
		select {
		case <-acks:
			out--
			acked++
			last = time.Now()
		case <-turn:
		case <-patience:
			err = fmt.Errorf("%s did not take the file (%d of %d parts were confirmed). Send it again.", agent, acked, len(chunks))
		case <-ctx.Done():
			err = fmt.Errorf("%s did not take the file in time (%d of %d parts were confirmed). Send it again.", agent, acked, len(chunks))
		}
		for _, tm := range timers {
			tm.Stop()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// rateLogEvery is the least time between two reports of messages lost to the ratchet's budget: a stranger who can publish
// to the bus makes it refuse messages as fast as he can send them, and must not fill the log.
const rateLogEvery = 10 * time.Second

// noteRateLimited reports a message that the ratchet refused because the agent offered new keys faster than the budget
// allows. The message is acknowledged to the bus and gone (it cannot be opened later), so this line is the only trace that
// something was lost, and of who went too fast.
func (h *Hub) noteRateLimited(agent string) {
	d := &h.down
	now := h.cfg.Now()
	d.mu.Lock()
	quiet := now.Sub(d.lastRateLog) < rateLogEvery
	skipped := d.rateSkipped
	if quiet {
		d.rateSkipped++
	} else {
		d.lastRateLog, d.rateSkipped = now, 0
	}
	d.mu.Unlock()
	if !quiet {
		h.log.Warn("dropped a message: the ratchet refused new keys offered faster than its budget (ratchet.DHStepBurst, refilled per second); the sender goes too fast, or someone floods the bus",
			"agent", agent, "also_since_last_log", skipped)
	}
}

// takeTurn says how long the hub must wait before it may send the next batch of parts to the agent, by the hub's clock;
// zero means now, and the turn is taken.
func (h *Hub) takeTurn(agent string) time.Duration {
	d := &h.down
	now := h.cfg.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if left := d.minTurn - now.Sub(d.turnAt[agent]); left > 0 {
		return left
	}
	d.turnAt[agent] = now
	return 0
}

// sealedPart is a part ready to be published.
type sealedPart struct {
	id   string
	data []byte
}

// sealPart seals one part for the agent. It is deliverWith's quieter sibling for a transfer of many parts: it says nothing
// to the chat (the caller does, once). A part is not a command, so it is not numbered.
//
// The parts of a batch are all sealed before the first is published. The ratchet sends with a new key each time it has
// received one from the other end, and every new key costs the receiver a step of its budget (ratchet.DHStepBurst). The
// agent answers each part at once, and its answers come back while the batch is still going out (publishing takes as long
// as the bus takes): sealed one by one, the parts would carry a new key whenever an answer had been read, and a batch would
// cost many steps instead of one.
func (h *Hub) sealPart(agent, body string, meta map[string]string) (sealedPart, error) {
	now := h.cfg.Now()
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
		return sealedPart{}, chatError("Lockdown is on: the file was not sent completely. /unlock turns the hub's relay back on.")
	case s == nil:
		return sealedPart{}, fmt.Errorf("%s is not connected. The file was not sent.", agent)
	case reconnecting:
		return sealedPart{}, fmt.Errorf("%s is reconnecting. The file was not sent; send it again in a moment.", agent)
	case silent >= h.cfg.OfflineAfter:
		return sealedPart{}, fmt.Errorf("%s went offline (last heard %s ago). The file was not sent.", agent, silent.Round(time.Second))
	}
	p := &wire.Payload{ID: newID(), Type: wire.TypePart, Body: body, TS: now.Unix(), Exp: now.Add(h.cfg.TextTTL).Unix(), Meta: meta}
	data, err := wire.Seal(s.r, h.cfg.Identity.Name, agent, s.sid, p)
	if err != nil {
		if errors.Is(err, ratchet.ErrNoSendChain) {
			return sealedPart{}, fmt.Errorf("%s is still connecting. The file was not sent; try again in a moment.", agent)
		}
		return sealedPart{}, chatError("Could not prepare the file for sending.")
	}
	return sealedPart{id: p.ID, data: data}, nil
}

// publishPart publishes a sealed part, unless the hub was locked down since it was sealed.
func (h *Hub) publishPart(ctx context.Context, agent string, sp sealedPart) error {
	h.mu.Lock()
	locked := h.locked
	h.mu.Unlock()
	if locked {
		return chatError("Lockdown is on: the file was not sent completely. /unlock turns the hub's relay back on.")
	}
	if err := h.publish(ctx, agent, sp.data); err != nil {
		return chatError("The message bus did not accept the file. Send it again.")
	}
	h.relayed.Add(1)
	h.log.Debug("relayed", "agent", agent, "type", string(wire.TypePart), "id", sp.id)
	return nil
}

// chatError is an error whose text is written to the chat as it is.
type chatError string

func (e chatError) Error() string { return string(e) }

// noteDone is told of each `done` an agent reports (for a command, or for a part it has stored). An answer for a part that
// is out lets the transfer send the next one; any other is not ours.
func (h *Hub) noteDone(agent, id string) {
	d := &h.down
	k := agent + "/" + id
	d.mu.Lock()
	ch := d.waiting[k]
	delete(d.waiting, k)
	d.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default: // never blocks the bus; the channel holds a full window
	}
}

// ---- order ----

// A command gets its number when the hub sends it, and the agent types the commands in the order of the numbers. A file
// is sent when its last part is through, seconds after the user sent it, so what the user types in the meantime would get
// lower numbers and be typed first: "what is in the photo I just sent?" before the photo. The agentGate keeps the order of
// the chat. The place of a file is taken when the message arrives (a reservation), and every command for that agent that
// comes after is held, in order, behind it; when the transfer ends, with its closing command or without, the queue is sent.
// A reservation cannot simply be given a number ahead of time: the agent waits for a missing number only a few seconds
// and then calls the command lost.

// gateMax bounds what is held for one agent, reservations included. It is far more than anyone types while a file goes (a
// file takes seconds; the budget of a transfer is minutes at the very most), and it keeps the burst that follows the
// release well under what the agent's ratchet skips when the bus reorders messages (ratchet.MaxSkip, 50).
const gateMax = 32

// heldCall is a command as deliverWith takes it.
type heldCall struct {
	agent  string
	t      wire.Type
	body   string
	meta   map[string]string
	ttl    time.Duration
	sentAt time.Time
	msgID  int64
}

// gateEntry is a place in the line of one agent: a command that waits its turn, or the place of a file on its way.
type gateEntry struct {
	reservation bool
	filled      bool // the call is there; a command is filled from the start
	skip        bool // a reservation whose transfer ended without a closing command
	call        heldCall
}

func (e *gateEntry) ready() bool { return e.filled || e.skip }

// agentGate is the line of one agent. It exists while something is in it or a release is sending it on.
type agentGate struct {
	q        []*gateEntry
	flushing bool // a release is sending the ready entries at the head, one by one; new commands join the end
}

type gateFlushKey struct{}

// gateCtx marks the context of a delivery that comes out of the queue, so that deliverWith does not hold it again.
func gateCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, gateFlushKey{}, true)
}

// gateReserve takes the place of a file at the end of the line; nil when the line is full.
func (h *Hub) gateReserve(agent string) *gateEntry {
	d := &h.down
	d.mu.Lock()
	defer d.mu.Unlock()
	g := d.gates[agent]
	if g == nil {
		g = &agentGate{}
		d.gates[agent] = g
	}
	if len(g.q) >= gateMax {
		return nil
	}
	e := &gateEntry{reservation: true}
	g.q = append(g.q, e)
	return e
}

// gateFill puts the closing command of a file in its place; it is sent by gateRelease.
func (h *Hub) gateFill(e *gateEntry, c heldCall) {
	d := &h.down
	d.mu.Lock()
	e.call, e.filled = c, true
	d.mu.Unlock()
}

// gateRelease ends a transfer's hold on the line: a place that was not filled is given up, and what can go, goes, in order.
// It runs when the transfer's goroutine ends, however it ends.
func (h *Hub) gateRelease(ctx context.Context, agent string, e *gateEntry) {
	d := &h.down
	d.mu.Lock()
	if !e.filled {
		e.skip = true
	}
	d.mu.Unlock()
	h.gateRun(ctx, agent)
}

// gateHold is called by deliverWith for every command: it keeps the command back, in order, when a file is on its way to
// the agent or earlier commands are still being sent on, and reports true. Commands that do not change the session
// (a screen request, an alarm command) go at once, and so does everything while the hub is locked down: deliverWith then
// says so, as it does for any command.
func (h *Hub) gateHold(ctx context.Context, agent string, t wire.Type, body string, meta map[string]string, ttl time.Duration, sentAt time.Time, msgID int64) bool {
	if !wire.IsCommand(t) || ctx.Value(gateFlushKey{}) != nil {
		return false
	}
	h.mu.Lock()
	locked := h.locked
	h.mu.Unlock()
	if locked {
		return false
	}
	d := &h.down
	d.mu.Lock()
	g := d.gates[agent]
	if g == nil || (len(g.q) == 0 && !g.flushing) {
		d.mu.Unlock()
		return false
	}
	if len(g.q) >= gateMax {
		d.mu.Unlock()
		h.sayAs(ctx, agent, "⚠️ Too many messages are waiting for a file that is still on its way to "+agent+". This one was not sent; send it again in a moment.")
		return true
	}
	c := heldCall{agent: agent, t: t, body: body, ttl: ttl, sentAt: sentAt, msgID: msgID}
	if len(meta) > 0 {
		c.meta = make(map[string]string, len(meta))
		for k, v := range meta {
			c.meta[k] = v
		}
	}
	g.q = append(g.q, &gateEntry{filled: true, call: c})
	d.mu.Unlock()
	h.log.Debug("held a command behind a file", "agent", agent, "type", string(t))
	return true
}

// gateRun sends the entries at the head of the line that are ready, in order, until it meets a place whose file is still
// on its way or the line is empty. Only one run goes at a time per agent: a command that arrives meanwhile joins the end
// and is sent by the run that is going, so the order cannot be broken by a command overtaking the ones that are being sent.
// Every entry goes through deliverWith, which checks the lockdown, the age and the state of the agent as for any command.
func (h *Hub) gateRun(ctx context.Context, agent string) {
	d := &h.down
	d.mu.Lock()
	g := d.gates[agent]
	if g == nil || g.flushing {
		d.mu.Unlock()
		return
	}
	g.flushing = true
	d.mu.Unlock()
	finished := false
	defer func() {
		if !finished { // a panic in a delivery: leave the line to the next release instead of blocking it for ever
			d.mu.Lock()
			g.flushing = false
			d.mu.Unlock()
		}
	}()
	lost := 0
	for {
		d.mu.Lock()
		if len(g.q) == 0 || !g.q[0].ready() {
			g.flushing = false
			if len(g.q) == 0 && d.gates[agent] == g {
				delete(d.gates, agent)
			}
			d.mu.Unlock()
			break
		}
		e := g.q[0]
		g.q = g.q[1:]
		d.mu.Unlock()
		switch {
		case e.skip:
		case ctx.Err() != nil: // the hub is stopping: nothing can be sent now
			lost++
		default:
			c := e.call
			h.deliverWith(gateCtx(ctx), c.agent, c.t, c.body, c.meta, c.ttl, c.sentAt, c.msgID)
		}
	}
	finished = true
	if lost > 0 {
		h.failFile(ctx, agent, fmt.Sprintf("The hub was stopped while a file was on its way to %s, so %d message(s) typed after it were not sent. Send them again.", agent, lost))
	}
}

// gateDropAll discards what waits behind files, for a lockdown: nothing that was typed before it may be sent after it. The
// files themselves end on their own (their next part is refused while the hub is locked).
func (h *Hub) gateDropAll(ctx context.Context) {
	d := &h.down
	d.mu.Lock()
	n := 0
	for _, g := range d.gates {
		kept := g.q[:0]
		for _, e := range g.q {
			switch {
			case e.reservation && !e.ready(): // a transfer still on its way keeps its place, to give it up itself
				kept = append(kept, e)
			case !e.skip:
				n++
			}
		}
		g.q = kept
	}
	d.mu.Unlock()
	if n > 0 {
		h.say(ctx, fmt.Sprintf("Lockdown: %d message(s) that waited for a file were discarded, not sent.", n))
	}
}
