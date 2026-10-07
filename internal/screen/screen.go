// Package screen reads what Claude Code shows in its terminal: the prompt that waits for an answer, the permission mode,
// the last lines. It works on text only (a capture of a tmux pane), so it can be used by the hook that runs where Claude
// Code runs and by the agent that presses the keys, and both reach the same conclusion about the same screen.
//
// It depends on how Claude Code draws its dialogs, which is not a stable interface. Everything here therefore fails
// toward "no prompt found": a caller that gets ok=false sends a plain notice and presses nothing.
package screen

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ErrNoSession is returned by whatever reads a screen when the tmux session does not exist.
var ErrNoSession = errors.New("screen: no such session")

// Limits of what is sent to the chat.
const (
	// MaxPromptChars bounds the text of a prompt. A longer dialog loses its middle, see Prompt.Shortened.
	MaxPromptChars = 1800
	// tailChars is how much of the end of a long dialog is kept: the question and the choices are the last lines, and they
	// are what someone who answers from a phone must not lose. markerRoom is the room left for the "left out" mark.
	tailChars  = 500
	markerRoom = 40
	// MaxOptions bounds the numbered choices that become buttons.
	MaxOptions = 8
	// fallbackLines is how much of the bottom of the screen is looked at when no rule above the dialog is found.
	fallbackLines = 30
)

// Option is one numbered choice of a prompt. Key is the digit to press.
type Option struct {
	Key   string
	Label string
}

// Prompt is a dialog that waits for an answer.
type Prompt struct {
	// Text is the dialog as shown: title, command or question, choices. Secrets are hidden, and a dialog longer than
	// MaxPromptChars loses its middle (the head and the last tailChars characters stay).
	Text string
	// Options are the numbered choices, in order. There are always at least two.
	Options []Option
	// Hash identifies this prompt: 16 hex characters over the dialog as drawn, before anything is hidden or cut, keyed
	// with the install's key (see SetHashKey). It does not depend on which choice is highlighted, and it differs between
	// two prompts for different commands.
	Hash string
	// Shortened says that Text lacks part of the dialog, so that what is asked cannot be judged from it. A caller must
	// not offer to approve on the strength of a shortened text.
	Shortened bool
	// Redacted says that Redact hid something in Text.
	Redacted bool
}

var (
	topRule  = regexp.MustCompile(`^─{20,}$`)
	dashRule = regexp.MustCompile(`^╌+$`)
	optionRe = regexp.MustCompile(`^\s*(?:❯\s*)?(\d{1,2})\.\s+(\S.*)$`)
	cursorRe = regexp.MustCompile(`^\s*❯\s*`)
	// tipLineRe is the rotating tip that Claude Code draws in a dialog.
	tipLineRe = regexp.MustCompile(`^\s*Tip:`)
	// boxRe is a line inside the frame that holds the command or the plan: what the user approves.
	boxRe = regexp.MustCompile(`^\s*│`)
)

// lines splits a capture, drops trailing white space and the line ends.
func lines(s string) []string {
	ls := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range ls {
		ls[i] = strings.TrimRight(l, " \t\r")
	}
	return ls
}

// ExtractPrompt finds the dialog at the bottom of the screen. The dialog starts after the last full-width rule and must
// have numbered choices 1, 2, ... in a row; a screen without one (an idle input box, a long answer) is not a prompt.
func ExtractPrompt(screen string) (Prompt, bool) {
	all := lines(screen)
	top := -1
	for i, l := range all {
		if topRule.MatchString(l) {
			top = i
		}
	}
	var body []string
	if top >= 0 {
		body = all[top+1:]
	} else if len(all) > fallbackLines {
		body = all[len(all)-fallbackLines:]
	} else {
		body = all
	}
	// Neither list has the decoration. drawn keeps the blank lines, which tell two commands apart and so count in the
	// hash; shown leaves them out, as the chat gets it.
	var drawn, shown []string
	for _, l := range body {
		if dashRule.MatchString(l) {
			continue
		}
		drawn = append(drawn, l)
		if l != "" {
			shown = append(shown, l)
		}
	}
	opts := options(shown)
	if len(opts) < 2 {
		return Prompt{}, false
	}
	whole := strings.Join(shown, "\n")
	hidden := Redact(whole)
	text, cut := cutMiddle(hidden, MaxPromptChars, tailChars)
	return Prompt{Text: text, Options: opts, Hash: hashOf(drawn), Shortened: cut, Redacted: hidden != whole}, true
}

// options reads the numbered choices 1, 2, 3 ... from the dialog's lines. Numbers come up earlier in a dialog as well (a
// list inside the command, the steps of a plan), so every "1." starts a new run and the last run of at least two entries
// is the choices: they are the last thing in the dialog before its hint line. Prose after them that is numbered from 1
// again, with a single item, does not displace them.
func options(ls []string) []Option {
	var best, cur []Option
	for _, l := range ls {
		m := optionRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		switch {
		case m[1] == "1":
			if len(cur) >= 2 {
				best = cur
			}
			cur = []Option{{Key: "1", Label: strings.TrimSpace(m[2])}}
		case len(cur) > 0 && len(cur) < MaxOptions && m[1] == strconv.Itoa(len(cur)+1):
			cur = append(cur, Option{Key: m[1], Label: strings.TrimSpace(m[2])})
		}
		// a number that does not continue the run is prose
	}
	if len(cur) >= 2 {
		best = cur
	}
	return best
}

// Short cuts long labels for buttons.
func Short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Cut shortens s to at most n characters, marking the cut.
func Cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cutMiddle shortens s to about max characters by leaving out the middle: the head stays, and so do the last tail
// characters, which in a dialog are the question and the choices. The gap is marked with the number of characters left
// out. It reports whether anything was left out. Both ends are moved to a line boundary when that costs little.
func cutMiddle(s string, max, tail int) (string, bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	head := max - tail - markerRoom
	if i := lastRune(r[:head], '\n'); i >= head/2 {
		head = i
	}
	from := len(r) - tail
	if r[from-1] != '\n' {
		if i := firstRune(r[from:], '\n'); i >= 0 && i < tail/2 {
			from += i + 1
		}
	}
	return string(r[:head]) + "\n[" + strconv.Itoa(from-head) + " characters left out]\n" + string(r[from:]), true
}

func lastRune(r []rune, c rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == c {
			return i
		}
	}
	return -1
}

func firstRune(r []rune, c rune) int {
	for i, x := range r {
		if x == c {
			return i
		}
	}
	return -1
}

// BottomLines returns the last n non-empty lines of the screen, secrets hidden.
func BottomLines(screen string, n int) string {
	var ne []string
	for _, l := range lines(screen) {
		if l != "" {
			ne = append(ne, l)
		}
	}
	if len(ne) > n {
		ne = ne[len(ne)-n:]
	}
	return Redact(strings.Join(ne, "\n"))
}

// Modes, as Claude Code names them in the line under the input box.
const (
	ModeAuto        = "auto"
	ModeManual      = "manual"
	ModeAcceptEdits = "accept edits"
	ModePlan        = "plan"
	ModeBypass      = "bypass permissions"
	ModeDontAsk     = "don't ask"
)

var modeMarks = []struct{ mark, mode string }{
	{"auto mode on", ModeAuto},
	{"manual mode on", ModeManual},
	{"accept edits on", ModeAcceptEdits},
	{"plan mode on", ModePlan},
	{"bypass permissions on", ModeBypass},
	{"don't ask on", ModeDontAsk},
	{"dont ask on", ModeDontAsk},
}

// Mode reads the permission mode from the bottom lines of the screen; "" when none is shown.
func Mode(screen string) string {
	var ne []string
	for _, l := range lines(screen) {
		if l != "" {
			ne = append(ne, strings.ToLower(l))
		}
	}
	if len(ne) > 4 {
		ne = ne[len(ne)-4:]
	}
	for i := len(ne) - 1; i >= 0; i-- {
		for _, m := range modeMarks {
			if strings.Contains(ne[i], m.mark) {
				return m.mode
			}
		}
	}
	return ""
}

// Busy says whether Claude Code looks to be working: its status line offers to interrupt. Best effort.
func Busy(screen string) bool {
	var ne []string
	for _, l := range lines(screen) {
		if l != "" {
			ne = append(ne, strings.ToLower(l))
		}
	}
	if len(ne) > 8 {
		ne = ne[len(ne)-8:]
	}
	for _, l := range ne {
		if strings.Contains(l, "esc to interrupt") {
			return true
		}
	}
	return false
}
