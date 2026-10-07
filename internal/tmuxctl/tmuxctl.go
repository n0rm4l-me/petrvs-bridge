// Package tmuxctl types text and keys into a tmux session. Every argument is
// validated: text is sent literally after "--", keys come from a short allow-list.
package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
)

// MaxText is the longest text accepted in one command.
const MaxText = 8192

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// BTab is Shift+Tab, which Claude Code uses to cycle its permission mode. Only these exact names are accepted.
var allowedKeys = map[string]string{
	"Enter": "Enter", "Escape": "Escape", "Up": "Up", "Down": "Down", "Tab": "Tab", "BTab": "BTab",
	"1": "1", "2": "2", "3": "3", "4": "4", "5": "5", "6": "6", "7": "7", "8": "8", "9": "9",
}

var (
	ErrText    = errors.New("tmuxctl: text not acceptable")
	ErrKey     = errors.New("tmuxctl: key not allowed")
	ErrSession = errors.New("tmuxctl: invalid session name")
)

// Runner runs tmux with the given arguments.
type Runner func(ctx context.Context, args ...string) error

// runTimeout bounds one tmux invocation; the longest one is the 0.3 s pause between the text and Enter.
const runTimeout = 10 * time.Second

// ExecRunner runs the real tmux binary. The caller's cancellation is not passed on: it would kill the tmux client, and
// the tmux server then drops the rest of a ";" sequence, so a cancel during the pause would leave the text typed and
// Enter never sent (and the next command appended to it). Whatever was started is finished, within runTimeout.
func ExecRunner(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// OutRunner runs tmux with the given arguments and returns what it printed.
type OutRunner func(ctx context.Context, args ...string) (string, error)

// ExecOutput runs the real tmux binary and returns its output. It gives a read ten seconds.
func ExecOutput(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if noSession(msg) {
			return "", screen.ErrNoSession
		}
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, msg)
	}
	return string(out), nil
}

// noSession recognises the ways tmux says that the session (or the whole server) is not there.
func noSession(msg string) bool {
	for _, m := range []string{"can't find session", "can't find pane", "can't find window", "no server running", "error connecting to", "no sessions"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// Tmux controls one session.
type Tmux struct {
	session string // the name as configured
	target  string // exact-match tmux target, see exactTarget
	run     Runner
	out     OutRunner
	mu      sync.Mutex // one command at a time: two senders must not interleave their keystrokes
}

// New returns a controller for the session; run may be nil to use the real tmux.
func New(session string, run Runner) (*Tmux, error) {
	if !sessionRe.MatchString(session) {
		return nil, ErrSession
	}
	if run == nil {
		run = ExecRunner
	}
	return &Tmux{session: session, target: exactTarget(session), run: run, out: ExecOutput}, nil
}

// WithOutput sets how tmux is run when its output is wanted (Capture, PaneCommand); tests use it.
func (t *Tmux) WithOutput(out OutRunner) *Tmux {
	t.out = out
	return t
}

// Capture returns the text on the screen of the session's pane. It returns screen.ErrNoSession when the session is gone.
func (t *Tmux) Capture(ctx context.Context) (string, error) {
	return t.out(ctx, "capture-pane", "-p", "-t", t.target)
}

// PaneCommand returns the name of the program that runs in the foreground of the active pane ("claude", "zsh", "node"...).
// It returns screen.ErrNoSession when the session is gone.
func (t *Tmux) PaneCommand(ctx context.Context) (string, error) {
	line, err := t.activePane(ctx, "#{pane_current_command}")
	return strings.TrimSpace(line), err
}

// PaneInMode says whether the active pane is in a tmux mode (copy mode after the owner scrolled with the mouse). Then the
// capture shows the history, not the end of the screen. It returns screen.ErrNoSession when the session is gone.
func (t *Tmux) PaneInMode(ctx context.Context) (bool, error) {
	line, err := t.activePane(ctx, "#{pane_in_mode}")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(line) {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("tmux: cannot tell whether the pane is in a mode: %q", line)
}

// activePane prints format for the active pane of the session's window, the one that send-keys and capture-pane use. A
// window may be split, and the pane that list-panes names first is then not the one that gets the keys. The active flag
// is tested by hand (not with -f, which needs tmux 3.2).
func (t *Tmux) activePane(ctx context.Context, format string) (string, error) {
	// list-panes, not display-message: for a session that does not exist display-message prints nothing and succeeds.
	out, err := t.out(ctx, "list-panes", "-t", t.target, "-F", "#{pane_active} "+format)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimRight(l, "\r"), "1 "); ok {
			return rest, nil
		}
	}
	return "", errors.New("tmux: the window has no active pane")
}

// Start creates the session and runs command in it, in workdir. The command comes from the local settings, never from
// the chat. The cd is part of the command: a tmux server whose own directory was deleted starts sessions there whatever
// -c says, and a program such as claude then stops with "the current working directory was deleted".
func (t *Tmux) Start(ctx context.Context, workdir, command string) error {
	if !filepath.IsAbs(workdir) || strings.ContainsAny(workdir, "\x00\n\r") || strings.TrimSpace(command) == "" || strings.ContainsAny(command, "\x00\n\r") {
		return errors.New("tmuxctl: a working directory (absolute) and a one-line command are needed to start a session")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.run(ctx, "new-session", "-d", "-s", t.session, "-c", workdir, "cd "+shellQuote(workdir)+" && exec "+command)
}

// shellQuote wraps s in single quotes for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// exactTarget turns the configured name into a target that only the session of exactly that name can match. tmux reads a
// plain "-t NAME" as a prefix when no session has that name, so with the session gone the text and the keys (Enter, or
// the "1" that answers a permission prompt) would go into "NAME-dev" or any other session that starts with it, and the
// agent would report success. The "=" asks for an exact match; the trailing ":" is needed because tmux does not accept a
// bare "=NAME" where it expects a pane. A name that already has a ":" is a session and window, and stays as it is.
func exactTarget(session string) string {
	if strings.Contains(session, ":") {
		return "=" + session
	}
	return "=" + session + ":"
}

// ValidateText checks text without sending it.
func ValidateText(text string) error {
	if text == "" || len(text) > MaxText || !utf8.ValidString(text) {
		return ErrText
	}
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) { // NUL, escape sequences, backspace and the like
			return ErrText
		}
	}
	return nil
}

// enterDelay is the pause between the typed text and Enter. Claude Code reads a fast burst of characters as a paste, and
// an Enter that follows at once becomes a line break inside it instead of "submit": against Claude Code 2.1.292 texts of
// 112 and 193 bytes stayed in the input box while 44 bytes went through. 0.05 s was enough in that test, also for 7 KB;
// 0.3 s leaves a margin.
const enterDelay = "0.3"

// SendText types the text literally and presses Enter.
func (t *Tmux) SendText(ctx context.Context, text string) error {
	text = strings.TrimRight(text, "\r\n")
	if err := ValidateText(text); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Text, the pause and Enter go in one tmux invocation, joined by lone ";" arguments, so nothing can slip in between.
	// run-shell waits for its command, so the pause holds up the rest of the sequence.
	return t.run(ctx, append(t.leaveModeFirst(), "send-keys", "-t", t.target, "-l", "--", escapeTrailingSemicolon(text), ";",
		"run-shell", "sleep "+enterDelay, ";", "send-keys", "-t", t.target, "Enter")...)
}

// leaveModeFirst is the start of a tmux sequence that types: it takes the pane out of copy mode, if it is in one. In copy
// mode send-keys goes to the copy-mode key table, where "1" or a line of text does nothing and tmux still reports
// success, so the agent would say "done" about keys that never reached Claude Code. It is part of the same invocation as
// the typing, so the order of commands is kept, and it names the same exact target.
func (t *Tmux) leaveModeFirst() []string {
	return []string{"if-shell", "-F", "-t", t.target, "#{pane_in_mode}", "send-keys -X -t " + t.target + " cancel", ";"}
}

// escapeTrailingSemicolon stops tmux from reading a final ";" of the text as a command separator: tmux turns an
// argument ending in "\;" into one ending in ";", and drops a bare trailing ";" as a separator.
func escapeTrailingSemicolon(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + `\;`
	}
	return s
}

// SendKey presses one allowed key.
func (t *Tmux) SendKey(ctx context.Context, key string) error {
	name, ok := allowedKeys[key]
	if !ok {
		return ErrKey
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.run(ctx, append(t.leaveModeFirst(), "send-keys", "-t", t.target, name)...)
}
