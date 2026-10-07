package hubsvc

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// What the user sees of the session: buttons under a prompt, the screen on request, and the commands that type into the
// session (/stop, /clear, ...).
//
// A button is only a request. The hub sends a key that is on a short list, to an agent it knows, and the agent presses it
// only if the prompt the button belongs to is still on the screen (see the agent's checkPrompt).

type uiState struct {
	mu sync.Mutex
	// tapped holds the chat messages whose buttons were pressed, oldest first in ring, so that a second tap on the same
	// message (a double tap, or one that was already on its way when the buttons were taken off) sends nothing.
	tapped map[int64]struct{}
	ring   []int64
	// sent is when the hub sent each chat message that has buttons, sentRing their order, oldest first. A tap carries no
	// date of its own, so this is what tells a button of a prompt from a minute ago from one of last week.
	sent     map[int64]time.Time
	sentRing []int64
}

func (u *uiState) init(h *Hub) {
	u.tapped = map[int64]struct{}{}
	u.sent = map[int64]time.Time{}
}

// buttonsLife is how long after they were sent the buttons of a message are obeyed. A prompt that is still waiting after
// this long is better looked at again (/screen) than answered from an old message.
const buttonsLife = 15 * time.Minute

// noteButtons remembers that these chat messages were sent, with buttons, at the given time.
func (u *uiState) noteButtons(ids []int64, at time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := u.sent[id]; !ok {
			u.sentRing = append(u.sentRing, id)
		}
		u.sent[id] = at
	}
	for len(u.sentRing) > maxTrackedMessages {
		delete(u.sent, u.sentRing[0])
		u.sentRing = u.sentRing[1:]
	}
}

// fresh says whether a tap on this message is to be obeyed: the hub sent it with buttons, not longer ago than buttonsLife.
// A message the hub does not remember (it was restarted since, or the message is among the oldest) is not.
func (u *uiState) fresh(messageID int64, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	at, ok := u.sent[messageID]
	return ok && now.Sub(at) <= buttonsLife
}

// maxTappedMessages bounds how many messages with pressed buttons are remembered.
const maxTappedMessages = 256

// claim notes that the buttons of a message are being used; it reports false when they were used before.
func (u *uiState) claim(messageID int64) bool {
	if messageID == 0 {
		return true
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, used := u.tapped[messageID]; used {
		return false
	}
	u.tapped[messageID] = struct{}{}
	u.ring = append(u.ring, messageID)
	for len(u.ring) > maxTappedMessages {
		delete(u.tapped, u.ring[0])
		u.ring = u.ring[1:]
	}
	return true
}

// release gives the buttons of a message back after claim, for a tap that did not send anything.
func (u *uiState) release(messageID int64) {
	if messageID == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.tapped, messageID)
	for i, id := range u.ring {
		if id == messageID {
			u.ring = append(u.ring[:i], u.ring[i+1:]...)
			break
		}
	}
}

// Help text of this part, appended to the list of commands (starts with a space).
const chatHelp = " /screen /sessions /stop /clear /compact /resume"

// chatMenu is what this part adds to the bot's command menu. Only commands that are harmless when tapped by accident
// belong there, and none that needs an argument, since a tap sends the bare command. /clear, /compact and /resume are
// not in it: they change the session.
func chatMenu() []telegram.Command {
	return []telegram.Command{
		{Name: "screen", Description: "Show the bottom of the session's screen, with buttons"},
		{Name: "sessions", Description: "Ask Claude Code to list all its sessions"},
		{Name: "stop", Description: "Interrupt Claude Code (presses Escape)"},
	}
}

// chatTargeted says which of this part's commands act on one agent (and so take @name, a swipe-reply or the active
// agent as their destination). All of them do: targeted() lets handleTo find the agent, and runTargeted hands the command
// to runChat, also for "@name /stop" and the like. Any other "@name /something" is typed into the session as text.
func chatTargeted(cmd string) bool {
	switch cmd {
	case "/stop", "/screen", "/sessions":
		return true
	}
	_, ok := slashCommands[cmd]
	return ok
}

// Commands that type a slash command into the session as text, and what they type.
var slashCommands = map[string]string{"/clear": "/clear", "/compact": "/compact", "/resume": "/resume"}

// sessionsAsk is what /sessions types: the assistant answers with the table, which the Stop hook sends to the chat.
const sessionsAsk = "List all your sessions (use ListAgents) in a short table."

// chatCommand handles this part's commands; it reports whether cmd was one of them.
func (h *Hub) chatCommand(ctx context.Context, cmd, arg string, re reply, sentAt time.Time, msgID int64) bool {
	switch cmd {
	case "/stop", "/screen", "/sessions":
	default:
		if _, ok := slashCommands[cmd]; !ok {
			return false
		}
	}
	if agent, ok := h.destination(ctx, re); ok {
		h.runChat(ctx, agent, cmd, arg, sentAt, msgID)
	}
	return true
}

// runChat carries out one of this part's commands on an agent. They are commands of the moment, so a backlog of them
// (the hub was away) is not acted on later: they live as long as a key does.
func (h *Hub) runChat(ctx context.Context, agent, cmd, arg string, sentAt time.Time, msgID int64) {
	ttl := h.cfg.KeyTTL
	switch cmd {
	case "/stop":
		h.deliverWith(ctx, agent, wire.TypeKey, "Escape", nil, ttl, sentAt, msgID)
	case "/screen":
		h.deliverWith(ctx, agent, wire.TypeScreen, "", nil, ttl, sentAt, 0)
	case "/sessions":
		h.deliverWith(ctx, agent, wire.TypeText, sessionsAsk, nil, ttl, sentAt, msgID)
	default:
		text := slashCommands[cmd]
		if arg != "" { // /compact can be told what to keep, /resume which session
			text += " " + arg
		}
		var meta map[string]string
		if cmd == "/resume" {
			meta = map[string]string{wire.MetaEcho: "screen"} // the list of sessions is the answer
		}
		h.deliverWith(ctx, agent, wire.TypeText, text, meta, ttl, sentAt, msgID)
	}
}

// keyMeta is what a key sent from the chat asks of the agent besides being pressed: Shift+Tab changes the permission
// mode, which the user cannot see from the phone, so the agent reports it.
func keyMeta(key string) map[string]string {
	if key == "BTab" {
		return map[string]string{wire.MetaEcho: "mode"}
	}
	return nil
}

// Callback data of the buttons this part makes. Telegram allows 64 bytes.
const (
	keyData         = "k:" // k:<agent>:<key>:<prompt>
	screenData      = "s:" // s:<agent>
	maxCallbackData = 64
	buttonsPerRow   = 3
)

// buttonKeys are the keys a button may press. BTab is not among them: it changes the permission mode, which decides what
// Claude Code may do without asking, and must not be one tap away from an old message.
var buttonKeys = map[string]bool{
	"Enter": true, "Escape": true, "Up": true, "Down": true, "Tab": true,
	"1": true, "2": true, "3": true, "4": true, "5": true, "6": true, "7": true, "8": true, "9": true,
}

// promptRe is what a prompt hash may look like in callback data (the agent compares it with its own).
var promptRe = regexp.MustCompile(`^[0-9A-Za-z]{0,16}$`)

// buttonData is the callback data of a button, false when the hub would not accept a tap on it: a key it does not send, a
// hash it cannot carry, data over the limit. Such a button is left out; it is never shortened into something else.
func buttonData(agent string, b wire.Button) (string, bool) {
	if !identity.ValidName(agent) || b.Label == "" {
		return "", false
	}
	var data string
	switch {
	case b.Key == wire.ButtonScreen:
		data = screenData + agent
	case buttonKeys[b.Key] && promptRe.MatchString(b.Prompt):
		data = keyData + agent + ":" + b.Key + ":" + b.Prompt
	default:
		return "", false
	}
	return data, len(data) <= maxCallbackData
}

// messageFor is the message to show for a reply, notice or alert of an agent: the text, with buttons when the agent sent
// some.
func (h *Hub) messageFor(agent string, p *wire.Payload, text string) telegram.Message {
	m := telegram.Message{Text: text}
	var row []telegram.Button
	left := 0
	for _, b := range p.Buttons {
		data, ok := buttonData(agent, b)
		if !ok {
			left++
			continue
		}
		row = append(row, telegram.Button{Text: b.Label, Data: data})
		if len(row) == buttonsPerRow {
			m.Buttons, row = append(m.Buttons, row), nil
		}
	}
	if len(row) > 0 {
		m.Buttons = append(m.Buttons, row)
	}
	if left > 0 {
		h.log.Warn("left out buttons the hub does not offer", "agent", agent, "buttons", left)
	}
	return m
}

// tapExtra handles a tap on a button that is not an agent choice; it reports whether it did. answer ends the button's
// loading state and may show a short text.
func (h *Hub) tapExtra(ctx context.Context, u telegram.Update, answer func(string)) bool {
	kind, rest, found := strings.Cut(u.CallbackData, ":")
	if !found || (kind != "k" && kind != "s") {
		return false
	}
	h.mu.Lock()
	locked := h.locked
	h.mu.Unlock()
	if locked {
		answer("Locked")
		return true
	}
	// A button answers the prompt it was made for, and nothing says how long ago that was. An old message, or one the hub
	// has forgotten, is not obeyed; /screen shows what is waiting now.
	if !h.ui.fresh(u.MessageID, h.cfg.Now()) {
		answer("Those buttons have expired; use /screen.")
		return true
	}
	if kind == "s" {
		if h.tapTarget(rest, answer) {
			if h.deliverWith(ctx, rest, wire.TypeScreen, "", nil, h.cfg.KeyTTL, time.Time{}, 0) {
				answer("Looking…")
			} else {
				answer("Not sent")
			}
		}
		return true
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 2 && len(parts) != 3 {
		answer("Unknown button")
		return true
	}
	name, key, prompt := parts[0], parts[1], ""
	if len(parts) == 3 {
		prompt = parts[2]
	}
	if !buttonKeys[key] || !promptRe.MatchString(prompt) {
		h.log.Warn("refused a button that presses a key the hub does not send from buttons", "agent", name, "key", key)
		answer("Not allowed")
		return true
	}
	if !h.tapTarget(name, answer) {
		return true
	}
	if !h.ui.claim(u.MessageID) {
		answer("Already answered")
		return true
	}
	// A key that answers a prompt carries the prompt's hash, and the agent presses it only if that prompt is still shown.
	// A key without one (moving in a list) is followed by the screen, since the buttons under the message are gone; the
	// same mark tells the agent to leave Enter and numbers alone while a prompt is on the screen, since the button says
	// nothing about which screen it was made for.
	meta := map[string]string{wire.MetaEcho: "screen"}
	if prompt != "" {
		meta = map[string]string{wire.MetaPrompt: prompt}
	}
	h.log.Info("button pressed", "agent", name, "key", key, "bound_to_prompt", prompt != "")
	if !h.deliverWith(ctx, name, wire.TypeKey, key, meta, h.cfg.KeyTTL, time.Time{}, 0) {
		// Nothing went out (the bus, an agent that is reconnecting, a lockdown just now): the prompt is still waiting, so
		// the buttons stay and can be tapped again.
		h.ui.release(u.MessageID)
		answer("Not sent")
		return true
	}
	answer("Sent " + key)
	if ed, ok := h.cfg.Telegram.(telegram.MarkupEditor); ok && u.MessageID != 0 {
		if err := ed.ClearButtons(ctx, h.cfg.ChatID, u.MessageID); err != nil && ctx.Err() == nil {
			h.log.Debug("could not take the buttons off a message", "err", err)
		}
	}
	return true
}

// tapTarget says whether a tap can be carried out on this agent, and answers the tap when it cannot: the agent must be
// one the hub knows, and online (a key sent to a machine that is away would arrive after the prompt is long gone).
func (h *Hub) tapTarget(name string, answer func(string)) bool {
	if _, known := h.lookup(name); !known {
		answer("Unknown agent")
		return false
	}
	if !h.Online(name) {
		answer(name + " is offline")
		return false
	}
	return true
}
