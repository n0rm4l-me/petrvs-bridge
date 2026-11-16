package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/sysinfo"
)

// Auto-heal: when the assistant has stopped, or the tmux session it ran in is gone, the agent starts it again, but not in
// a loop, not while the owner may be restarting it by hand, and not on a machine that is locked.

const (
	defaultAssistantCommand = "claude --continue"
	defaultHealTries        = 3
	defaultHealWindow       = 10 * time.Minute

	healPoll = 5 * time.Second
	minPoll  = 20 * time.Millisecond
	// startTimeout bounds one start: creating a session, or typing the command into a shell.
	startTimeout = 15 * time.Second
)

// paneTimeout bounds one question to tmux; a variable so that a test need not wait for it.
var paneTimeout = 3 * time.Second

// pollEvery is how often background work looks: every max, but at least four times within span, so that a short grace
// period is not overshot by a long poll.
func pollEvery(max, span time.Duration) time.Duration {
	if span <= 0 || span/4 >= max {
		return max
	}
	if e := span / 4; e > minPoll {
		return e
	}
	return minPoll
}

// isShell says whether prog, as tmux names the program in the foreground of the pane, shows that the pane has been left
// at a prompt. Anything else counts as the assistant: it appears under the names claude, opencode, node and its version
// number, depending on how it was installed.
func isShell(prog string) bool { return sysinfo.IsShell(prog) }

type paneStateKind int

const (
	paneUnknown   paneStateKind = iota // tmux could not be asked, or did not say
	paneRunning                        // something other than a shell is in the foreground
	paneNoSession                      // the session does not exist
	paneShell                          // the session is at a shell prompt
)

type paneState struct {
	kind paneStateKind
	prog string
}

// Why the agent thinks the assistant is gone; these words go to the chat.
const (
	goneSession = "session gone"
	goneShell   = "a shell is in the foreground"
)

func (p paneState) reason() string {
	if p.kind == paneNoSession {
		return goneSession
	}
	return goneShell
}

// readPaneState looks at what runs in the pane. An answer that cannot be told (tmux hangs, fails in another way) is
// paneUnknown and makes nobody act: a tmux that does not answer is no proof that the assistant is gone.
func (a *Agent) readPaneState(ctx context.Context) paneState {
	ctx, cancel := context.WithTimeout(ctx, paneTimeout)
	defer cancel()
	prog, err := a.cfg.Screen.PaneCommand(ctx)
	switch {
	case errors.Is(err, screen.ErrNoSession):
		return paneState{kind: paneNoSession}
	case err != nil || strings.TrimSpace(prog) == "":
		return paneState{kind: paneUnknown}
	case isShell(prog):
		return paneState{kind: paneShell, prog: prog}
	}
	return paneState{kind: paneRunning, prog: prog}
}

// healClock is what auto-heal remembers between two looks.
type healClock struct {
	since  time.Time // when the pane was first seen without Claude Code (or last seen changing); zero while it runs
	reason string
	// line is the last line on the screen of a shell when the grace period began. Typing into the pane is allowed only
	// while it is still the same, idle prompt: a line that changed, or has text after the prompt, is somebody's work.
	line      string
	starts    []time.Time // the starts it made, within the window
	lastStart time.Time   // the last start of Claude Code, by auto-heal or by /continue
	gaveUp    bool        // it has said that it stops trying; cleared when Claude Code runs again
}

type healSettings struct {
	command, workdir string
	grace, window    time.Duration
	tries            int
}

func (a *Agent) healSettings() healSettings {
	h := a.cfg.Ext.Heal
	s := healSettings{command: strings.TrimSpace(h.Command), workdir: a.cfg.Ext.Workdir, grace: h.Grace, tries: h.MaxTries, window: h.Window}
	if s.command == "" {
		s.command = defaultAssistantCommand
	}
	if s.tries <= 0 {
		s.tries = defaultHealTries
	}
	if s.window <= 0 {
		s.window = defaultHealWindow
	}
	if s.grace < 0 {
		s.grace = 0
	}
	return s
}

// healTick is one look at the pane. It acts only when the assistant has been gone for the whole grace period, and at a
// shell only when the prompt has stayed idle for all of it.
func (a *Agent) healTick(ctx context.Context) {
	p := a.readPaneState(ctx)
	if p.kind == paneUnknown || ctx.Err() != nil {
		return
	}
	s := a.healSettings()
	locked := a.Locked() // a.mu is taken here and not while m.mu is held
	var look shellLook
	if p.kind == paneShell && !locked {
		look = a.lookAtShell(ctx)
		if ctx.Err() != nil {
			return
		}
	}
	now := a.cfg.Now()
	m := &a.mach
	m.mu.Lock()
	h := &m.heal
	switch {
	case p.kind == paneRunning:
		h.since, h.reason, h.line, h.gaveUp = time.Time{}, "", "", false
	case locked:
		// A locked machine is left alone, and the grace starts afresh when it is unlocked.
		h.since, h.reason, h.line = time.Time{}, "", ""
	default:
		switch {
		case h.since.IsZero() || h.reason != p.reason(): // another state means somebody is at work on the pane
			h.since, h.reason, h.line = now, p.reason(), look.line
		case p.kind == paneShell && (!look.idle || look.line != h.line):
			// Something is typed after the prompt, or the screen moved on: the owner is at the shell. The grace begins
			// again from what is on the screen now.
			h.since, h.line = now, look.line
		}
		if now.Sub(h.since) >= s.grace && !h.gaveUp && (p.kind != paneShell || look.idle) {
			m.mu.Unlock()
			a.healStart(ctx, s)
			return
		}
	}
	m.mu.Unlock()
}

// healStart makes a try, if the tries allow it. It looks at the pane again under the start lock, and at what healTick
// decided on: a /continue or the owner may have started Claude Code while this waited for the lock.
func (a *Agent) healStart(ctx context.Context, s healSettings) {
	m := &a.mach
	m.startMu.Lock()
	defer m.startMu.Unlock()
	p := a.readPaneState(ctx)
	if (p.kind != paneNoSession && p.kind != paneShell) || a.Locked() || ctx.Err() != nil {
		return
	}
	now := a.cfg.Now()
	m.mu.Lock()
	h := &m.heal
	// The look of healTick may be old by now. A /continue that ran meanwhile has cleared the clock and may have left a pane
	// that still shows the shell for a moment, which would get a second command.
	if h.since.IsZero() || now.Sub(h.since) < s.grace || h.reason != p.reason() ||
		(!h.lastStart.IsZero() && now.Sub(h.lastStart) < s.grace) {
		m.mu.Unlock()
		return
	}
	h.starts = startsWithin(h.starts, now, s.window)
	n := len(h.starts)
	if n >= s.tries {
		first := !h.gaveUp
		h.gaveUp = true
		m.mu.Unlock()
		if first {
			a.log.Warn("auto-heal gives up", "starts", n, "window", s.window.String())
			a.queueNotice(ctx, fmt.Sprintf("⚠️ %s is down and could not be kept running (%d starts in %s); use /continue when you want to try again", a.assistantName(), n, humanSpan(s.window)), nil)
		}
		return
	}
	h.starts = append(h.starts, now) // a start that fails counts too: it must not be repeated every poll
	seen := h.line
	m.mu.Unlock()

	launched, err := a.launch(ctx, s, p, &seen)
	busy := errors.Is(err, errShellBusy)
	m.mu.Lock()
	h.since, h.reason, h.line = time.Time{}, "", "" // the next try waits a grace period of its own
	if (!launched && err == nil) || busy {
		h.starts = h.starts[:len(h.starts)-1] // nothing was started: Claude Code is there after all, or the shell is in use
	} else {
		h.lastStart = a.cfg.Now()
	}
	m.mu.Unlock()
	switch {
	case busy:
		a.log.Debug("auto-heal leaves the shell alone: something is typed at its prompt", "why", p.reason())
	case err != nil:
		a.log.Warn("auto-heal could not start the assistant", "why", p.reason(), "err", err)
	case launched:
		a.log.Info("auto-heal started the assistant", "why", p.reason())
		a.stuckForget()
		a.queueNotice(ctx, fmt.Sprintf("♻️ %s had stopped (%s); started with %s", a.assistantName(), p.reason(), shownCommand(s.command)), nil)
	}
}

// errShellBusy means that the shell prompt has something typed after it (or the pane is scrolled back), so a command typed
// now would be glued to it.
var errShellBusy = errors.New("the shell prompt is in use")

// launch starts the assistant in a pane that is gone or at a shell. It reports whether it started anything; false with no
// error means the pane turned out to show the assistant already. At a shell it types only into an idle prompt; when seen is
// set, the last line of the screen must also be the one that auto-heal saw when its grace period began.
func (a *Agent) launch(ctx context.Context, s healSettings, p paneState, seen *string) (bool, error) {
	switch {
	case !filepath.IsAbs(s.workdir) || strings.ContainsAny(s.workdir, "\x00\n\r"):
		return false, &userError{"the working directory (PB_WORKDIR) must be an absolute path"}
	case strings.ContainsAny(s.command, "\x00\n\r"):
		return false, &userError{"the command to start the assistant (PB_ASSISTANT_COMMAND) must be one line"}
	}
	if st, err := os.Stat(s.workdir); err != nil || !st.IsDir() {
		return false, &userError{"the working directory does not exist, so " + a.assistantName() + " was not started"}
	}
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	switch p.kind {
	case paneNoSession:
		return true, a.cfg.Screen.Start(ctx, s.workdir, s.command)
	case paneShell:
		// A wrapper script that runs the assistant can make the pane look like a shell. Typing a command into the
		// assistant's own input box would send it to the assistant, so a screen that looks like the assistant is left alone.
		look := a.lookAtShell(ctx)
		if look.assistant {
			return false, nil
		}
		// The owner may have begun a line since the last look (rm -rf ~/old, no Enter yet); the command would be glued to it.
		if !look.idle || (seen != nil && look.line != *seen) {
			return false, errShellBusy
		}
		return true, a.cfg.Exec.SendText(ctx, "cd "+healShellQuote(s.workdir)+" && exec "+s.command)
	}
	return false, nil
}

// shellLook is what the pane of a shell shows.
type shellLook struct {
	line      string // the last line that has anything on it
	idle      bool   // the line is a prompt with nothing typed after it, and the pane is not scrolled back
	assistant bool   // the screen is the assistant's (a wrapper script runs it)
}

// paneModer is what a screen offers if it can tell that the pane is scrolled back (copy mode); without it the pane is
// taken to be in no mode.
type paneModer interface {
	PaneInMode(ctx context.Context) (bool, error)
}

// lookAtShell reads the pane of a shell. Anything it cannot tell makes the prompt not idle: the caller then types nothing.
func (a *Agent) lookAtShell(ctx context.Context) shellLook {
	ctx, cancel := context.WithTimeout(ctx, paneTimeout)
	defer cancel()
	if pm, ok := a.cfg.Screen.(paneModer); ok {
		if in, err := pm.PaneInMode(ctx); err != nil || in {
			return shellLook{}
		}
	}
	scr, err := a.cfg.Screen.Capture(ctx)
	if err != nil {
		return shellLook{}
	}
	l := shellLook{assistant: looksLikeAssistantCode(scr)}
	if ls := nonEmptyLines(scr); len(ls) > 0 {
		l.line = ls[len(ls)-1]
	}
	l.idle = endsWithPromptMark(l.line)
	return l
}

// nonEmptyLines splits a capture into its lines that have something on them, white space at the ends taken off.
func nonEmptyLines(scr string) []string {
	var out []string
	for _, l := range strings.Split(scr, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// promptMarks are the characters that the prompt of a shell commonly ends with: % for zsh and csh, $ for sh and bash, # for
// root, > for fish and others, and the arrows of the prompt themes.
const promptMarks = "%$#>❯➜»→λ▶"

// endsWithPromptMark says whether line, the last one on the screen, is a prompt with nothing typed after it. A prompt that
// ends otherwise is not recognised, and auto-heal then leaves the shell alone.
func endsWithPromptMark(line string) bool {
	r, _ := utf8.DecodeLastRuneInString(strings.TrimSpace(line))
	return r != utf8.RuneError && strings.ContainsRune(promptMarks, r)
}

// userHostRe is the start of the usual prompts: user@host followed by the directory.
var userHostRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+[\s:]`)

// looksLikeShellPrompt says whether the last line of a screen is a shell prompt, with or without a typed line after it.
func looksLikeShellPrompt(line string) bool {
	return endsWithPromptMark(line) || userHostRe.MatchString(strings.TrimSpace(line))
}

// tailOf is the last n elements of ls.
func tailOf(ls []string, n int) []string {
	if len(ls) > n {
		return ls[len(ls)-n:]
	}
	return ls
}

// topRule is the full-width line above a dialog of Claude Code.
var topRule = regexp.MustCompile(`^─{20,}$`)

// looksLikeAssistantCode says whether a screen shows the assistant's interface at its end: the permission mode or the hint
// under the input box, the hint to interrupt, a dialog with choices. Only the end of the screen counts. The patterns are
// those of Claude Code; for another assistant they simply do not match, which is safe: the pane is then taken as it is.
// When the assistant has exited inline, its last answer stays above the prompt of the shell, and that prompt is the last
// line; so a screen whose last line is a shell prompt is a remnant, not the assistant.
func looksLikeAssistantCode(scr string) bool {
	ls := nonEmptyLines(scr)
	if len(ls) == 0 || looksLikeShellPrompt(ls[len(ls)-1]) {
		return false
	}
	tail := strings.ToLower(strings.Join(tailOf(ls, 3), "\n"))
	if screen.Mode(tail) != "" || strings.Contains(tail, "? for shortcuts") || strings.Contains(tail, "shift+tab to cycle") {
		return true
	}
	if screen.Busy(strings.Join(tailOf(ls, 5), "\n")) {
		return true
	}
	// A dialog needs its rule: without one, ExtractPrompt takes the last 30 lines, which may be an answer with a list.
	for _, l := range ls {
		if topRule.MatchString(l) {
			_, ok := screen.ExtractPrompt(scr)
			return ok
		}
	}
	return false
}

// continueClaude is /continue: start the assistant if it is not running, without counting against the tries of auto-heal.
func (a *Agent) continueClaude(ctx context.Context) error {
	switch {
	case a.cfg.Screen == nil:
		return &userError{"refused: this agent cannot see its tmux session, so it cannot start the assistant"}
	case a.cfg.Ext.Workdir == "":
		return &userError{"refused: no working directory is configured (PB_WORKDIR), so the assistant cannot be started"}
	}
	s := a.healSettings()
	m := &a.mach
	m.startMu.Lock()
	defer m.startMu.Unlock()
	p := a.readPaneState(ctx)
	if p.kind == paneUnknown {
		return &userError{"could not look at the tmux session, so nothing was started"}
	}
	already := func() error {
		m.mu.Lock()
		m.heal.since, m.heal.reason, m.heal.line, m.heal.gaveUp = time.Time{}, "", "", false
		m.mu.Unlock()
		body := a.assistantName() + " is already running"
		if p.kind == paneRunning && !strings.EqualFold(strings.TrimSpace(p.prog), "claude") && !strings.EqualFold(strings.TrimSpace(p.prog), "opencode") {
			body += " (in the foreground: " + screen.Cut(oneLineOf(p.prog), 30) + ")"
		}
		a.queueNotice(ctx, body, nil)
		return nil
	}
	if p.kind == paneRunning {
		return already()
	}
	launched, err := a.launch(ctx, s, p, nil)
	var ue *userError
	switch {
	case errors.As(err, &ue):
		return ue
	case errors.Is(err, errShellBusy):
		return &userError{"refused: the shell prompt of the session is in use (text is typed after it, or the pane is scrolled back), so the assistant was not started. Clear the line on the laptop, then send /continue again"}
	case err != nil:
		a.log.Warn("could not start the assistant on request", "why", p.reason(), "err", err)
		return &userError{"could not start the assistant (the agent's log says why)"}
	case !launched:
		return already()
	}
	m.mu.Lock()
	m.heal.since, m.heal.reason, m.heal.line = time.Time{}, "", "" // auto-heal gives it a grace period before it looks again
	m.heal.lastStart = a.cfg.Now()                                 // and, if the pane still shows the shell, no second command
	m.mu.Unlock()
	a.log.Info("started the assistant on request", "why", p.reason())
	a.stuckForget()
	a.queueNotice(ctx, fmt.Sprintf("▶️ %s was started (%s) with %s", a.assistantName(), p.reason(), shownCommand(s.command)), nil)
	return nil
}

// within keeps the times that are not older than window.
func startsWithin(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < window {
			out = append(out, t)
		}
	}
	return out
}

// shQuote wraps s in single quotes for sh, fish and csh alike.
func healShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shownCommand is the command as the chat shows it: it comes from local settings, which may carry a key, so it is
// redacted and cut.
func shownCommand(c string) string { return screen.Cut(screen.Redact(oneLineOf(c)), 100) }

// cleanLine turns runs of white space and control characters into single spaces.
func oneLineOf(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// span writes a duration for the chat: "45 s", "10 min", "3 h".
func humanSpan(d time.Duration) string {
	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	case d < 90*time.Minute:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
	}
	return fmt.Sprintf("%d h", int(d.Round(time.Hour)/time.Hour))
}
