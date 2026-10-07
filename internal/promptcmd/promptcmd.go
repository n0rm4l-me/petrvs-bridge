// Package promptcmd builds what the chat is told when Claude Code waits for an answer: the prompt as it is on the screen,
// with one button per choice, each bound to that prompt by its hash. `agent prompt` (the Notification hook) and the agent
// (when the hub asks for the screen) both use it, so they say the same thing and bind the buttons the same way.
package promptcmd

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/tmuxctl"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

const (
	// answerHint ends a notice that carries the prompt and its buttons; plainHint one that does not.
	answerHint = "Answer with the buttons, /key N, /enter or /esc."
	plainHint  = "Answer with /key N (the number of your choice), /enter or /esc."

	// hiddenNote follows a text in which something was hidden, and tooLong one that lacks part of the dialog. With that
	// text only Esc is offered as a button, since nobody should approve what they could not read; answerHint then names
	// no buttons.
	hiddenNote = "(some values are hidden)"
	tooLong    = "The command is too long to show here: look at it on the laptop (or /screen) before you answer."
	longHint   = "Answer with /key N, /enter or /esc."

	// screenLines is how much of the bottom of the screen a snapshot shows; maxScreenChars bounds it when the lines are
	// long, keeping the bottom. maxMessage bounds the hook's own message.
	screenLines    = 25
	maxScreenChars = 3500
	maxMessage     = 300
	// maxInput bounds what is read from the hook's standard input.
	maxInput = 1 << 20
	// labelRunes and labelBytes bound a button label: 30 characters to fit a phone, and the 40 bytes that the wire format
	// allows (a payload with a longer label is rejected whole, which would lose the notice).
	labelRunes = 30
	labelBytes = 40
)

// Notice is a message for the chat and the buttons under it.
type Notice struct {
	Body    string
	Buttons []wire.Button
}

// ForPrompt is the notice for a prompt on the screen. message is what Claude Code said ("Claude needs your permission").
// The notice is Markdown: the headline in bold, the dialog as it is drawn in a code block, the hint in italics.
func ForPrompt(message string, p screen.Prompt) Notice {
	body, hint := "⏳ **"+headline(message)+"**\n"+code(p.Text), answerHint
	if p.Redacted {
		body += "\n_" + hiddenNote + "_"
	}
	if p.Shortened {
		body, hint = body+"\n"+tooLong, longHint
	}
	return Notice{Body: body + "\n\n_" + hint + "_", Buttons: Buttons(p)}
}

// Plain is the notice when no prompt could be read from the screen: nothing but what Claude Code said.
func Plain(message string) Notice {
	return Notice{Body: "⏳ **" + headline(message) + "**\n_" + plainHint + "_"}
}

// code puts text in a Markdown code block, with a fence longer than any run of backticks inside it.
func code(text string) string {
	n := 3
	for run, i := 0, 0; i <= len(text); i++ {
		if i < len(text) && text[i] == '`' {
			run++
			continue
		}
		n = max(n, run+1)
		run = 0
	}
	fence := strings.Repeat("`", n)
	return fence + "\n" + text + "\n" + fence
}

// ruleLine is a line that is only a horizontal rule: Claude Code draws them as wide as the window, and on a phone that is a
// line to scroll along for nothing.
var ruleLine = regexp.MustCompile(`^[ ]*[─━═╌╍┄┅-]{20,}[ ]*$`)

func squashRules(text string) string {
	ls := strings.Split(text, "\n")
	for i, l := range ls {
		if ruleLine.MatchString(l) {
			ls[i] = strings.Repeat("─", 30)
		}
	}
	return strings.Join(ls, "\n")
}

// Buttons gives one button per choice of the prompt and one for Escape, all bound to the prompt by its hash; at most
// wire.MaxButtons (a prompt with more choices loses the last ones, which are still listed in the text). A prompt whose
// text was shortened gets Esc only: the choices are there, but the command they would approve is not fully shown, and a
// tap must not approve it.
func Buttons(p screen.Prompt) []wire.Button {
	var out []wire.Button
	for _, o := range p.Options {
		if p.Shortened || len(out) == wire.MaxButtons-1 {
			break
		}
		if len(o.Key) != 1 || o.Key[0] < '1' || o.Key[0] > '9' {
			continue
		}
		out = append(out, wire.Button{Label: label(o.Key + ". " + o.Label), Key: o.Key, Prompt: p.Hash})
	}
	return append(out, wire.Button{Label: "✖ Esc", Key: "Escape", Prompt: p.Hash})
}

// ForScreen is the snapshot of a screen that shows no prompt: its bottom lines, secrets hidden, with the buttons to move
// in what is shown. They carry no prompt hash, so the hub sends the keys as they are.
func ForScreen(agent, scr string) Notice {
	text := screen.BottomLines(scr, screenLines)
	if text == "" {
		text = "(the screen is empty)"
	}
	return Notice{Body: "📺 **" + agent + "'s screen**\n" + code(tail(squashRules(text), maxScreenChars)), Buttons: NavButtons()}
}

// NavButtons move in a list or dialog that the screen shows.
func NavButtons() []wire.Button {
	return []wire.Button{{Label: "↑", Key: "Up"}, {Label: "↓", Key: "Down"}, {Label: "⏎", Key: "Enter"}, {Label: "✖ Esc", Key: "Escape"}}
}

// Snapshot is what to show for a screen: the prompt with its buttons when there is one, else the bottom lines.
func Snapshot(agent, scr string) Notice {
	if p, ok := screen.ExtractPrompt(scr); ok {
		return ForPrompt("Claude Code is waiting for an answer", p)
	}
	return ForScreen(agent, scr)
}

// headline is the message on one line, secrets hidden, without the punctuation it ends in (the notice adds its own).
func headline(message string) string {
	m := strings.Join(strings.Fields(message), " ")
	m = strings.TrimRight(screen.Redact(m), ".:!? ")
	m = strings.NewReplacer("*", "", "`", "").Replace(m) // the headline is set in bold: a mark inside it would close it early
	return screen.Cut(m, maxMessage)
}

// label is the text of a button: secrets hidden, control characters gone, short enough for a phone and for the wire format.
func label(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, screen.Redact(s))
	s = screen.Short(s, labelRunes)
	for len(s) > labelBytes {
		r := []rune(strings.TrimSuffix(s, "…"))
		s = string(r[:len(r)-1]) + "…"
	}
	return s
}

// tail keeps the last n characters of s, starting at a line.
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	s = string(r[len(r)-n:])
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…\n" + s
}

// Hook is what the Notification hook is told by Claude Code.
type Hook struct{ Message, Type string }

// ReadHook reads the hook's JSON. It reports false when there is nothing to tell the user: the input is not what Claude
// Code sends, the message is empty, or the notification needs no answer (idle_prompt arrives a minute after every finished
// turn, auth_success when a login completes). Any other type is forwarded, also none at all (older versions send no
// type): missing a prompt that waits for the user costs more than a spare message.
func ReadHook(r io.Reader) (Hook, bool) {
	raw, err := io.ReadAll(io.LimitReader(r, maxInput))
	if err != nil {
		return Hook{}, false
	}
	var in struct {
		Message string `json:"message"`
		Type    string `json:"notification_type"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return Hook{}, false
	}
	in.Message = strings.TrimSpace(in.Message)
	if in.Message == "" || in.Type == "idle_prompt" || in.Type == "auth_success" {
		return Hook{}, false
	}
	return Hook{Message: in.Message, Type: in.Type}, true
}

// Queue takes the notice; the spool is one.
type Queue interface {
	AddItem(spool.Item) error
}

// Options are the inputs of Run. Only Stdin and Queue are required.
type Options struct {
	Stdin io.Reader
	// Pane is the tmux pane Claude Code runs in ($TMUX_PANE); empty means there is none to look at.
	Pane string
	// Capture reads the pane; it defaults to `tmux capture-pane -p -t PANE`.
	Capture func(ctx context.Context, pane string) (string, error)
	Queue   Queue
	// Tries captures are made, Pause apart, until a prompt shows (default 6 and 300 ms: the prompt may not be drawn yet
	// when the notification fires).
	Tries int
	Pause time.Duration
}

// paneRe is what tmux puts in $TMUX_PANE. Anything else is not looked at.
var paneRe = regexp.MustCompile(`^%[0-9]{1,9}$`)

// Capture reads a tmux pane.
func Capture(ctx context.Context, pane string) (string, error) {
	return tmuxctl.ExecOutput(ctx, "capture-pane", "-p", "-t", pane)
}

// Run is `agent prompt`: it reads the hook's JSON, looks at the screen, and queues a notice for the chat, with buttons
// when a prompt is on the screen. It returns an error only when the notice could not be queued.
func Run(ctx context.Context, o Options) error {
	h, ok := ReadHook(o.Stdin)
	if !ok {
		return nil
	}
	n := Plain(h.Message)
	if p, found := o.find(ctx); found {
		n = ForPrompt(h.Message, p)
	}
	return o.Queue.AddItem(spool.Item{Type: wire.TypeNotice, Body: n.Body, Buttons: n.Buttons})
}

// find looks at the pane until a prompt shows or the tries are used. A pane that cannot be read is not tried again.
func (o Options) find(ctx context.Context) (screen.Prompt, bool) {
	if !paneRe.MatchString(o.Pane) {
		return screen.Prompt{}, false
	}
	capture, tries, pause := o.Capture, o.Tries, o.Pause
	if capture == nil {
		capture = Capture
	}
	if tries <= 0 {
		tries = 6
	}
	if pause <= 0 {
		pause = 300 * time.Millisecond
	}
	for i := 0; i < tries && ctx.Err() == nil; i++ {
		scr, err := capture(ctx, o.Pane)
		if err != nil {
			return screen.Prompt{}, false
		}
		if p, ok := screen.ExtractPrompt(scr); ok {
			return p, true
		}
		if i < tries-1 {
			select {
			case <-time.After(pause):
			case <-ctx.Done():
			}
		}
	}
	return screen.Prompt{}, false
}
