package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"sync"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Files from the chat to the machine: the parts arrive unordered, the closing `file` command is run in order with the
// text and keys, assembles the file, saves it in the inbox and tells the session where it is.

const (
	// fileWait is how long the closing command waits for parts that have not arrived; a part is stored before the hub
	// hears of it, so this only matters when the bus is slow or the parts were lost.
	fileWait = 10 * time.Second
	filePoll = 100 * time.Millisecond
	// idleTransfer is how long the parts of a transfer may lie unused before a new transfer makes room by dropping them.
	// The assembler holds only a few transfers, and the hub gives up after seconds, so a stale one is an abandoned one.
	idleTransfer = 2 * time.Minute
	// maxRefused bounds the transfers remembered as refused.
	maxRefused = 16
)

type downState struct {
	asm *filetx.Assembler

	mu         sync.Mutex
	seen       map[string]time.Time // transfers that hold parts, by key, and when the last part came
	refused    map[string]struct{}  // transfers refused and reported, so that the rest of their parts are dropped quietly
	refusedQ   []string
	lastTidy   time.Time
	saves      int // files saved since the inbox was last tidied
	wait, poll time.Duration
}

func (d *downState) init(a *Agent) {
	d.asm = filetx.NewAssembler(a.cfg.Ext.MaxFileBytes)
	d.seen = map[string]time.Time{}
	d.refused = map[string]struct{}{}
	d.wait, d.poll = fileWait, filePoll
}

// transferKey names a transfer in the assembler. The hub's transfers are kept apart from any other user of the type.
func transferKey(t string) string { return "in/" + t }

// validTransfer accepts the short token the hub chose: it ends up in a file name, so nothing but letters and digits.
func validTransfer(t string) bool {
	if len(t) < 6 || len(t) > 24 {
		return false
	}
	for _, r := range t {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// refuse drops a transfer and says why, once: the parts that are still on their way would otherwise each repeat it.
func (a *Agent) refuse(ctx context.Context, key, why string) {
	d := &a.down
	d.asm.Forget(key)
	d.mu.Lock()
	delete(d.seen, key)
	_, said := d.refused[key]
	if !said {
		d.refused[key] = struct{}{}
		d.refusedQ = append(d.refusedQ, key)
		for len(d.refusedQ) > maxRefused {
			delete(d.refused, d.refusedQ[0])
			d.refusedQ = d.refusedQ[1:]
		}
	}
	d.mu.Unlock()
	if said {
		return
	}
	a.refused.Add(1)
	a.log.Warn("refused a file from the chat", "why", why)
	a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: "refused the file: " + why})
}

func (d *downState) wasRefused(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.refused[key]
	return ok
}

// onPart receives one part of a file that the hub sends to the session. It answers each part it has stored with a `done`
// that names the part, which is what lets the hub send the next one; a part it did not store is not answered.
func (a *Agent) onPart(ctx context.Context, p *wire.Payload) {
	t := p.Meta[wire.MetaTransfer]
	i, err := strconv.Atoi(p.Meta[wire.MetaIndex])
	if !validTransfer(t) || err != nil {
		a.rejected.Add(1)
		a.log.Debug("dropped a part without a valid transfer or index", "id", p.ID)
		return
	}
	key := transferKey(t)
	d := &a.down
	switch {
	case a.cfg.Ext.InboxDir == "":
		a.refuse(ctx, key, "this machine takes no files (no inbox is set)")
		return
	case a.Locked():
		a.refuse(ctx, key, "this machine is locked")
		return
	case d.wasRefused(key):
		return
	}
	data, err := filetx.DecodePart(p.Body)
	if err != nil {
		a.refuse(ctx, key, "a part was damaged")
		return
	}
	now := a.cfg.Now()
	d.mu.Lock()
	if _, known := d.seen[key]; !known {
		for k, at := range d.seen {
			if now.Sub(at) > idleTransfer {
				d.asm.Forget(k)
				delete(d.seen, k)
			}
		}
	}
	d.mu.Unlock()
	if err := d.asm.AddPart(key, i, data, now); err != nil {
		why := "the file could not be received"
		if errors.Is(err, filetx.ErrTooLarge) {
			why = "the file is larger than this machine takes"
		}
		a.refuse(ctx, key, why)
		return
	}
	d.mu.Lock()
	d.seen[key] = now
	d.mu.Unlock()
	a.send(ctx, &wire.Payload{Type: wire.TypeDone, Body: p.ID})
}

// announced is what the closing command says about the transfer.
type announced struct {
	transfer string
	parts    int
	size     int64
	sha      string
	name     string
	photo    bool
}

func parseAnnounced(meta map[string]string) (announced, bool) {
	var n announced
	var err error
	n.transfer = meta[wire.MetaTransfer]
	if n.parts, err = strconv.Atoi(meta[wire.MetaParts]); err != nil || n.parts <= 0 {
		return n, false
	}
	if n.size, err = strconv.ParseInt(meta[wire.MetaSize], 10, 64); err != nil || n.size <= 0 {
		return n, false
	}
	n.sha = meta[wire.MetaSHA]
	n.name = filetx.SafeName(meta[wire.MetaName])
	n.photo = meta[wire.MetaKind] == "photo"
	return n, validTransfer(n.transfer) && len(n.sha) == 64
}

// executeFile is the command that ends a transfer from the chat: assemble, save, tell the session.
func (a *Agent) executeFile(ctx context.Context, p *wire.Payload) error {
	if a.cfg.Ext.InboxDir == "" {
		return &userError{"refused: this machine takes no files (no inbox is set)"}
	}
	n, ok := parseAnnounced(p.Meta)
	if !ok {
		return &userError{"refused: the description of the file is not valid"}
	}
	data, err := a.collect(ctx, n)
	if err != nil {
		return err
	}
	if a.Locked() { // a lockdown may have come while the parts were being waited for
		return &userError{"refused: this machine is locked"}
	}
	path, err := saveInbox(a.cfg.Ext.InboxDir, n.name, n.transfer, data, a.cfg.Now())
	if err != nil {
		a.log.Warn("could not save a file from the chat", "id", p.ID, "why", withoutPath(err))
		var ue *userError
		if errors.As(err, &ue) {
			return ue
		}
		return &userError{"could not save the file in the inbox"}
	}
	a.log.Info("saved a file from the chat", "id", p.ID, "kind", kindWord(n.photo), "bytes", len(data))
	if err := a.checkFileLine(ctx); err != nil { // the same guard as for a text: no shell, no dialog
		a.tidyIfDue()
		return err
	}
	typeErr := a.cfg.Exec.SendText(ctx, fileLine(n.photo, path, n.name, len(data), p.Body))
	a.tidyIfDue()
	if typeErr != nil {
		return &userError{"the file was saved in the inbox, but the session could not be told about it"}
	}
	return nil
}

// tidyIfDue tidies the inbox when an hour has passed since the last time or many files have come since.
func (a *Agent) tidyIfDue() {
	d := &a.down
	now := a.cfg.Now()
	d.mu.Lock()
	d.saves++
	due := d.saves >= tidyAfterSaves || now.Sub(d.lastTidy) >= tidyEvery
	if due {
		d.saves, d.lastTidy = 0, now
	}
	d.mu.Unlock()
	if due {
		if err := tidyInbox(a.cfg.Ext.InboxDir, now); err != nil {
			a.log.Debug("could not tidy the inbox", "why", withoutPath(err))
		}
	}
}

// withoutPath is an error's reason without the file it names, since a log line has no file names in it.
func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// collect puts the parts together. Parts that are missing are waited for a few seconds, since the bus may deliver them
// after the closing command; the transfer is forgotten in every case that does not end in a file.
func (a *Agent) collect(ctx context.Context, n announced) ([]byte, error) {
	d := &a.down
	key := transferKey(n.transfer)
	defer func() {
		d.mu.Lock()
		delete(d.seen, key)
		d.mu.Unlock()
	}()
	if d.wasRefused(key) {
		return nil, &userError{"the file was not accepted (see the message before this one)"}
	}
	deadline := time.Now().Add(d.wait)
	for {
		data, err := d.asm.Complete(key, n.parts, n.size, n.sha, a.cfg.Now())
		if err == nil {
			return data, nil
		}
		var miss *filetx.MissingError
		if errors.As(err, &miss) || errors.Is(err, filetx.ErrUnknown) {
			if time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(d.poll):
				}
				continue
			}
			d.asm.Forget(key)
			if miss != nil {
				return nil, &userError{fmt.Sprintf("the file did not arrive whole (%d of %d parts); send it again", miss.Have, miss.Want)}
			}
			return nil, &userError{"the file did not arrive whole (no part of it came); send it again"}
		}
		switch {
		case errors.Is(err, filetx.ErrTooLarge):
			return nil, &userError{"the file is larger than this machine takes"}
		case errors.Is(err, filetx.ErrSize), errors.Is(err, filetx.ErrHash):
			return nil, &userError{"the file was damaged on the way (it does not match its checksum); send it again"}
		}
		return nil, &userError{"the file could not be put together"}
	}
}
