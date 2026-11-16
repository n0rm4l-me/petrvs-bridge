package agentsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The stuck-request signal: a text from the chat starts a clock, the assistant's reply stops it. When the clock runs out
// the chat is told, at most three times, unless the assistant is waiting for the user's answer to a prompt.

const (
	stuckPoll      = 30 * time.Second
	stuckMaxAlerts = 3
	previewChars   = 60
)

// stuckReq is the request that has not been answered yet.
type stuckReq struct {
	seq     uint64 // tells one request from the next while the screen is being read
	at      time.Time
	preview string
	// waiting is set while a prompt waits for the user; the clock does not run then.
	waiting bool
	waitKey string // the prompt notice that paused this request, so that a resend of it does not pause it again
	alerts  int
	last    time.Time // when the last alert was queued
}

// localCommands are the assistant's own commands that do their work without a turn of the assistant, so that no reply
// follows and no clock must start. (A custom command or a skill does run a turn, and is not listed.)
var localCommands = map[string]bool{"clear": true, "compact": true, "resume": true, "help": true, "exit": true, "quit": true,
	"model": true, "config": true, "status": true, "cost": true, "usage": true, "doctor": true, "login": true, "logout": true,
	"mcp": true, "memory": true, "permissions": true, "vim": true, "agents": true, "hooks": true, "ide": true, "context": true,
	"theme": true, "rewind": true, "export": true, "output-style": true, "statusline": true, "bug": true, "feedback": true,
	"add-dir": true, "terminal-setup": true, "release-notes": true, "todos": true, "privacy-settings": true, "upgrade": true,
	"bashes": true, "plugin": true, "fast": true, "copy": true, "stats": true,
	// OpenCode's own commands
	"/": true, "ask": true, "cmd": true, "edit": true, "new": true, "open": true, "run": true, "task": true}

// startsRequest says whether a text typed from the chat asks the assistant for something, so that a reply is due.
func startsRequest(body string) bool {
	t := strings.TrimSpace(body)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "!") { // the assistant's bash mode runs the command itself: no turn of the assistant follows
		return false
	}
	if rest, ok := strings.CutPrefix(t, "/"); ok {
		word, _, _ := strings.Cut(rest, " ")
		return !localCommands[strings.ToLower(word)]
	}
	return true
}

// previewOf is the start of a request as the alert shows it: secrets hidden, one line, cut.
func previewOf(body string) string {
	return screen.Cut(oneLineOf(screen.Redact(body)), previewChars)
}

// waitsForUser says whether a notice from the spool is a prompt that waits for an answer: it carries buttons, and is not
// this agent's own stuck notice (which carries the screen button).
func waitsForUser(it spool.Item) bool {
	if it.Type != wire.TypeNotice || len(it.Buttons) == 0 {
		return false
	}
	for _, b := range it.Buttons {
		if b.Key == wire.ButtonScreen {
			return false
		}
	}
	return true
}

// stuckRequested starts the clock for a text. It is called before the text is typed: the assistant can answer, and the
// Stop hook queue the reply, before the typing call has returned, and a clock started after that would wait for a reply
// that has already gone by. The function it returns takes the request back when the text was not typed; it puts back the
// request that was pending before, unless something has replaced or answered this one meanwhile.
func (a *Agent) stuckRequested(body string) (undo func()) {
	if a.cfg.Ext.StuckAfter <= 0 || !startsRequest(body) {
		return func() {}
	}
	now := a.cfg.Now()
	m := &a.mach
	m.mu.Lock()
	prev := m.req
	m.reqSeq++
	mine := &stuckReq{seq: m.reqSeq, at: now, preview: previewOf(body)}
	m.req = mine
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if m.req == mine {
			m.req = prev
		}
		m.mu.Unlock()
	}
}

// stuckKey follows a key that was pressed: it answers a prompt (the clock runs again, from now), or interrupts (the
// request will not be answered, so the clock is dropped).
func (a *Agent) stuckKey(key string) {
	now := a.cfg.Now()
	m := &a.mach
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.req
	switch {
	case r == nil:
	case key == "Escape": // an interrupt, or a prompt turned down with Esc: no reply follows either way
		m.req = nil
	case r.waiting:
		if answersPrompt(key) {
			r.waiting, r.at, r.alerts, r.last = false, now, 0, time.Time{}
		}
	}
}

// answersPrompt says whether a key decides a prompt (Up, Down and Tab only move inside it).
func answersPrompt(key string) bool {
	return key == "Enter" || key == "Escape" || (len(key) == 1 && key[0] >= '1' && key[0] <= '9')
}

// stuckAnswered stops the clock: the assistant has replied.
func (a *Agent) stuckAnswered() {
	a.mach.mu.Lock()
	a.mach.req = nil
	a.mach.mu.Unlock()
}

// stuckForget drops the request when Claude Code was started again: nothing will answer it.
func (a *Agent) stuckForget() { a.stuckAnswered() }

// stuckWaiting pauses the clock: a prompt is waiting for the user.
func (a *Agent) stuckWaiting(it spool.Item) {
	sum := sha256.Sum256([]byte(it.Body))
	key := hex.EncodeToString(sum[:8])
	m := &a.mach
	m.mu.Lock()
	if r := m.req; r != nil && !r.waiting && r.waitKey != key {
		r.waiting, r.waitKey = true, key
	}
	m.mu.Unlock()
}

// spoolHoldsReply says whether a reply waits in the spool to be sent.
func (a *Agent) spoolHoldsReply() bool {
	sp := a.cfg.Spool
	if sp == nil {
		return false
	}
	names, err := sp.Names()
	if err != nil {
		return false
	}
	for _, n := range names {
		if it, ok, err := sp.Read(n); err == nil && ok && it.Type == wire.TypeReply {
			return true
		}
	}
	return false
}

// stuckTick is one look at the request that waits for a reply.
func (a *Agent) stuckTick(ctx context.Context) {
	after := a.cfg.Ext.StuckAfter
	if after <= 0 {
		return
	}
	m := &a.mach
	if a.Locked() { // nothing can be done about it from the chat while the machine is locked, and it would be old news later
		m.mu.Lock()
		m.req = nil
		m.mu.Unlock()
		return
	}
	now := a.cfg.Now()
	m.mu.Lock()
	r := m.req
	if r == nil || r.waiting || r.alerts >= stuckMaxAlerts {
		m.mu.Unlock()
		return
	}
	ref := r.at
	if r.alerts > 0 {
		ref = r.last
	}
	if now.Sub(ref) < after {
		m.mu.Unlock()
		return
	}
	seq, elapsed, preview := r.seq, now.Sub(r.at), r.preview
	m.mu.Unlock()

	// A reply that is queued but not yet sent has not stopped the clock, and the alert would go out right behind it.
	if a.spoolHoldsReply() {
		return
	}

	state := ""
	if a.cfg.Screen != nil {
		c, cancel := context.WithTimeout(ctx, paneTimeout)
		scr, err := a.cfg.Screen.Capture(c)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if _, ok := screen.ExtractPrompt(scr); ok {
				return // the user has a prompt to answer: that is why there is no reply
			}
			state = a.assistantName() + " looks idle."
			if screen.Busy(scr) {
				state = a.assistantName() + " looks busy."
			}
		case errors.Is(err, screen.ErrNoSession):
			state = "The tmux session is gone."
		default:
			state = "The screen cannot be read."
		}
	}

	m.mu.Lock()
	r = m.req
	if r == nil || r.seq != seq || r.waiting { // answered, or a prompt came, while the screen was being read
		m.mu.Unlock()
		return
	}
	r.alerts++
	r.last = now
	n := r.alerts
	m.mu.Unlock()

	mins := int(elapsed.Round(time.Minute) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	body := fmt.Sprintf("⏳ No reply to «%s» for %d min.", preview, mins)
	if state != "" {
		body += " " + state
	}
	body += " /screen to look, /stop to interrupt."
	// The screen button is also how waitsForUser knows this notice is no prompt.
	buttons := []wire.Button{{Label: "📺 Screen", Key: wire.ButtonScreen}, {Label: "✖ Stop", Key: "Escape"}}
	a.log.Info("a request has had no reply for a long time", "minutes", mins, "alert", n)
	a.queueNotice(ctx, body, buttons)
}
