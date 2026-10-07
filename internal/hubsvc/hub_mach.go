package hubsvc

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/n0rm4l-me/petrvs-bridge/internal/sysinfo"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// What concerns the machine itself: the status card, and starting Claude Code again.

type machState struct{}

func (m *machState) init(h *Hub) {}

// Help text of this part, appended to the list of commands (starts with a space).
const machineHelp = " /continue"

// machineMenu is what this part adds to the bot's command menu (see chatMenu). /continue is not in it: a tap on the
// menu entry would start a program.
func machineMenu() []telegram.Command { return nil }

// machineTargeted says which of this part's commands act on one agent (see chatTargeted). It says none for now:
// targeted() sends such a command to runTargeted, which knows only keys and /alarm, so the command would be lost there.
// /continue finds its agent itself, in machineCommand, the way plain text does.
func machineTargeted(cmd string) bool { return false }

// machineCommand handles this part's commands; it reports whether cmd was one of them.
func (h *Hub) machineCommand(ctx context.Context, cmd, arg string, re reply, sentAt time.Time, msgID int64) bool {
	if cmd != "/continue" {
		return false
	}
	agent, ok := "", false
	if arg != "" { // /continue NAME
		if _, known := h.lookup(arg); !known {
			h.say(ctx, "Unknown agent. /agents lists them.")
			return true
		}
		agent, ok = arg, true
	} else {
		agent, ok = h.destination(ctx, re)
	}
	if !ok {
		return true
	}
	h.startOn(ctx, agent, sentAt, msgID)
	return true
}

// machineAtName says which of this part's commands can also be given as "@name command". /continue takes an agent name
// of its own as an argument, so it is not among the commands that machineTargeted makes go through the destination.
func machineAtName(cmd string) bool { return cmd == "/continue" }

// startOn asks one agent to start Claude Code in its session.
func (h *Hub) startOn(ctx context.Context, agent string, sentAt time.Time, msgID int64) {
	// It starts a program on the machine: leave a trace, like the permission mode key does.
	h.log.Info("start of Claude Code requested from the chat", "agent", agent)
	h.deliver(ctx, agent, wire.TypeStart, "", h.cfg.KeyTTL, sentAt, msgID)
}

// ---- the status card ----

type cardState int

const (
	cardNever cardState = iota
	cardOnline
	cardOffline
)

// cardAgent is what the card shows of one agent: its state, when it was last heard and the facts of its last heartbeat.
type cardAgent struct {
	name   string
	marked bool // the active agent, marked when there are several
	state  cardState
	silent time.Duration
	info   map[string]string
}

// renderCard writes the card of one agent. What the agent did not report is left out; the facts of an agent that is
// offline are the last it reported, and say so.
func renderCard(c cardAgent) string {
	head := c.name
	if c.marked {
		head = "● " + head
	}
	switch c.state {
	case cardOnline:
		head += " — online, heard " + ago(c.silent) + " ago"
	case cardOffline:
		head += " — offline, last heard " + ago(c.silent) + " ago"
	default:
		head += " — never connected"
	}
	lines := []string{head}
	if facts := factLines(c.info); len(facts) > 0 {
		if c.state == cardOffline {
			lines = append(lines, "🕓 last known:")
		}
		lines = append(lines, facts...)
	}
	return strings.Join(lines, "\n")
}

// knownFacts are the heartbeat keys the card has a place for.
var knownFacts = map[string]bool{"bat": true, "up": true, "disk": true, "tmux": true, "cc": true, "mode": true, "q": true,
	"locked": true, "alarm": true, "alarm_blind": true, "host": true, "os": true}

const maxOtherFacts = 6

// factLines are the lines under the heading: power and disk, the session, the lock and the alarm, the host.
func factLines(info map[string]string) []string {
	get := func(k string) string { return cardText(info[k]) }
	var out []string
	add := func(prefix string, parts []string) {
		if len(parts) > 0 {
			out = append(out, prefix+strings.Join(parts, " · "))
		}
	}
	var l []string
	if v := get("bat"); v != "" {
		l = append(l, batteryIcon(v)+" "+v)
	}
	if v := get("up"); v != "" {
		l = append(l, "⏱ up "+v)
	}
	if v := get("disk"); v != "" {
		l = append(l, "💾 "+v)
	}
	add("", l)

	l = nil
	if v := get("tmux"); v != "" {
		l = append(l, "tmux "+v)
	}
	if v := get("cc"); v != "" {
		if sysinfo.IsShell(v) && get("mode") == "" { // a shell with no Claude Code screen: it is not running
			v = "⚠️ shell (" + v + ")"
		}
		l = append(l, v)
	}
	if v := get("mode"); v != "" {
		l = append(l, v+" mode")
	}
	if v := get("q"); v != "" {
		l = append(l, "queue "+v)
	}
	add("🖥 ", l)

	l = nil
	switch get("locked") {
	case "true":
		l = append(l, "🔒 locked")
	case "false":
		l = append(l, "🔓 unlocked")
	}
	switch get("alarm") {
	case "on", "armed":
		l = append(l, "🚨 alarm armed")
	case "off":
		l = append(l, "alarm off")
	}
	if v := get("alarm_blind"); v != "" {
		l = append(l, "⚠️ alarm blind: "+v)
	}
	add("", l)

	host, os := get("host"), get("os")
	switch {
	case host != "" && os != "":
		out = append(out, "💻 "+host+" ("+os+")")
	case host != "" || os != "":
		out = append(out, "💻 "+host+os)
	}

	var keys []string
	for k := range info {
		if !knownFacts[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxOtherFacts {
		keys = keys[:maxOtherFacts]
	}
	l = nil
	for _, k := range keys {
		if v := get(k); v != "" {
			l = append(l, cardText(k)+"="+v)
		}
	}
	add("", l)
	return out
}

// batteryIcon is the empty battery for a low charge that is not being charged.
func batteryIcon(v string) string {
	if f := strings.Fields(v); len(f) > 0 && !strings.Contains(v, "charging") {
		if n, err := strconv.Atoi(strings.TrimSuffix(f[0], "%")); err == nil && n < 20 {
			return "🪫"
		}
	}
	return "🔋"
}

// cardText makes a value from a heartbeat fit on a line of the card: the agent is authenticated, but a line of the card
// is not the place for its line breaks or for more than a short text.
func cardText(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

// ago writes how long ago something was: "12 s", "5 min", "3 h 5 min", "4 d".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%d d", int(d/(24*time.Hour)))
}
