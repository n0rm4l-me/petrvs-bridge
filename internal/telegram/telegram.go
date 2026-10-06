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

// Update is one incoming text message.
type Update struct {
	ID        int64
	ChatID    int64
	UserID    int64
	MessageID int64
	Text      string
	Date      time.Time // when Telegram received the message; zero when unknown
}

// Client is what the hub needs from Telegram.
type Client interface {
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	Send(ctx context.Context, chatID int64, text string) error
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

// slack is how long a call may run beyond the wait it asked the server for.
const slack = 15 * time.Second

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

// GetUpdates long-polls for new text messages. Updates without text are skipped but still advance the offset.
func (c *HTTP) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	out, err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": int(timeout.Seconds()), "allowed_updates": []string{"message"}}, timeout+slack)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		UpdateID int64 `json:"update_id"`
		Message  *struct {
			MessageID int64  `json:"message_id"`
			Date      int64  `json:"date"`
			Text      string `json:"text"`
			From      *struct {
				ID int64 `json:"id"`
			} `json:"from"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		return nil, fmt.Errorf("telegram getUpdates: bad result")
	}
	var ups []Update
	for _, r := range raw {
		u := Update{ID: r.UpdateID}
		if r.Message != nil {
			u.ChatID, u.MessageID, u.Text = r.Message.Chat.ID, r.Message.MessageID, r.Message.Text
			if r.Message.Date > 0 {
				u.Date = time.Unix(r.Message.Date, 0)
			}
			if r.Message.From != nil {
				u.UserID = r.Message.From.ID
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
	for _, part := range Chunks(text, MaxMessage) {
		for attempt := 0; ; attempt++ {
			_, err := c.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": part, "disable_web_page_preview": true}, 30*time.Second)
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
					return ctx.Err()
				}
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// Sent is a message recorded by Fake.
type Sent struct {
	ChatID int64
	Text   string
}

// Fake is an in-memory Client for tests.
type Fake struct {
	mu      sync.Mutex
	queue   []Update
	sent    []Sent
	wake    chan struct{}
	sendErr error
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
func (f *Fake) Send(_ context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, Sent{chatID, text})
	return nil
}

// Sent returns what was sent so far.
func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}
