package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

// What the client lets through: one user, in the direct message with the bot, in the workspace the bot belongs to. Everything
// else is a stranger. A stranger is counted and shown to the hub as a message from another chat and another user, which the
// hub's own check refuses and counts; it is never obeyed.

type slackFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
}

type messageEvent struct {
	Type        string      `json:"type"`
	Subtype     string      `json:"subtype"`
	Channel     string      `json:"channel"`
	ChannelType string      `json:"channel_type"`
	User        string      `json:"user"`
	Team        string      `json:"team"`
	BotID       string      `json:"bot_id"`
	Text        string      `json:"text"`
	TS          string      `json:"ts"`
	ThreadTS    string      `json:"thread_ts"`
	Files       []slackFile `json:"files"`
}

// who classifies the origin of an event: the numbers the hub is shown, and whether it is the allowed user in the allowed chat.
func (c *Client) who(channel, user, team string) (chatID, userID int64, allowed bool) {
	c.mu.Lock()
	ch, mine := c.channel, c.team
	c.mu.Unlock()
	if team != "" && mine != "" && team != mine {
		return otherID(channel), otherID(user), false // another workspace (Slack Connect): never the allowed user
	}
	chatID, userID = otherID(channel), otherID(user)
	if ch != "" && channel == ch {
		chatID = ChatID
	}
	if user == c.cfg.UserID {
		userID = UserID
	}
	return chatID, userID, chatID == ChatID && userID == UserID
}

// maxFilesPerMessage bounds how many files of one message become updates.
const maxFilesPerMessage = 10

func (c *Client) onEvent(payload json.RawMessage) {
	var p struct {
		TeamID  string       `json:"team_id"`
		EventID string       `json:"event_id"`
		Event   messageEvent `json:"event"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	team := p.Event.Team
	if team == "" {
		team = p.TeamID
	}
	c.message(p.Event, team)
}

// message deals with a message that was written in a direct message: one that came as an event, or one read back from the
// history after a gap (backfill). Both go through the same checks, and a message that comes twice is handled once.
func (c *Client) message(ev messageEvent, team string) {
	if ev.Type != "message" || ev.ChannelType != "im" || ev.BotID != "" || !ValidUserID(ev.User) || !anyChannelRe.MatchString(ev.Channel) {
		return
	}
	c.mu.Lock()
	bot := c.botUser
	c.mu.Unlock()
	if ev.User == bot {
		return
	}
	// Only a new message of a person: not an edit, a deletion, a join, a bot's message or a reminder. A file, and a reply in a
	// thread that was also sent to the channel, are the user's own words.
	if ev.Subtype != "" && ev.Subtype != "file_share" && ev.Subtype != "thread_broadcast" {
		return
	}
	id, ok := tsToID(ev.TS)
	if !ok {
		return
	}
	chatID, userID, allowed := c.who(ev.Channel, ev.User, team)
	base := telegram.Update{ChatID: chatID, UserID: userID, MessageID: id, Date: tsTime(ev.TS)}
	if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		if t, ok := tsToID(ev.ThreadTS); ok {
			base.ReplyToID = t
		}
	}
	text := cleanText(decodeText(ev.Text))
	key := "m:" + ev.Channel + ":" + ev.TS
	if allowed {
		c.heard(id, ev.TS)
	}
	if !allowed {
		// Nothing of a stranger's text is kept: the hub only needs to count the message.
		base.Text = "-"
		c.push(key, base)
		return
	}
	sent := false
	for i, f := range ev.Files {
		if i >= maxFilesPerMessage {
			break
		}
		if !fileIDRe.MatchString(f.ID) || (f.Mode != "hosted" && f.Mode != "snippet" && f.Mode != "") {
			continue
		}
		u := base
		u.Media = &telegram.FileRef{ID: f.ID, Name: cut(f.Name, 512), Mime: cut(f.Mimetype, 128), Size: max(f.Size, 0)}
		if !sent {
			u.Text = text // the first file carries the words that came with it
		}
		sent = true
		c.push(key+":"+f.ID, u)
	}
	if !sent && text != "" {
		base.Text = text
		c.push(key, base)
	}
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // not in the middle of a character
		n--
	}
	return s[:n]
}

func (c *Client) onInteractive(payload json.RawMessage) {
	var p struct {
		Type string `json:"type"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Container struct {
			ChannelID string `json:"channel_id"`
			MessageTS string `json:"message_ts"`
		} `json:"container"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
		Actions []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"actions"`
		TriggerID   string `json:"trigger_id"`
		ResponseURL string `json:"response_url"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Type != "block_actions" || len(p.Actions) == 0 || p.Actions[0].Type != "button" {
		return
	}
	channel := p.Channel.ID
	if channel == "" {
		channel = p.Container.ChannelID
	}
	if !triggerRe.MatchString(p.TriggerID) || !anyChannelRe.MatchString(channel) || !ValidUserID(p.User.ID) {
		return
	}
	ts := p.Message.TS
	if ts == "" {
		ts = p.Container.MessageTS
	}
	mid, _ := tsToID(ts)
	chatID, userID, allowed := c.who(channel, p.User.ID, p.Team.ID)
	u := telegram.Update{ChatID: chatID, UserID: userID, MessageID: mid, CallbackID: p.TriggerID, CallbackData: cut(p.Actions[0].Value, 2000), Date: c.cfg.Now()}
	if !allowed {
		u.CallbackData = "-"
	} else if _, ok := c.trustedHTTP(p.ResponseURL, "hooks.slack.com"); ok {
		c.mu.Lock()
		c.respURLs[p.TriggerID] = p.ResponseURL
		c.respRing = append(c.respRing, p.TriggerID)
		if len(c.respRing) > 64 {
			delete(c.respURLs, c.respRing[0])
			c.respRing = c.respRing[1:]
		}
		c.mu.Unlock()
	}
	c.push("t:"+p.TriggerID, u)
}

var slashNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// onSlash turns a slash command of the app into the text command the hub knows ("/pb-key 3" becomes "/key 3") and returns
// what Slack shows to the person who typed it, if anything.
func (c *Client) onSlash(payload json.RawMessage) any {
	var p struct {
		Command   string `json:"command"`
		Text      string `json:"text"`
		UserID    string `json:"user_id"`
		ChannelID string `json:"channel_id"`
		TeamID    string `json:"team_id"`
		TriggerID string `json:"trigger_id"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	name := strings.TrimPrefix(strings.ToLower(p.Command), "/")
	if !strings.HasPrefix(name, c.cfg.CommandPrefix) {
		return nil
	}
	name = strings.TrimPrefix(name, c.cfg.CommandPrefix)
	if !slashNameRe.MatchString(name) {
		return nil
	}
	if !triggerRe.MatchString(p.TriggerID) || !anyChannelRe.MatchString(p.ChannelID) || !ValidUserID(p.UserID) {
		return nil
	}
	chatID, userID, allowed := c.who(p.ChannelID, p.UserID, p.TeamID)
	if userID != UserID {
		c.push("t:"+p.TriggerID, telegram.Update{ChatID: chatID, UserID: userID, Text: "-", Date: c.cfg.Now()})
		return ephemeral("This command is not available to you.")
	}
	if !allowed {
		c.strangers.Add(1)
		return ephemeral("Use this command in the direct message with the app.")
	}
	text := "/" + name
	if arg := strings.TrimSpace(cleanText(p.Text)); arg != "" {
		text += " " + arg
	}
	c.push("t:"+p.TriggerID, telegram.Update{ChatID: ChatID, UserID: UserID, Text: text, Date: c.cfg.Now()})
	return nil
}

func ephemeral(text string) map[string]string {
	return map[string]string{"response_type": "ephemeral", "text": Escape(text)}
}

// AnswerCallback ends a tap on a button. Slack needs nothing for that (the tap was acknowledged when it arrived); a text is shown
// to the person who tapped, privately, through the address that came with the tap.
func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	if text == "" {
		return nil
	}
	c.mu.Lock()
	raw, ok := c.respURLs[callbackID]
	delete(c.respURLs, callbackID)
	c.mu.Unlock()
	if !ok {
		return nil
	}
	u, ok := c.trustedHTTP(raw, "hooks.slack.com")
	if !ok {
		return nil
	}
	body := jsonString(map[string]any{"response_type": "ephemeral", "text": Escape(text), "replace_original": false})
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return clean("response_url", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.api.hc.Do(req)
	if err != nil {
		return clean("response_url", err)
	}
	resp.Body.Close()
	return nil
}
