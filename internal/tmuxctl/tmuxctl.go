// Package tmuxctl types text and keys into a tmux session. Every argument is
// validated: text is sent literally after "--", keys come from a short allow-list.
package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// MaxText is the longest text accepted in one command.
const MaxText = 8192

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

var allowedKeys = map[string]string{
	"Enter": "Enter", "Escape": "Escape", "Up": "Up", "Down": "Down", "Tab": "Tab",
	"1": "1", "2": "2", "3": "3", "4": "4", "5": "5", "6": "6", "7": "7", "8": "8", "9": "9",
}

var (
	ErrText    = errors.New("tmuxctl: text not acceptable")
	ErrKey     = errors.New("tmuxctl: key not allowed")
	ErrSession = errors.New("tmuxctl: invalid session name")
)

// Runner runs tmux with the given arguments.
type Runner func(ctx context.Context, args ...string) error

// ExecRunner runs the real tmux binary.
func ExecRunner(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Tmux controls one session.
type Tmux struct {
	session string
	run     Runner
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
	return &Tmux{session: session, run: run}, nil
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
	return t.run(ctx, "send-keys", "-t", t.session, "-l", "--", escapeTrailingSemicolon(text), ";",
		"run-shell", "sleep "+enterDelay, ";", "send-keys", "-t", t.session, "Enter")
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
	return t.run(ctx, "send-keys", "-t", t.session, name)
}
