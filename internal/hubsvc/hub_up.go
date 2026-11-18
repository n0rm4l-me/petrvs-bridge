package hubsvc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Files from a machine to the chat: the parts and the closing item arrive from the spool of the agent, are put together
// and uploaded to the chat.
//
// Everything in a part or a closing item is the agent's word, and an agent may be confused or taken over: the ids, the
// counts, the sizes and the names are checked before they are used, the memory a transfer may hold is bounded (by the
// Assembler, and here by the number of transfers, of finished files waiting for Telegram and of the records kept), and a
// transfer that cannot finish is given up with a word in the chat instead of being resent for ever.

const (
	upLiveMax      = 16               // transfers with parts held at once; the Assembler holds as many (an agent sends the parts of every queued file before any closing item)
	upRecordsMax   = 64               // transfers remembered, dead and finished ones included
	upRecordTTL    = 15 * time.Minute // a record with no part or closing item for this long is forgotten
	upReadyMax     = 2                // finished files kept for a retry of the upload
	upReadyTTL     = 10 * time.Minute // how long a finished file waits for Telegram
	upMissGiveUp   = 3                // closing items that found parts missing before the hub says so
	upMissWindow   = 2 * time.Minute  // or this long after the first one
	upFailGiveUp   = 5                // uploads that Telegram did not take (and did not refuse) before the hub says so
	upFailWindow   = 5 * time.Minute  // or this long after the first one (less than upReadyTTL, or the copy expires first)
	upDoneMax      = 128              // finished transfers remembered, so that a second closing item for one is ignored
	upCaptionBytes = 1000             // what is taken of the caption; the client cuts it to 1024 characters with the tag
	upIndexLen     = 7                // digits of a part index
	upTransferLen  = 32               // bytes of a transfer id
)

type upState struct {
	// sem holds one slot: one file is put together and uploaded at a time, so that at most one finished file is being
	// written out and the others wait as parts, which the Assembler bounds.
	sem chan struct{}

	mu       sync.Mutex // guards the fields below; never held while talking to Telegram or the bus
	max      int64
	asm      *filetx.Assembler
	records  map[string]*upRecord
	ready    map[string]*upReady
	readyN   uint64        // generation of the last entry in ready
	readyTTL time.Duration // how long a finished file is kept for the retry of the upload (upReadyTTL; a test shortens it)
	done     map[string]struct{}
	doneLog  []string
}

// upRecord is what the hub knows about one transfer besides the parts the Assembler holds.
type upRecord struct {
	seen      time.Time
	got       bool   // at least one part was taken
	dead      string // why the transfer is dropped ("" while it lives); later parts are confirmed and ignored
	misses    int    // closing items that found parts missing
	firstMiss time.Time
	fails     int // uploads Telegram did not take
	firstFail time.Time
	// verdict is what the chat is to be told about a transfer that is given up, kept until the chat has heard it: the item
	// is confirmed only then, and the resend that follows a failure to tell must say the same thing without redoing the work.
	verdict string
}

// upReady is a finished file that Telegram has not taken yet.
type upReady struct {
	data  []byte
	at    time.Time
	gen   uint64
	timer *time.Timer // frees the bytes after upReadyTTL; stopped when the entry goes earlier
}

const (
	upDeadTooLarge = "too large"
)

func (u *upState) init(h *Hub) {
	u.max = h.cfg.MaxFileBytes
	if u.max <= 0 {
		u.max = filetx.DefaultMaxBytes
	}
	u.sem = make(chan struct{}, 1)
	u.asm = filetx.NewAssembler(u.max)
	u.records = map[string]*upRecord{}
	u.ready = map[string]*upReady{}
	u.readyTTL = upReadyTTL
	u.done = map[string]struct{}{}
}

// upAssemblerID is the key the Assembler holds a transfer under: the agent and the transfer, cut to the length it takes.
func upAssemblerID(key string) string {
	if len(key) <= upTransferLen {
		return key
	}
	return filetx.SHA([]byte(key))[:upTransferLen]
}

// upValidTransfer accepts what filetx.NewID makes and a little more, and nothing that could be a path or a control byte.
func upValidTransfer(t string) bool {
	if t == "" || len(t) > upTransferLen {
		return false
	}
	for _, r := range t {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// digits reads a decimal number of at most n digits, with no sign and no space.
func upDigits(s string, n int) (int64, bool) {
	if s == "" || len(s) > n {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func upValidSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// upFileMeta is the description in a closing item, once it has been checked.
type upFileMeta struct {
	t     string
	parts int
	size  int64
	sha   string
	name  string // made safe
	kind  string
}

// upParseMeta checks the Meta of a closing item against what the hub takes. problem is empty when the description is
// usable; otherwise it says what the chat is told. A size above the limit is reported with the usable fields filled in.
func upParseMeta(m map[string]string, max int64) (fm upFileMeta, problem string) {
	fm.t = m[wire.MetaTransfer]
	fm.name = filetx.SafeName(m[wire.MetaName])
	if !upValidTransfer(fm.t) {
		return fm, "damaged"
	}
	parts, okParts := upDigits(m[wire.MetaParts], upIndexLen)
	size, okSize := upDigits(m[wire.MetaSize], 12)
	if !okParts || !okSize || parts < 1 || size < 1 || !upValidSHA(m[wire.MetaSHA]) {
		return fm, "damaged"
	}
	if size > max {
		return fm, "large"
	}
	// A file is cut in pieces of wire.MaxChunk, none empty: any other count is not a transfer of this program.
	if parts > size || parts*wire.MaxChunk < size || parts > int64(filetx.MaxParts(max)) {
		return fm, "damaged"
	}
	fm.parts, fm.size, fm.sha = int(parts), size, strings.ToLower(m[wire.MetaSHA])
	fm.kind = "document"
	switch m[wire.MetaKind] {
	case "photo":
		fm.kind = "photo"
	case "voice":
		fm.kind = "voice"
	}
	return fm, ""
}

// sweep forgets what is old; the caller holds u.mu.
func (u *upState) sweep(now time.Time) {
	for key, r := range u.records {
		if now.Sub(r.seen) > upRecordTTL {
			u.asm.Forget(upAssemblerID(key))
			delete(u.records, key)
		}
	}
	for key, f := range u.ready {
		if now.Sub(f.at) > upReadyTTL {
			u.dropReady(key)
		}
	}
}

// dropReady forgets a finished file and the timer that would free it; the caller holds u.mu.
func (u *upState) dropReady(key string) {
	if f := u.ready[key]; f != nil {
		if f.timer != nil {
			f.timer.Stop()
		}
		delete(u.ready, key)
	}
}

// live counts the transfers whose parts the Assembler holds; the caller holds u.mu.
func (u *upState) live() int {
	n := 0
	for _, r := range u.records {
		if r.got && r.dead == "" {
			n++
		}
	}
	return n
}

// finish records that a transfer is over, successfully or not, and drops what is kept for it; the caller holds u.mu.
func (u *upState) finish(key string) {
	u.asm.Forget(upAssemblerID(key))
	delete(u.records, key)
	u.dropReady(key)
	if _, ok := u.done[key]; !ok {
		u.done[key] = struct{}{}
		u.doneLog = append(u.doneLog, key)
		if len(u.doneLog) > upDoneMax {
			delete(u.done, u.doneLog[0])
			u.doneLog = u.doneLog[1:]
		}
	}
}

// onFileItem receives a part or the closing item of a file from an agent. The hub has claimed the id (it is in
// st.inflight); the body must release it, remember it and send the receipt when the item is dealt with.
func (h *Hub) onFileItem(ctx context.Context, name string, st *agentState, s *sess, p *wire.Payload) {
	finished := false
	defer func() { // every way out, a panic included, releases the claim; only a finished item is remembered and confirmed
		h.mu.Lock()
		delete(st.inflight, p.ID)
		if finished {
			h.remember(st, p.ID)
		}
		h.mu.Unlock()
		if finished {
			h.receipt(ctx, name, s, p.ID)
		}
	}()
	switch p.Type {
	case wire.TypePart:
		finished = h.upPart(name, p)
	case wire.TypeFile:
		finished = h.upFile(ctx, name, p)
	}
}

// upPart stores one part. It returns true when the part is dealt with (also when it was not usable: the agent must not
// send it for ever) and false when the hub cannot take it now, which leaves it to the agent's resend.
func (h *Hub) upPart(name string, p *wire.Payload) bool {
	u := &h.up
	t := p.Meta[wire.MetaTransfer]
	index, okIndex := upDigits(p.Meta[wire.MetaIndex], upIndexLen)
	data, err := filetx.DecodePart(p.Body)
	if !upValidTransfer(t) || !okIndex || err != nil || len(data) == 0 {
		h.log.Warn("dropping a file part that cannot be used", "agent", name, "id", p.ID, "bytes", len(p.Body))
		return true
	}
	key := name + "/" + t
	now := h.cfg.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	u.sweep(now)
	if _, done := u.done[key]; done {
		return true // a straggler of a transfer that is over
	}
	r := u.records[key]
	if r != nil && r.dead != "" {
		r.seen = now
		return true
	}
	// A transfer that holds no part yet takes one of the places of the Assembler with its first part, whether or not a
	// record of it exists: a closing item that came first makes the record, and must not let the transfer past the limit.
	if (r == nil && len(u.records) >= upRecordsMax) || ((r == nil || !r.got) && u.live() >= upLiveMax) {
		h.log.Warn("too many file transfers at once, a part waits for its resend", "agent", name, "id", p.ID)
		return false
	}
	created := r == nil
	if created {
		r = &upRecord{}
		u.records[key] = r
	}
	r.seen = now
	switch err := u.asm.AddPart(upAssemblerID(key), int(index), data, now); {
	case err == nil:
		r.got = true
	case errors.Is(err, filetx.ErrTooLarge):
		u.asm.Forget(upAssemblerID(key))
		r.dead, r.got = upDeadTooLarge, false
		h.log.Warn("dropping a file transfer that grows past the limit", "agent", name, "id", p.ID, "index", index, "bytes", len(data))
	case errors.Is(err, filetx.ErrBadPart):
		if created { // a record made for a part that was no good is of no use
			delete(u.records, key)
		}
		h.log.Warn("dropping a file part the assembler refuses", "agent", name, "id", p.ID, "index", index, "bytes", len(data))
	default:
		// Not the part's fault (the Assembler is full): confirming it would throw it away, and the file could never be
		// whole. The agent sends it again.
		if created {
			delete(u.records, key)
		}
		h.log.Warn("the assembler cannot take a file part now, it waits for its resend", "agent", name, "id", p.ID, "index", index, "err", err)
		return false
	}
	return true
}

// upTell says something about an item in the chat and reports whether the item may be confirmed: it may when the chat has
// heard it, or refuses it for good (400). When the chat cannot take text now (Telegram is down, a rate limit) the answer is
// no, the agent gets no receipt and sends the item again, and the next try tells the chat again.
func (h *Hub) upTell(ctx context.Context, name, text string) bool {
	err := h.sayAs(ctx, name, text)
	return err == nil || refused(err)
}

// upDrop ends a transfer for good: the chat is told first, the transfer is forgotten after, and only then is the item
// confirmed. The other order lost files without a word when the chat was down for a while: the agent deleted the file on
// the receipt, and nobody knew. The verdict is kept in the record, so that the resend after a failure to tell says the same
// thing without doing the work again (the parts are gone, the upload was refused).
func (h *Hub) upDrop(ctx context.Context, name, key, text string) bool {
	u := &h.up
	u.mu.Lock()
	if r := u.records[key]; r != nil {
		r.verdict = text
	}
	u.mu.Unlock()
	if !h.upTell(ctx, name, text) {
		return false
	}
	u.mu.Lock()
	u.finish(key)
	u.mu.Unlock()
	return true
}

// upFile puts a file together and uploads it. It returns true when the item is dealt with and false when the agent
// should send it again.
func (h *Hub) upFile(ctx context.Context, name string, p *wire.Payload) bool {
	u := &h.up
	fm, problem := upParseMeta(p.Meta, u.max)
	switch problem {
	case "damaged":
		h.log.Warn("dropping a file item with a damaged description", "agent", name, "id", p.ID)
		return h.upTell(ctx, name, fmt.Sprintf("⚠️ %s: a file arrived with a damaged description and was dropped", name))
	case "large":
		h.log.Warn("dropping a file that is larger than the limit", "agent", name, "id", p.ID)
		return h.upDrop(ctx, name, name+"/"+fm.t, fmt.Sprintf("⚠️ %s: the file %s is larger than this hub takes (%d MiB at most); it was not sent", name, fm.name, u.max>>20))
	}
	key := name + "/" + fm.t
	aid := upAssemblerID(key)
	sender, canUpload := h.cfg.Telegram.(telegram.FileSender)
	now := h.cfg.Now()

	u.mu.Lock()
	u.sweep(now)
	if _, done := u.done[key]; done {
		u.mu.Unlock()
		return true // a second closing item for a file that is already in the chat
	}
	r := u.records[key]
	if r == nil {
		if len(u.records) >= upRecordsMax {
			u.mu.Unlock()
			return false
		}
		r = &upRecord{}
		u.records[key] = r
	}
	r.seen = now
	dead, verdict := r.dead, r.verdict
	rf := u.ready[key]
	full := rf == nil && len(u.ready) >= upReadyMax
	u.mu.Unlock()

	switch {
	case verdict != "": // decided on an earlier try, which the chat did not hear
		return h.upDrop(ctx, name, key, verdict)
	case dead != "":
		return h.upDrop(ctx, name, key, fmt.Sprintf("⚠️ %s: the file %s could not be taken (too large for this hub, or the hub was busy with other files); send it again", name, fm.name))
	case !canUpload:
		return h.upDrop(ctx, name, key, fmt.Sprintf("⚠️ %s: this hub cannot upload files, so %s was dropped", name, fm.name))
	case full:
		return false // two finished files wait for Telegram already; this one waits as parts
	}

	select {
	case u.sem <- struct{}{}:
		defer func() { <-u.sem }()
	case <-ctx.Done():
		return false
	}
	// Looked at again now that this item has its turn: other items ran while it waited for the slot, and each may have
	// left a finished file behind (an upload that failed), so the room that was there a moment ago may be gone.
	u.mu.Lock()
	_, done := u.done[key] // another closing item for the same transfer may have been served while this one waited
	rf = u.ready[key]
	full = rf == nil && len(u.ready) >= upReadyMax
	u.mu.Unlock()
	if done {
		return true
	}
	if full {
		return false
	}

	var data []byte
	if rf != nil {
		data = rf.data
	} else {
		var err error
		data, err = u.asm.Complete(aid, fm.parts, fm.size, fm.sha, now)
		var missing *filetx.MissingError
		switch {
		case err == nil:
			u.keepReady(key, data, now)
		case errors.As(err, &missing):
			return h.upMissing(ctx, name, key, fm, missing.Have)
		case errors.Is(err, filetx.ErrUnknown):
			return h.upMissing(ctx, name, key, fm, 0)
		default: // the wrong size, the wrong hash, too large: the Assembler has forgotten the transfer
			h.log.Warn("dropping a file that does not match its description", "agent", name, "id", p.ID, "err", err)
			return h.upDrop(ctx, name, key, fmt.Sprintf("⚠️ %s: the file %s arrived damaged (%s); send it again", name, fm.name, upDamage(err)))
		}
	}
	return h.upUpload(ctx, name, key, fm, p.Body, data, sender)
}

// keepReady holds a finished file until Telegram has taken it, for a while at most.
func (u *upState) keepReady(key string, data []byte, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.dropReady(key)
	u.readyN++
	gen := u.readyN
	f := &upReady{data: data, at: now, gen: gen}
	u.ready[key] = f
	// The timer must not hold the bytes: it is not stopped by every way out of here, and a closure over f would keep each
	// finished file in memory for the whole TTL. It names the entry by its generation and looks it up when it fires.
	f.timer = time.AfterFunc(u.readyTTL, func() { // memory is freed even if no other file comes to sweep it
		u.mu.Lock()
		defer u.mu.Unlock()
		if cur := u.ready[key]; cur != nil && cur.gen == gen {
			delete(u.ready, key)
		}
	})
}

// damage says in words what Complete found wrong.
func upDamage(err error) string {
	switch {
	case errors.Is(err, filetx.ErrHash):
		return "the checksum does not match"
	case errors.Is(err, filetx.ErrSize):
		return "the size does not match"
	}
	return "it is larger than this hub takes"
}

// upMissing handles a closing item that came before all its parts (or after they were forgotten). The agent resends
// the item after its receipt timeout, which gives late parts time; after a few tries the hub says so and gives up.
func (h *Hub) upMissing(ctx context.Context, name, key string, fm upFileMeta, have int) bool {
	u := &h.up
	now := h.cfg.Now()
	u.mu.Lock()
	r := u.records[key]
	if r == nil { // swept while this item waited for its turn
		u.mu.Unlock()
		return false
	}
	failed := r.fails > 0 // the file was complete once, the upload failed and the copy kept for the retry has expired
	r.misses++
	if r.misses == 1 {
		r.firstMiss = now
	}
	giveUp := failed || r.misses >= upMissGiveUp || now.Sub(r.firstMiss) >= upMissWindow
	u.mu.Unlock()
	if !giveUp {
		return false
	}
	h.log.Warn("giving up on a file that did not arrive whole", "agent", name, "have", have, "want", fm.parts)
	if failed {
		return h.upDrop(ctx, name, key, fmt.Sprintf("⚠️ %s: the file %s could not be uploaded to the chat in time; send it again", name, fm.name))
	}
	return h.upDrop(ctx, name, key, fmt.Sprintf("⚠️ %s: the file %s did not arrive whole (%d of %d parts); send it again", name, fm.name, have, fm.parts))
}

// upload gives a finished file to Telegram. It returns true when the file is dealt with: in the chat, or refused for good.
func (h *Hub) upUpload(ctx context.Context, name, key string, fm upFileMeta, caption string, data []byte, sender telegram.FileSender) bool {
	u := &h.up
	// What the file is shown as is decided by the hub from the name and size itself, not taken from the agent.
	kind := "document"
	switch {
	case fm.kind == "voice" && filetx.IsVoice(fm.name):
		kind = "voice"
	case fm.kind == "photo" && filetx.Kind(fm.name, int64(len(data))) == "photo":
		kind = "photo"
	}
	ids, err := sender.SendFile(ctx, h.cfg.ChatID, telegram.OutFile{Name: fm.name, Data: data, Caption: h.tag(name) + upCaption(caption, fm.name), Photo: kind == "photo", Voice: kind == "voice"})
	h.track(name, ids)
	if err == nil {
		h.log.Info("uploaded a file to the chat", "agent", name, "kind", kind, "bytes", len(data))
		u.mu.Lock()
		u.finish(key)
		u.mu.Unlock()
		return true
	}
	if ctx.Err() != nil {
		return false // the hub is stopping
	}
	var ae *telegram.APIError
	if errors.As(err, &ae) && (ae.Code == 400 || ae.Code == 413) {
		// Telegram will refuse the same file again; resending it for ever would only repeat the attempt.
		h.log.Warn("the chat refuses a file, dropping it", "agent", name, "kind", kind, "bytes", len(data), "err", err)
		return h.upDrop(ctx, name, key, fmt.Sprintf("⚠️ %s: %s refused the file %s (%s); it was not sent", name, h.cfg.ChatName, fm.name, upShort(ae.Desc, 200)))
	}
	// A rate limit, a server error, no answer: the file is kept for the resend of the closing item, a few times.
	now := h.cfg.Now()
	u.mu.Lock()
	r := u.records[key]
	giveUp := false
	if r != nil {
		r.fails++
		if r.fails == 1 {
			r.firstFail = now
		}
		giveUp = r.fails >= upFailGiveUp || now.Sub(r.firstFail) >= upFailWindow
	}
	u.mu.Unlock()
	if !giveUp {
		h.log.Warn("the chat did not take a file, the agent will send it again", "agent", name, "kind", kind, "bytes", len(data), "err", err)
		return false
	}
	h.log.Warn("giving up on uploading a file", "agent", name, "kind", kind, "bytes", len(data), "err", err)
	// No verdict is kept here: the file is still in `ready`, so when the chat cannot take the words either, the next resend
	// tries the upload again (which may well work by then) before it gives up again.
	if !h.upTell(ctx, name, fmt.Sprintf("⚠️ %s: %s did not take the file %s after several tries; send it again", name, h.cfg.ChatName, fm.name)) {
		return false
	}
	u.mu.Lock()
	u.finish(key)
	u.mu.Unlock()
	return true
}

// upCaption is what the chat shows under a file: the caption, or the file's name when there is none.
func upCaption(caption, name string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return name
	}
	if len(caption) > upCaptionBytes {
		cut := upCaptionBytes
		for cut > 0 && !utf8.RuneStart(caption[cut]) {
			cut--
		}
		caption = caption[:cut]
	}
	return caption
}

// short cuts s to at most n bytes without splitting a character.
func upShort(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
