package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slackid"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

// Config is what the client needs. The two tokens are secrets: they are only ever sent in an Authorization header, to Slack.
type Config struct {
	BotToken string // xoxb-..., the bot user's token
	AppToken string // xapp-..., the app-level token with connections:write, for Socket Mode
	UserID   string // the only user that is obeyed (U... or W...)
	// ChannelID is the direct message of that user with the bot (D...). Empty means: ask Slack for it at the start, which needs
	// the im:write right.
	ChannelID string
	// CommandPrefix is put before the name of every slash command of the app (default "pb-"): the names of commands are shared by
	// the whole workspace, and some belong to Slack itself (/status, /help). "/pb-status" reaches the hub as "/status".
	CommandPrefix string
	// APIBase is where the Web API is; the default is Slack's. Plain http is accepted only for a loopback address (tests).
	APIBase    string
	HTTPClient *http.Client
	Log        *slog.Logger
	Now        func() time.Time

	// Waits, shortened by tests.
	ReconnectMin, ReconnectMax time.Duration
	DownAfter                  time.Duration // how long without a socket before GetUpdates reports an error (default 60s)
	MaxRetryWait               time.Duration // the longest rate limit a send waits out itself (default 30s)
	PingEvery                  time.Duration // default 30s
}

// Client is the hub's connection to Slack. It implements telegram.Client and the optional parts the hub uses (reactions, files,
// removing buttons).
type Client struct {
	cfg  Config
	log  *slog.Logger
	api  *apiClient
	loop bool // the API is on a loopback address: the other addresses Slack hands out may then be loopback too
	base *url.URL

	mu       sync.Mutex
	channel  string // the allowed direct message; empty until known
	lastTS   string // the ts of the newest message of the allowed user that was heard (see backfill)
	botUser  string
	team     string
	started  time.Time
	upQueue  []telegram.Update
	nextID   int64
	notify   chan struct{}
	seen     map[string]struct{}
	seenRing []string
	respURLs map[string]string // callback id -> response_url, for AnswerCallback
	respRing []string
	reacted  map[int64]string // message id -> the emoji this client put last
	reactRng []int64

	// connected is the time the socket last said hello, or was last known to be up; connectedNow says that it is up now.
	connectedNow atomic.Bool
	everUp       atomic.Bool   // the socket has said hello at least once
	hello        chan struct{} // told on every hello, for a GetUpdates that waits for the first one
	lastUp       atomic.Int64
	sending      chan struct{}
	strangers    atomic.Uint64
}

// HelpNote is the line the hub adds to its list of commands in Slack, where a command is a slash command of the app.
func HelpNote(prefix string) string {
	return "In Slack every command is a slash command of the app with the prefix " + prefix + ", for example /" + prefix + "status or /" + prefix + "key 2."
}

// ValidAPIBase says that s can be the address of the Web API: https on slack.com (the tokens go there), or plain http to a loopback
// address (tests).
func ValidAPIBase(s string) bool { return slackid.ValidAPIBase(s) }

// ValidCommandPrefix says that s can be put before the names of the slash commands.
func ValidCommandPrefix(s string) bool { return slackid.ValidCommandPrefix(s) }

// New checks the settings and returns a client. It starts nothing: Run does.
func New(cfg Config) (*Client, error) {
	switch {
	case !ValidBotToken(cfg.BotToken):
		return nil, errors.New("slack: the bot token must look like xoxb-...")
	case !ValidAppToken(cfg.AppToken):
		return nil, errors.New("slack: the app-level token must look like xapp-...")
	case !ValidUserID(cfg.UserID):
		return nil, errors.New("slack: the user id must look like U0123ABCD")
	case cfg.ChannelID != "" && !ValidChannelID(cfg.ChannelID):
		return nil, errors.New("slack: the channel id must be the direct message, D0123ABCD")
	}
	if cfg.CommandPrefix == "" {
		cfg.CommandPrefix = "pb-"
	}
	if !ValidCommandPrefix(cfg.CommandPrefix) {
		return nil, errors.New("slack: the command prefix is lower-case letters, digits, - and _, at most 10 characters")
	}
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	if !ValidAPIBase(cfg.APIBase) {
		return nil, errors.New("slack: the API address must be https on slack.com (plain http only for localhost, for tests)")
	}
	base, _ := url.Parse(strings.TrimRight(cfg.APIBase, "/"))
	loop := base.Scheme == "http"
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.ReconnectMin, time.Second)
	def(&cfg.ReconnectMax, 30*time.Second)
	def(&cfg.DownAfter, 60*time.Second)
	def(&cfg.MaxRetryWait, 30*time.Second)
	def(&cfg.PingEvery, 30*time.Second)
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: newTransport(), CheckRedirect: sameHostOnly}
	}
	c := &Client{
		cfg: cfg, log: cfg.Log.With("chat", "slack"), api: &apiClient{base: strings.TrimRight(cfg.APIBase, "/"), hc: hc},
		loop: loop, base: base, started: cfg.Now(), notify: make(chan struct{}, 1), hello: make(chan struct{}, 1), nextID: 1,
		seen: map[string]struct{}{}, respURLs: map[string]string{}, reacted: map[int64]string{}, sending: make(chan struct{}, 1),
	}
	c.lastUp.Store(cfg.Now().UnixNano())
	return c, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// newTransport is the default transport with HTTP/2 health checks, so that a connection that went silent is closed and not reused.
func newTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
	return t
}

// sameHostOnly refuses a redirect to another host: a token (or a file) must not follow a link to a place Slack did not name.
func sameHostOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
		return errors.New("redirect to another host refused")
	}
	return nil
}

// trustedHTTP says whether an address that Slack handed out (an upload, a download, a response) may be used: https, on one of the
// hosts given (a name, or a suffix that starts with a dot), no credentials in it, the usual port. Plain http is never trusted,
// since the bot token goes with a download. For a client on a loopback API (tests) the same host and port as the API.
func (c *Client) trustedHTTP(raw string, hosts ...string) (*url.URL, bool) {
	return c.trusted(raw, []string{"https"}, []string{"http", "https"}, hosts)
}

// trustedSocket is trustedHTTP for the address of the WebSocket (wss on a host of slack.com).
func (c *Client) trustedSocket(raw string) (*url.URL, bool) {
	return c.trusted(raw, []string{"wss"}, []string{"ws", "wss"}, []string{".slack.com"})
}

func (c *Client) trusted(raw string, schemes, loopSchemes, hosts []string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, false
	}
	if c.loop {
		return u, slices.Contains(loopSchemes, u.Scheme) && u.Host == c.base.Host
	}
	if !slices.Contains(schemes, u.Scheme) || (u.Port() != "" && u.Port() != "443") {
		return nil, false
	}
	h := strings.ToLower(u.Hostname())
	for _, want := range hosts {
		if h == want || (strings.HasPrefix(want, ".") && strings.HasSuffix(h, want)) {
			return u, true
		}
	}
	return nil, false
}

// DMChannel is the allowed direct message once it is known.
func (c *Client) DMChannel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channel
}

// Strangers counts the messages and taps of people who are not the allowed user (they are not obeyed).
func (c *Client) Strangers() uint64 { return c.strangers.Load() }

// Run keeps the client alive until ctx ends: it asks Slack who the bot is, finds the direct message, and holds the Socket Mode
// connection, opening it again when it drops or Slack asks for it. Run it in a goroutine; GetUpdates reads what it collects.
func (c *Client) Run(ctx context.Context) {
	wait := c.cfg.ReconnectMin
	for ctx.Err() == nil {
		if err := c.start(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("cannot start the connection to Slack", "err", err, "retry_in", wait)
			if !sleep(ctx, wait) {
				return
			}
			wait = min(wait*2, c.cfg.ReconnectMax)
			continue
		}
		break
	}
	wait = c.cfg.ReconnectMin
	for ctx.Err() == nil {
		began := c.cfg.Now()
		err := c.session(ctx)
		c.connectedNow.Store(false)
		if ctx.Err() != nil {
			return
		}
		lasted := c.cfg.Now().Sub(began) > 30*time.Second
		if lasted {
			wait = c.cfg.ReconnectMin // it worked for a while: the next failure is a new one
		}
		if err != nil && !errors.Is(err, errReconnect) {
			c.log.Warn("the connection to Slack ended", "err", err, "retry_in", wait)
		} else {
			// Slack asks for a new connection every few hours, and that is no failure: go back at once. But a socket that was closed at
			// once, over and over, is one: it gets the waits of any failure, so that a Slack that keeps asking is not asked back in a loop.
			c.log.Info("Slack asked for a new connection")
			if lasted {
				wait = 0
			}
		}
		if !sleep(ctx, wait) {
			return
		}
		wait = min(max(wait*2, c.cfg.ReconnectMin), c.cfg.ReconnectMax)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// start learns who the bot is and which direct message is the allowed one.
func (c *Client) start(ctx context.Context) error {
	var who struct {
		reply
		UserID string `json:"user_id"`
		TeamID string `json:"team_id"`
		BotID  string `json:"bot_id"`
	}
	if err := c.callRetry(ctx, "auth.test", c.cfg.BotToken, nil, &who); err != nil {
		return err
	}
	if !ValidUserID(who.UserID) {
		return errors.New("slack: auth.test did not name the bot user")
	}
	c.mu.Lock()
	c.botUser, c.team = who.UserID, who.TeamID
	c.mu.Unlock()
	// Slack is asked for the direct message even when it is configured: a channel id that is not the one with this user would
	// send what the agent writes to someone else's conversation. (The answer is the same every time; opening it again is free.)
	var open struct {
		reply
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	err := c.callRetry(ctx, "conversations.open", c.cfg.BotToken, url.Values{"users": {c.cfg.UserID}}, &open)
	if err == nil {
		if !ValidChannelID(open.Channel.ID) {
			return errors.New("slack: conversations.open did not name a direct message")
		}
		if c.cfg.ChannelID != "" && c.cfg.ChannelID != open.Channel.ID {
			// Permanent: the settings are wrong. Say so and do not use the channel.
			return errors.New("slack: the configured channel is not the direct message with the configured user (" + safeWord(c.cfg.ChannelID) + " is not what Slack names for them)")
		}
		c.mu.Lock()
		c.channel = open.Channel.ID
		c.mu.Unlock()
		return nil
	}
	return err
}

// callRetry is call for the bot, waiting out a short rate limit.
func (c *Client) callRetry(ctx context.Context, method, token string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	waited := time.Duration(0)
	for {
		err := c.api.call(ctx, method, token, params, out)
		var rl telegram.ErrRateLimited
		if !errors.As(err, &rl) {
			return err
		}
		after := max(rl.After, time.Second)
		if waited+after > c.cfg.MaxRetryWait {
			return err
		}
		waited += after
		if !sleep(ctx, after) {
			return ctx.Err()
		}
	}
}

// acquire takes the one slot for writing to the chat: a message goes out whole before the next starts, so that messages
// written at the same moment do not interleave.
func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.sending <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) release() { <-c.sending }

var errNeverUp = errors.New("slack: no Socket Mode connection yet")

var errNotReady = &telegram.APIError{Method: "slack", Code: 503, Desc: "not connected to Slack yet"}

// chat checks that the hub means the allowed chat and gives the channel to write to.
func (c *Client) chat(chatID int64) (string, error) {
	if chatID != ChatID {
		return "", &telegram.APIError{Method: "slack", Code: 400, Desc: "unknown chat"}
	}
	ch := c.DMChannel()
	if ch == "" {
		return "", errNotReady
	}
	return ch, nil
}

// ---- reading ----

// maxQueue bounds what waits for the hub. Messages of the allowed user are always kept; those of strangers are dropped first.
const (
	maxStrangerQueue = 64
	maxQueue         = 4096
)

// push puts an update in the queue of the hub unless it repeats one already seen. key identifies the event (the same event can
// arrive again when an acknowledgement was late).
func (c *Client) push(key string, u telegram.Update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Only the allowed user's events are remembered: a flood of strangers must not push the user's keys out of the ring and
	// let an event that Slack sends again be handled twice.
	if key != "" && u.ChatID == ChatID && u.UserID == UserID {
		if _, dup := c.seen[key]; dup {
			return
		}
		c.seen[key] = struct{}{}
		c.seenRing = append(c.seenRing, key)
		if len(c.seenRing) > 2048 {
			delete(c.seen, c.seenRing[0])
			c.seenRing = c.seenRing[1:]
		}
	}
	if u.ChatID != ChatID || u.UserID != UserID {
		c.strangers.Add(1)
		n := 0
		for _, q := range c.upQueue {
			if q.ChatID != ChatID || q.UserID != UserID {
				n++
			}
		}
		if n >= maxStrangerQueue {
			return
		}
	} else if len(c.upQueue) >= maxQueue {
		c.upQueue = c.upQueue[1:] // far beyond anything one person types: the oldest goes, loudly
		c.log.Error("the queue of messages for the hub overflowed; the oldest was dropped")
	}
	u.ID = c.nextID
	c.nextID++
	c.upQueue = append(c.upQueue, u)
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// GetUpdates gives the updates with an id of at least offset, waiting up to timeout for one; those below offset are forgotten
// (the hub has handled them). It reports an error when Slack has been out of reach for too long, so that the hub knows.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]telegram.Update, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		keep := c.upQueue[:0]
		for _, u := range c.upQueue {
			if u.ID >= offset {
				keep = append(keep, u)
			}
		}
		c.upQueue = keep
		var out []telegram.Update
		if len(keep) > 0 {
			out = append(out, keep[:min(len(keep), 100)]...)
		}
		c.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		if err := c.down(); err != nil {
			return nil, err
		}
		select {
		case <-c.notify:
		case <-c.hello:
		case <-deadline.C:
			if !c.everUp.Load() && timeout > 0 {
				// Not one hello yet: a hub with a wrong token, or no way to Slack, must not look ready (as one that polls
				// Telegram with a bad token does not).
				return nil, errNeverUp
			}
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// down is an error when the socket has been out of reach longer than DownAfter.
func (c *Client) down() error {
	if c.connectedNow.Load() {
		return nil
	}
	since := time.Unix(0, c.lastUp.Load())
	if gap := c.cfg.Now().Sub(since); gap > c.cfg.DownAfter {
		return fmt.Errorf("slack: no Socket Mode connection for %s", gap.Truncate(time.Second))
	}
	return nil
}

// ---- small shared pieces ----

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
