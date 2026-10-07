package agentsvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/promptcmd"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// What looks at the screen for the user: the check that a key answers the prompt that is shown, the permission mode after
// Shift+Tab, and the snapshot of the screen on request.

type uiState struct {
	mu sync.Mutex
	// lastScreen is when a request for the screen was last answered, in real time.
	lastScreen time.Time
	// modeWait and screenWait are how long the session gets to react before the screen is read after a key and after a
	// text; tests shorten them.
	modeWait, screenWait time.Duration
}

func (u *uiState) init(a *Agent) {
	u.modeWait, u.screenWait = 400*time.Millisecond, 800*time.Millisecond
}

// screenEvery is the least time between two requests for the screen that are answered.
const screenEvery = time.Second

// captureTimeout bounds one read of the screen.
const captureTimeout = 5 * time.Second

// errNoScreen is what reading the screen gives when the agent has no way to.
var errNoScreen = errors.New("agentsvc: no screen")

// allowScreen says whether a request for the screen may be answered now, and counts it if so.
func (u *uiState) allowScreen() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.lastScreen.IsZero() && time.Since(u.lastScreen) < screenEvery {
		return false
	}
	u.lastScreen = time.Now()
	return true
}

// capture reads the screen of the session.
func (a *Agent) capture(ctx context.Context) (string, error) {
	if a.cfg.Screen == nil {
		return "", errNoScreen
	}
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	return a.cfg.Screen.Capture(ctx)
}

// pause waits d, or less when ctx ends; it reports whether the wait was served in full.
func pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// onScreen answers a request for the screen. Like a command it is refused when it has expired, and while the machine is
// locked: what is on the screen is what the lock keeps from the chat.
func (a *Agent) onScreen(ctx context.Context, p *wire.Payload) {
	var why string
	switch {
	case p.Expired(a.cfg.Now(), a.cfg.SkewAllowance):
		a.expired.Add(1)
		why = "expired before it could run, not executed"
	case a.Locked():
		a.refused.Add(1)
		why = "refused: this machine is locked"
	case !a.ui.allowScreen():
		why = "the screen was just sent; ask again in a second"
	default:
		a.sendSnapshot(ctx)
		return
	}
	a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: why})
}

// sendSnapshot sends what the screen shows: the prompt with its buttons, or the bottom lines with buttons to move. It is
// sent directly, not spooled: a snapshot that arrives late is of no use.
func (a *Agent) sendSnapshot(ctx context.Context) {
	scr, err := a.capture(ctx)
	if err != nil {
		why := "the screen cannot be read"
		if errors.Is(err, screen.ErrNoSession) {
			why += ": the tmux session is not running"
		}
		a.send(ctx, &wire.Payload{Type: wire.TypeDropped, Body: why})
		return
	}
	n := promptcmd.Snapshot(a.cfg.Identity.Name, scr)
	if err := a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: n.Body, Buttons: n.Buttons}); err != nil {
		a.log.Debug("could not send the screen", "err", err)
		return
	}
	a.log.Debug("screen sent", "bytes", len(n.Body), "buttons", len(n.Buttons))
}

// errStalePrompt is what the user is told when a key for an earlier prompt is refused.
const errStalePrompt = "that prompt is no longer on the screen, so the key was not pressed (use /screen)"

// checkPrompt is called before a key is pressed. A key that answers a prompt carries MetaPrompt, the hash of that prompt;
// the key is pressed only if the same prompt is on the screen now. It returns a *userError to refuse.
//
// The hash is of the dialog as drawn, so an old notice cannot answer a new prompt, not even one that differs only in the
// command, while moving the highlight does not change it. Only Escape and the digits of the prompt's own choices are
// accepted: Enter would take whatever is highlighted at that moment.
func (a *Agent) checkPrompt(ctx context.Context, p *wire.Payload) error {
	want := p.Meta[wire.MetaPrompt]
	if want == "" {
		return a.checkNoPrompt(ctx, p)
	}
	scr, err := a.capture(ctx)
	if err != nil {
		if errors.Is(err, screen.ErrNoSession) {
			return &userError{errStalePrompt}
		}
		return &userError{"the screen cannot be read, so the key for that prompt was not pressed (use /screen)"}
	}
	pr, ok := screen.ExtractPrompt(scr)
	if !ok || pr.Hash != want || !answers(pr, p.Body) {
		// Why, without the screen itself: a keyed hash is not the text, and the counts say whether the pane was too small to
		// hold the dialog (a prompt that is cut off is not found, or has another hash from one look to the next).
		a.log.Warn("refused a key for a prompt: it is not the one on the screen",
			"key", p.Body, "prompt_found", ok, "same_prompt", ok && pr.Hash == want, "answers_it", ok && answers(pr, p.Body),
			"wanted", want, "on_screen", pr.Hash, "choices", len(pr.Options), "screen_lines", strings.Count(scr, "\n")+1)
		return &userError{errStalePrompt}
	}
	return nil
}

// checkNoPrompt covers a key from a button under a snapshot of the screen that showed no prompt (the hub marks it with
// MetaEcho "screen"). Such a button is tied to no prompt, so Enter or a number from an old snapshot would answer a
// prompt that has appeared since and that the user has not seen: they are pressed only while no prompt is on the screen.
// Up, Down, Tab and Escape cannot approve anything and go through, as do keys typed in the chat (no MetaEcho).
func (a *Agent) checkNoPrompt(ctx context.Context, p *wire.Payload) error {
	answersAPrompt := p.Body == "Enter" || (len(p.Body) == 1 && p.Body[0] >= '1' && p.Body[0] <= '9')
	if p.Meta[wire.MetaEcho] != "screen" || !answersAPrompt {
		return nil
	}
	scr, err := a.capture(ctx)
	if err != nil {
		if errors.Is(err, screen.ErrNoSession) {
			return nil // there is nothing to answer, and the key goes nowhere
		}
		return &userError{"the screen cannot be read, so the key was not pressed (use /screen)"}
	}
	if _, ok := screen.ExtractPrompt(scr); ok {
		return &userError{"a prompt is on the screen now that was not there when that screen was shown, so the key was not pressed (use /screen)"}
	}
	return nil
}

// answers says whether key is a way to answer the prompt: Escape, or the number of one of its choices.
func answers(p screen.Prompt, key string) bool {
	if key == "Escape" {
		return true
	}
	for _, o := range p.Options {
		if o.Key == key {
			return true
		}
	}
	return false
}

// afterKey reports the permission mode when the key asked for it (MetaEcho "mode"), or the screen when a button that moves
// in a list was pressed (MetaEcho "screen": its buttons are gone, the new ones are on the new snapshot).
func (a *Agent) afterKey(ctx context.Context, p *wire.Payload) {
	switch p.Meta[wire.MetaEcho] {
	case "mode":
		if pause(ctx, a.ui.modeWait) && !a.Locked() {
			a.reportMode(ctx)
		}
	case "screen":
		if pause(ctx, a.ui.modeWait) && !a.Locked() {
			a.sendSnapshot(ctx)
		}
	}
}

// afterText sends a snapshot of the screen when the text asked for it (MetaEcho "screen"), after giving the session a
// moment to react (for /resume, the list of sessions).
func (a *Agent) afterText(ctx context.Context, p *wire.Payload) {
	if p.Meta[wire.MetaEcho] == "screen" && pause(ctx, a.ui.screenWait) && !a.Locked() {
		a.sendSnapshot(ctx)
	}
}

// reportMode tells the permission mode the screen shows, or that it shows none (and then its last line, which may say why).
func (a *Agent) reportMode(ctx context.Context) {
	body := "Permission mode: not shown"
	if scr, err := a.capture(ctx); err != nil {
		body += " (the screen cannot be read)"
	} else if mode := screen.Mode(scr); mode != "" {
		body = "Permission mode: " + mode
	} else if last := screen.BottomLines(scr, 1); last != "" {
		body += "\n" + screen.Cut(last, 200)
	}
	a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: body})
}
