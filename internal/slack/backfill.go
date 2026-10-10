package slack

import (
	"context"
	"net/url"
	"strconv"
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

// backfillTimeout bounds the reading back, so that a slow history cannot keep the socket from being read. backfillPage is how many
// messages one call asks for, and backfillPages how many calls are made at most; a variable so that a test can make it small.
const (
	backfillTimeout = 10 * time.Second
	backfillPages   = 5
)

var backfillPage = 100

// What this does not read: a reply in a thread is not in the history of the channel (the history holds the messages that are not
// replies), so a reply written to an older thread while the socket was closed is lost, and so is one written to a newer one that
// was not also sent to the channel. A reply that was also sent to the channel is read as any message.

// heard notes the newest message of the allowed user that this client has seen.
func (c *Client) heard(id int64, ts string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := tsToID(c.lastTS); !ok || id > last {
		c.lastTS = ts
	}
}

// backfill reads the messages written after the last one heard of: as many pages as it takes, up to backfillPages, newest first, and
// then hands them on oldest first.
func (c *Client) backfill(parent context.Context) {
	c.mu.Lock()
	ch, last := c.channel, c.lastTS
	c.mu.Unlock()
	if ch == "" || last == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, backfillTimeout)
	defer cancel()
	var all []messageEvent // the newest first
	cursor, more := "", false
	for page := 0; page < backfillPages; page++ {
		var h struct {
			reply
			Messages []messageEvent `json:"messages"`
			HasMore  bool           `json:"has_more"`
			Meta     struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		params := url.Values{"channel": {ch}, "oldest": {last}, "inclusive": {"false"}, "limit": {strconv.Itoa(backfillPage)}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		if err := c.callRetry(ctx, "conversations.history", c.cfg.BotToken, params, &h); err != nil {
			// All or nothing: the newest of the messages without the older ones they follow would run without their context, and
			// nobody would know. Nothing is handled, so none of them gets the mark of the machine and the user sends them again.
			if parent.Err() == nil {
				c.log.Error("could not read back what was written while the socket was closed; none of it is handled, send it again", "err", err)
			}
			return
		}
		all = append(all, h.Messages...)
		cursor, more = h.Meta.Next, h.HasMore
		if !more || cursor == "" {
			more = false
			break
		}
	}
	if more {
		c.log.Error("more was written while the socket was closed than could be read back; none of it is handled, send it again", "read", len(all))
		return
	}
	for i := len(all) - 1; i >= 0; i-- {
		m := all[i]
		m.Type, m.Channel, m.ChannelType = "message", ch, "im"
		c.message(m, "")
	}
	if len(all) > 0 {
		c.log.Info("read back messages written while the socket was closed", "messages", len(all))
	}
}
