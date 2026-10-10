package slack

import (
	"context"
	"net/url"
	"time"
)

// Slack keeps no events for a connection that is not there: a message written while the socket was closed (it is closed and
// opened again every few hours at Slack's request, and whenever the network blinks) is never delivered. So when the socket is
// open again, the history of the direct message is read from the last message this client heard of, and what the user wrote since
// is handled as if it had come as an event. The checks are the same ones, and a message that was heard already is dropped by its
// id, so nothing runs twice.
//
// This covers the gaps within a run of the hub. A hub that was started anew does not know the last message and reads nothing back:
// what the user wrote while it was down is lost, where Telegram would have kept it. (Replaying old messages after a restart would
// be worse: a command could run twice.)

// backfillTimeout bounds the reading back, so that a slow history cannot keep the socket from being read.
const backfillTimeout = 10 * time.Second

// heard notes the newest message of the allowed user that this client has seen.
func (c *Client) heard(id int64, ts string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := tsToID(c.lastTS); !ok || id > last {
		c.lastTS = ts
	}
}

// backfill reads the messages written after the last one heard of.
func (c *Client) backfill(parent context.Context) {
	c.mu.Lock()
	ch, last := c.channel, c.lastTS
	c.mu.Unlock()
	if ch == "" || last == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, backfillTimeout)
	defer cancel()
	var h struct {
		reply
		Messages []messageEvent `json:"messages"` // the newest first
	}
	params := url.Values{"channel": {ch}, "oldest": {last}, "inclusive": {"false"}, "limit": {"100"}}
	if err := c.callRetry(ctx, "conversations.history", c.cfg.BotToken, params, &h); err != nil {
		if parent.Err() == nil {
			c.log.Warn("could not read back what was written while the socket was closed", "err", err)
		}
		return
	}
	n := 0
	for i := len(h.Messages) - 1; i >= 0; i-- {
		m := h.Messages[i]
		m.Type, m.Channel, m.ChannelType = "message", ch, "im"
		c.message(m, "")
		n++
	}
	if n > 0 {
		c.log.Info("read back messages written while the socket was closed", "messages", n)
	}
}
