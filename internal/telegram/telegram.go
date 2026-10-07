// Package telegram is a small Telegram Bot API client: long polling for text
// messages and sending text. It never puts the bot token in an error or a log line.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxMessage is Telegram's limit for one message, with headroom.
const MaxMessage = 4000

// Update is one incoming message, or a tap on an inline button.
type Update struct {
	ID        int64
	ChatID    int64
	UserID    int64
	MessageID int64
	// ReplyToID is the message this one answers (a swipe-reply in the chat), 0 when it answers none.
	ReplyToID int64
	Text      string
	Date      time.Time // when Telegram received the message; zero when unknown
	// CallbackID and CallbackData are set instead of Text when the user tapped an inline button; MessageID is then the
	// message that carries the button.
	CallbackID, CallbackData string
}

// Button is one inline button under a message. Data comes back in the Update when the button is tapped (Telegram
// allows 64 bytes).
type Button struct{ Text, Data string }

// Message is text to send, with optional rows of inline buttons under its last part.
type Message struct {
	Text    string
	Buttons [][]Button
}

// Client is what the hub needs from Telegram.
type Client interface {
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	Send(ctx context.Context, chatID int64, text string) error
	// SendMessage is Send with buttons; it returns the ids Telegram gave to the parts of the message, so that a
	// later reply to one of them can be told apart.
	SendMessage(ctx context.Context, chatID int64, m Message) ([]int64, error)
	// AnswerCallback ends the "loading" state of a tapped button; text, if any, is shown briefly to the user.
	AnswerCallback(ctx context.Context, callbackID, text string) error
}

// Command is one entry of the bot's command menu, the list Telegram shows when the user types "/". The name has no
// leading slash.
type Command struct{ Name, Description string }

// CommandMenu is implemented by clients that can publish the command menu. The hub uses it when it is there.
type CommandMenu interface {
	SetCommands(ctx context.Context, cmds []Command) error
}

// Reactor is implemented by clients that can mark a message with an emoji reaction. The hub uses it when it is there.
type Reactor interface {
	React(ctx context.Context, chatID, messageID int64, emoji string) error
}

// HTTP talks to the real Bot API.
type HTTP struct {
	token string
	base  string
	hc    *http.Client
}

// NewHTTP returns a client for the bot token.
func NewHTTP(token string) *HTTP {
	return &HTTP{token: token, base: "https://api.telegram.org", hc: &http.Client{}}
}

// WithBaseURL points the client at another server (tests, or a local Bot API server).
func (c *HTTP) WithBaseURL(u string) *HTTP { c.base = strings.TrimRight(u, "/"); return c }

// WithHTTPClient replaces the HTTP client.
func (c *HTTP) WithHTTPClient(hc *http.Client) *HTTP { c.hc = hc; return c }

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// ErrRateLimited carries the wait Telegram asked for.
type ErrRateLimited struct{ After time.Duration }

func (e ErrRateLimited) Error() string {
	return fmt.Sprintf("telegram: rate limited, retry after %s", e.After)
}

// clean removes the URL (which contains the token) from transport errors.
func clean(method string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("telegram %s: %w", method, err)
}

// How long a call may run: a long poll gets slack beyond the wait it asked the server for, a send gets sendTimeout.
// Variables so that tests can shorten them.
var (
	slack       = 15 * time.Second
	sendTimeout = 30 * time.Second
)

func (c *HTTP) call(ctx context.Context, method string, body any, within time.Duration) (*apiResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// A connection that goes quiet (a middlebox that drops packets) must not hang the hub for ever.
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return nil, clean(method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, clean(method, err)
	}
	defer resp.Body.Close()
	var out apiResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("telegram %s: HTTP %d, unreadable answer", method, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited{After: time.Duration(out.Parameters.RetryAfter) * time.Second}
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram %s: HTTP %d: %s", method, resp.StatusCode, out.Description)
	}
	return &out, nil
}

// GetUpdates long-polls for new messages and button taps. Updates without text are skipped but still advance the offset.
func (c *HTTP) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	out, err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": int(timeout.Seconds()), "allowed_updates": []string{"message", "callback_query"}}, timeout+slack)
	if err != nil {
		return nil, err
	}
	type rawMessage struct {
		MessageID int64  `json:"message_id"`
		Date      int64  `json:"date"`
		Text      string `json:"text"`
		From      *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		ReplyTo *struct {
			MessageID int64 `json:"message_id"`
		} `json:"reply_to_message"`
	}
	var raw []struct {
		UpdateID      int64       `json:"update_id"`
		Message       *rawMessage `json:"message"`
		CallbackQuery *struct {
			ID   string `json:"id"`
			Data string `json:"data"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
			Message *rawMessage `json:"message"`
		} `json:"callback_query"`
	}
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		return nil, fmt.Errorf("telegram getUpdates: bad result")
	}
	var ups []Update
	for _, r := range raw {
		u := Update{ID: r.UpdateID}
		switch {
		case r.Message != nil:
			u.ChatID, u.MessageID, u.Text = r.Message.Chat.ID, r.Message.MessageID, r.Message.Text
			if r.Message.Date > 0 {
				u.Date = time.Unix(r.Message.Date, 0)
			}
			if r.Message.From != nil {
				u.UserID = r.Message.From.ID
			}
			if r.Message.ReplyTo != nil {
				u.ReplyToID = r.Message.ReplyTo.MessageID
			}
		case r.CallbackQuery != nil:
			cq := r.CallbackQuery
			u.UserID, u.CallbackID, u.CallbackData = cq.From.ID, cq.ID, cq.Data
			if cq.Message != nil { // Telegram omits the message when it is too old to be shown
				u.ChatID, u.MessageID = cq.Message.Chat.ID, cq.Message.MessageID
			}
		}
		ups = append(ups, u) // text may be empty: the caller still needs the ID to advance the offset
	}
	return ups, nil
}

// Chunks splits text into pieces of at most max bytes, preferring line breaks, never inside a rune.
func Chunks(text string, max int) []string {
	var out []string
	for len(text) > max {
		cut := strings.LastIndexByte(text[:max], '\n')
		if cut < max/2 {
			cut = max
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, text[:cut])
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// Send sends plain text (no formatting, so nothing needs escaping), splitting long text and honouring rate limits.
func (c *HTTP) Send(ctx context.Context, chatID int64, text string) error {
	_, err := c.SendMessage(ctx, chatID, Message{Text: text})
	return err
}

// SendMessage is Send with inline buttons under the last part. It returns the ids of the parts that were sent, also on
// error (the parts before the failing one did arrive).
func (c *HTTP) SendMessage(ctx context.Context, chatID int64, m Message) ([]int64, error) {
	var ids []int64
	parts := Chunks(m.Text, MaxMessage)
	for i, part := range parts {
		body := map[string]any{"chat_id": chatID, "text": part, "disable_web_page_preview": true}
		if i == len(parts)-1 && len(m.Buttons) > 0 {
			body["reply_markup"] = map[string]any{"inline_keyboard": keyboard(m.Buttons)}
		}
		for attempt := 0; ; attempt++ {
			out, err := c.call(ctx, "sendMessage", body, sendTimeout)
			var rl ErrRateLimited
			if errors.As(err, &rl) && attempt < 2 {
				wait := rl.After
				if wait <= 0 || wait > 30*time.Second {
					wait = 5 * time.Second
				}
				select {
				case <-time.After(wait):
					continue
				case <-ctx.Done():
					return ids, ctx.Err()
				}
			}
			if err != nil {
				return ids, err
			}
			var sent struct {
				MessageID int64 `json:"message_id"`
			}
			json.Unmarshal(out.Result, &sent) // an answer without an id just means the message cannot be told apart later
			ids = append(ids, sent.MessageID)
			break
		}
	}
	return ids, nil
}

func keyboard(rows [][]Button) [][]map[string]string {
	out := make([][]map[string]string, 0, len(rows))
	for _, row := range rows {
		r := make([]map[string]string, 0, len(row))
		for _, b := range row {
			r = append(r, map[string]string{"text": b.Text, "callback_data": b.Data})
		}
		out = append(out, r)
	}
	return out
}

// AnswerCallback acknowledges a tap on an inline button.
func (c *HTTP) AnswerCallback(ctx context.Context, callbackID, text string) error {
	body := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		body["text"] = text
	}
	_, err := c.call(ctx, "answerCallbackQuery", body, sendTimeout)
	return err
}

// SetCommands publishes the command menu of the bot. The menu is public (it is the same for every chat) and only lists
// names; the hub obeys nobody but the configured user.
func (c *HTTP) SetCommands(ctx context.Context, cmds []Command) error {
	list := make([]map[string]string, len(cmds))
	for i, cmd := range cmds {
		list[i] = map[string]string{"command": cmd.Name, "description": cmd.Description}
	}
	_, err := c.call(ctx, "setMyCommands", map[string]any{"commands": list}, sendTimeout)
	return err
}

// React marks a message with one emoji. Telegram accepts only a fixed set of emoji for this (ReactionTypeEmoji in the
// Bot API documentation) and answers REACTION_INVALID for any other.
func (c *HTTP) React(ctx context.Context, chatID, messageID int64, emoji string) error {
	_, err := c.call(ctx, "setMessageReaction", map[string]any{
		"chat_id": chatID, "message_id": messageID,
		"reaction": []map[string]string{{"type": "emoji", "emoji": emoji}},
	}, sendTimeout)
	return err
}

// Sent is a message recorded by Fake. ID is the id the fake gave it (ids are unique and increase).
type Sent struct {
	ChatID  int64
	Text    string
	ID      int64
	Buttons [][]Button
}

// Reaction is a reaction recorded by Fake.
type Reaction struct {
	ChatID, MessageID int64
	Emoji             string
}

// Fake is an in-memory Client for tests.
type Fake struct {
	mu        sync.Mutex
	queue     []Update
	sent      []Sent
	wake      chan struct{}
	sendErr   error
	menu      []Command
	menuErr   error
	reacted   []Reaction
	reactErr  error
	reactTry  int
	reactGate chan struct{}
	nextMsg   int64
	answered  []string
	refused   map[string]bool
}

// NewFake returns an empty fake.
func NewFake() *Fake { return &Fake{wake: make(chan struct{}, 1)} }

// Push queues an incoming message.
func (f *Fake) Push(u Update) {
	f.mu.Lock()
	f.queue = append(f.queue, u)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// GetUpdates returns queued updates, or waits for one until the timeout or ctx ends.
func (f *Fake) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		var out []Update
		for _, u := range f.queue {
			if u.ID >= offset {
				out = append(out, u)
			}
		}
		f.queue = out
		f.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		select {
		case <-f.wake:
		case <-deadline:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SetSendErr makes every Send fail with err until it is set to nil.
func (f *Fake) SetSendErr(err error) {
	f.mu.Lock()
	f.sendErr = err
	f.mu.Unlock()
}

// Send records a message.
func (f *Fake) Send(ctx context.Context, chatID int64, text string) error {
	_, err := f.SendMessage(ctx, chatID, Message{Text: text})
	return err
}

// SendMessage records a message and returns the id the fake gave it.
func (f *Fake) SendMessage(_ context.Context, chatID int64, m Message) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.nextMsg++
	id := 1000 + f.nextMsg
	f.sent = append(f.sent, Sent{ChatID: chatID, Text: m.Text, ID: id, Buttons: m.Buttons})
	return []int64{id}, nil
}

// AnswerCallback records the answer to a button tap.
func (f *Fake) AnswerCallback(_ context.Context, callbackID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, callbackID+": "+text)
	return nil
}

// Answered returns the answers given to button taps, as "callback id: text".
func (f *Fake) Answered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answered...)
}

// SetCommands records the menu, or fails with the error set by SetMenuErr.
func (f *Fake) SetCommands(_ context.Context, cmds []Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.menuErr != nil {
		return f.menuErr
	}
	f.menu = append([]Command(nil), cmds...)
	return nil
}

// SetMenuErr makes SetCommands fail with err until it is set to nil.
func (f *Fake) SetMenuErr(err error) {
	f.mu.Lock()
	f.menuErr = err
	f.mu.Unlock()
}

// Commands returns the menu that was published, nil before that.
func (f *Fake) Commands() []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Command(nil), f.menu...)
}

// React records a reaction, or fails with the error set by SetReactErr. While a gate is set by BlockReactions it waits
// for the gate to be closed.
func (f *Fake) React(ctx context.Context, chatID, messageID int64, emoji string) error {
	f.mu.Lock()
	f.reactTry++
	gate := f.reactGate
	refused := f.refused[emoji]
	f.mu.Unlock()
	if refused {
		return errors.New("telegram setMessageReaction: HTTP 400: Bad Request: REACTION_INVALID")
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reactErr != nil {
		return f.reactErr
	}
	f.reacted = append(f.reacted, Reaction{chatID, messageID, emoji})
	return nil
}

// RefuseEmoji makes React fail with REACTION_INVALID for one emoji, as Telegram does for an emoji it does not accept.
func (f *Fake) RefuseEmoji(emoji string) {
	f.mu.Lock()
	if f.refused == nil {
		f.refused = map[string]bool{}
	}
	f.refused[emoji] = true
	f.mu.Unlock()
}

// BlockReactions makes React wait until gate is closed (a Telegram that does not answer).
func (f *Fake) BlockReactions(gate chan struct{}) {
	f.mu.Lock()
	f.reactGate = gate
	f.mu.Unlock()
}

// ReactAttempts counts the calls to React, whether they worked or not.
func (f *Fake) ReactAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reactTry
}

// SetReactErr makes React fail with err until it is set to nil.
func (f *Fake) SetReactErr(err error) {
	f.mu.Lock()
	f.reactErr = err
	f.mu.Unlock()
}

// Reactions returns the reactions recorded so far.
func (f *Fake) Reactions() []Reaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Reaction(nil), f.reacted...)
}

// Sent returns what was sent so far.
func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}
