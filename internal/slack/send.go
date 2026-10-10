package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

// maxText is the longest text of one part of a message, in bytes. A block of text holds 3000 characters; escaping can make a
// text longer, so a part that does not fit after it is cut in two.
const (
	maxPartInput = 2800
	maxBlockText = 3000
	maxButtonRow = 5
	maxRows      = 10
	maxButtonLen = 75
)

// part is one message of Slack: the text as Slack should read it, and how to read it.
type part struct {
	text  string // escaped, or mrkdwn
	plain bool   // the text is plain: show it as it is, no formatting
	raw   string // the text before escaping, for a plain part (shown in a plain_text block)
}

// parts cuts a message into messages of Slack, each short enough for a block of text after conversion.
func parts(m telegram.Message) []part {
	var out []part
	var add func(src string, depth int)
	add = func(src string, depth int) {
		var pieces []string
		if m.Markdown {
			pieces = Split(src, maxPartInput)
		} else {
			pieces = SplitPlain(src, maxPartInput)
		}
		for _, p := range pieces {
			var pt part
			if m.Markdown {
				pt = part{text: FromMarkdown(p)}
			} else {
				pt = part{text: Escape(p), plain: true, raw: p}
			}
			if len(pt.text) > maxBlockText && len(p) > 1 && depth < 6 {
				// Escaping made it longer than a block holds: cut the source in two and convert each half.
				half := len(p) / 2
				for half > 0 && !utf8.RuneStart(p[half]) {
					half--
				}
				if half == 0 {
					half = len(p) / 2
				}
				add(p[:half], depth+1)
				add(p[half:], depth+1)
				continue
			}
			if strings.TrimSpace(pt.text) == "" {
				continue
			}
			out = append(out, pt)
		}
	}
	add(m.Text, 0)
	return out
}

// blocks builds the message blocks for a part with buttons under it.
func blocks(p part, rows [][]telegram.Button) string {
	typ, txt := "mrkdwn", p.text
	if p.plain {
		typ, txt = "plain_text", p.raw
	}
	section := map[string]any{"type": "section", "text": map[string]any{"type": typ, "text": txt}}
	bl := []any{section}
	for r, row := range rows {
		if r >= maxRows {
			break
		}
		var els []any
		for c, b := range row {
			if c >= maxButtonRow || b.Text == "" {
				continue
			}
			els = append(els, map[string]any{
				"type": "button", "action_id": "pb" + itoa(r) + "_" + itoa(c),
				"text":  map[string]any{"type": "plain_text", "text": cutRunes(b.Text, maxButtonLen), "emoji": true},
				"value": b.Data,
			})
		}
		if len(els) > 0 {
			bl = append(bl, map[string]any{"type": "actions", "elements": els})
		}
	}
	return jsonString(bl)
}

func itoa(n int) string { return string(rune('0' + n%10)) } // rows and columns are below 10

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// SendMessage writes to the allowed chat, in as many messages as the text needs, and returns the ids of the messages. Buttons go
// under the last one. A message goes out whole before the next starts.
func (c *Client) SendMessage(ctx context.Context, chatID int64, m telegram.Message) ([]int64, error) {
	ch, err := c.chat(chatID)
	if err != nil {
		return nil, err
	}
	ps := parts(m)
	if len(ps) == 0 {
		if hasButtons(m.Buttons) {
			ps = []part{{text: "·", plain: true, raw: "·"}}
		} else {
			return nil, nil
		}
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	var ids []int64
	for i, p := range ps {
		params := url.Values{
			"channel":      {ch},
			"text":         {p.text},
			"mrkdwn":       {boolStr(!p.plain)},
			"unfurl_links": {"false"}, "unfurl_media": {"false"},
			"link_names": {"false"},
		}
		if i == len(ps)-1 && hasButtons(m.Buttons) {
			params.Set("blocks", blocks(p, m.Buttons))
		}
		var out struct {
			reply
			TS string `json:"ts"`
		}
		if err := c.callRetry(ctx, "chat.postMessage", c.cfg.BotToken, params, &out); err != nil {
			return ids, err
		}
		if id, ok := tsToID(out.TS); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func hasButtons(rows [][]telegram.Button) bool {
	for _, r := range rows {
		for _, b := range r {
			if b.Text != "" {
				return true
			}
		}
	}
	return false
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Send is SendMessage for a plain text.
func (c *Client) Send(ctx context.Context, chatID int64, text string) error {
	_, err := c.SendMessage(ctx, chatID, telegram.Message{Text: text})
	return err
}

// ClearButtons takes the buttons off a message the client sent: the message is read back, the blocks of buttons are dropped
// and the rest is written again.
func (c *Client) ClearButtons(ctx context.Context, chatID, messageID int64) error {
	ch, err := c.chat(chatID)
	if err != nil {
		return err
	}
	ts, ok := idToTS(messageID)
	if !ok {
		return nil
	}
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	var h struct {
		reply
		Messages []struct {
			TS     string            `json:"ts"`
			Text   string            `json:"text"`
			Blocks []json.RawMessage `json:"blocks"`
		} `json:"messages"`
	}
	err = c.callRetry(ctx, "conversations.history", c.cfg.BotToken, url.Values{"channel": {ch}, "latest": {ts}, "inclusive": {"true"}, "limit": {"1"}}, &h)
	if err != nil {
		return err
	}
	for _, msg := range h.Messages {
		if msg.TS != ts {
			continue
		}
		kept := []json.RawMessage{}
		for _, b := range msg.Blocks {
			var t struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(b, &t) == nil && t.Type != "actions" {
				kept = append(kept, b)
			}
		}
		if len(kept) == len(msg.Blocks) {
			return nil // no buttons on it
		}
		text := msg.Text
		if strings.TrimSpace(text) == "" {
			text = "·"
		}
		return c.callRetry(ctx, "chat.update", c.cfg.BotToken, url.Values{"channel": {ch}, "ts": {ts}, "text": {text}, "blocks": {jsonString(kept)}}, nil)
	}
	return nil
}

// React puts an emoji on a message as a mark. Like Telegram's reaction it replaces the mark this client put before on the same
// message: the new one is added first, then the old one removed.
func (c *Client) React(ctx context.Context, chatID, messageID int64, emoji string) error {
	ch, err := c.chat(chatID)
	if err != nil {
		return err
	}
	name, ok := ReactionName(emoji)
	if !ok {
		return &telegram.APIError{Method: "reactions.add", Code: 400, Desc: "REACTION_INVALID: Slack has no name for " + safeWord(emoji)}
	}
	ts, ok := idToTS(messageID)
	if !ok {
		return &telegram.APIError{Method: "reactions.add", Code: 400, Desc: "message_not_found"}
	}
	err = c.callRetry(ctx, "reactions.add", c.cfg.BotToken, url.Values{"channel": {ch}, "timestamp": {ts}, "name": {name}}, nil)
	var ae *telegram.APIError
	if err != nil {
		if errors.As(err, &ae) && strings.Contains(ae.Desc, "already_reacted") {
			err = nil
		} else {
			if errors.As(err, &ae) && strings.Contains(ae.Desc, "invalid_name") {
				return &telegram.APIError{Method: "reactions.add", Code: 400, Desc: "REACTION_INVALID: " + name}
			}
			return err
		}
	}
	c.mu.Lock()
	old, had := c.reacted[messageID]
	c.reacted[messageID] = name
	if !had {
		c.reactRng = append(c.reactRng, messageID)
		if len(c.reactRng) > 256 {
			delete(c.reacted, c.reactRng[0])
			c.reactRng = c.reactRng[1:]
		}
	}
	c.mu.Unlock()
	if had && old != name {
		// A mark that is already gone is no failure.
		c.callRetry(ctx, "reactions.remove", c.cfg.BotToken, url.Values{"channel": {ch}, "timestamp": {ts}, "name": {old}}, nil)
	}
	return nil
}

var (
	_ telegram.Client       = (*Client)(nil)
	_ telegram.Reactor      = (*Client)(nil)
	_ telegram.MarkupEditor = (*Client)(nil)
)
