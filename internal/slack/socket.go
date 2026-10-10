package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

// errReconnect is Slack's request to open the connection again (it refreshes connections now and then).
var errReconnect = errors.New("slack: asked to reconnect")

// maxFrame bounds one message of the socket. Slack's events are a few kilobytes; a megabyte leaves room for a message with many
// files and stops a peer that sends more.
const maxFrame = 1 << 20

// socketMsg is one message of Socket Mode.
type socketMsg struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	Reason     string          `json:"reason"`
}

// session opens one Socket Mode connection and reads it until it ends. The address that Slack gives for it carries a one-time
// ticket, so it is kept out of every error and log line.
func (c *Client) session(ctx context.Context) error {
	var open struct {
		reply
		URL string `json:"url"`
	}
	if err := c.callRetry(ctx, "apps.connections.open", c.cfg.AppToken, nil, &open); err != nil {
		return err
	}
	u, ok := c.trustedSocket(open.URL)
	if !ok {
		return errors.New("slack: apps.connections.open gave an address that is not a Slack socket")
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, _, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{HTTPClient: c.api.hc})
	cancel()
	if err != nil {
		return clean("socket", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxFrame)

	hello, err := readWithin(ctx, conn, 20*time.Second)
	if err != nil {
		return err
	}
	if hello.Type != "hello" {
		return fmt.Errorf("slack: the socket opened with %q and not hello", safeWord(hello.Type))
	}
	c.up()
	c.connectedNow.Store(true)
	c.everUp.Store(true)
	select {
	case c.hello <- struct{}{}:
	default:
	}
	c.log.Info("connected to Slack (Socket Mode)")
	// What the user wrote while there was no socket is read back before the socket is read, so that it comes in order and
	// before anything newer.
	c.backfill(ctx)

	pctx, stop := context.WithCancel(ctx)
	defer stop()
	go c.pinger(pctx, conn)
	for {
		m, err := readMsg(ctx, conn)
		if err != nil {
			if ctx.Err() != nil {
				conn.Close(websocket.StatusNormalClosure, "")
				return ctx.Err()
			}
			return clean("socket", err)
		}
		c.up()
		if err := c.handle(ctx, conn, m); err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			return err
		}
	}
}

func (c *Client) up() { c.lastUp.Store(c.cfg.Now().UnixNano()) }

// pinger pings the socket, so that a connection that went silent (a middlebox that dropped it) is noticed and closed instead of
// waiting for ever.
func (c *Client) pinger(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(c.cfg.PingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pc, cancel := context.WithTimeout(ctx, c.cfg.PingEvery/2+time.Second)
			err := conn.Ping(pc)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					conn.CloseNow()
				}
				return
			}
			c.up()
		}
	}
}

func readMsg(ctx context.Context, conn *websocket.Conn) (socketMsg, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return socketMsg{}, err
	}
	if typ != websocket.MessageText {
		return socketMsg{}, nil // not ours; ignored
	}
	var m socketMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return socketMsg{}, nil // a message that is not JSON is ignored, not fatal
	}
	return m, nil
}

func readWithin(ctx context.Context, conn *websocket.Conn, d time.Duration) (socketMsg, error) {
	rc, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	m, err := readMsg(rc, conn)
	if err != nil {
		return socketMsg{}, clean("socket", err)
	}
	return m, nil
}

// handle deals with one message of the socket. Every envelope is acknowledged first, at once: Slack sends it again when the
// acknowledgement is late (three seconds), and the repeat is dropped by the id. The price is that an event is not handled twice
// but can be lost if the hub dies between the acknowledgement and the hub's work on it, which the user sees as a message
// without a mark and sends again.
func (c *Client) handle(ctx context.Context, conn *websocket.Conn, m socketMsg) error {
	switch m.Type {
	case "disconnect":
		return errReconnect
	case "events_api":
		c.ack(ctx, conn, m.EnvelopeID, nil)
		c.onEvent(m.Payload)
	case "interactive":
		c.ack(ctx, conn, m.EnvelopeID, nil)
		c.onInteractive(m.Payload)
	case "slash_commands":
		resp := c.onSlash(m.Payload)
		c.ack(ctx, conn, m.EnvelopeID, resp)
	}
	return nil
}

// ack answers an envelope. payload, if any, is shown to the user who typed a slash command.
func (c *Client) ack(ctx context.Context, conn *websocket.Conn, id string, payload any) {
	if id == "" || len(id) > 200 {
		return
	}
	body := map[string]any{"envelope_id": id}
	if payload != nil {
		body["payload"] = payload
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, raw); err != nil && ctx.Err() == nil {
		c.log.Warn("could not acknowledge an event to Slack", "err", clean("socket", err))
	}
}

// parseQuery is a tiny helper for tests that look at an address.
func parseQuery(raw string) url.Values {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u.Query()
}
