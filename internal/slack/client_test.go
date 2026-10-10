package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slacktest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type rig struct {
	t      *testing.T
	srv    *slacktest.Server
	c      *Client
	log    *syncBuf
	offset int64
	cancel context.CancelFunc
}

// newRig starts a fake Slack and a client connected to it.
func newRig(t *testing.T, mut func(*Config)) *rig {
	t.Helper()
	srv := slacktest.New(t)
	buf := &syncBuf{}
	cfg := Config{
		BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: srv.APIURL(),
		Log:          slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 80 * time.Millisecond, PingEvery: 200 * time.Millisecond,
	}
	if mut != nil {
		mut(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	srv.WaitConnected(t, 5*time.Second)
	return &rig{t: t, srv: srv, c: c, log: buf, cancel: cancel}
}

// next waits for the next update and confirms the earlier ones, as the hub does.
func (r *rig) next() telegram.Update {
	r.t.Helper()
	ups := r.updates(1, 5*time.Second)
	return ups[0]
}

// updates waits for n updates.
func (r *rig) updates(n int, within time.Duration) []telegram.Update {
	r.t.Helper()
	var got []telegram.Update
	deadline := time.Now().Add(within)
	for len(got) < n {
		left := time.Until(deadline)
		if left <= 0 {
			r.t.Fatalf("got %d of %d updates: %+v", len(got), n, got)
		}
		ups, err := r.c.GetUpdates(context.Background(), r.offset, left)
		if err != nil {
			r.t.Fatal(err)
		}
		for _, u := range ups {
			r.offset = u.ID + 1
			got = append(got, u)
		}
	}
	return got
}

// quiet checks that nothing arrives within d.
func (r *rig) quiet(d time.Duration) {
	r.t.Helper()
	ups, err := r.c.GetUpdates(context.Background(), r.offset, d)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(ups) != 0 {
		r.t.Fatalf("unexpected updates: %+v", ups)
	}
}

func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

func mustID(t *testing.T, ts string) int64 {
	t.Helper()
	id, ok := tsToID(ts)
	if !ok {
		t.Fatalf("bad ts %q", ts)
	}
	return id
}

// ---- the start ----

func TestTheClientFindsItsDirectMessageAndItsBot(t *testing.T) {
	r := newRig(t, nil)
	if r.c.DMChannel() != slacktest.Channel {
		t.Fatalf("channel %q", r.c.DMChannel())
	}
	if r.srv.Calls("conversations.open") != 1 || r.srv.Calls("auth.test") != 1 {
		t.Fatal("it did not ask who it is and where to write")
	}
}

func TestAGivenChannelIsCheckedAgainstTheUser(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ChannelID = slacktest.Channel })
	if r.srv.Calls("conversations.open") != 1 || r.c.DMChannel() != slacktest.Channel {
		t.Fatalf("the channel was not checked: %d calls, channel %q", r.srv.Calls("conversations.open"), r.c.DMChannel())
	}
}

func TestAChannelThatIsNotTheUsersDirectMessageIsNeverWrittenTo(t *testing.T) {
	srv := slacktest.New(t)
	buf := &syncBuf{}
	c, err := New(Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: srv.APIURL(),
		ChannelID: "D0SOMEONE2", Log: slog.New(slog.NewJSONHandler(buf, nil)), ReconnectMin: 10 * time.Millisecond, ReconnectMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for srv.Calls("conversations.open") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.DMChannel() != "" {
		t.Fatalf("the wrong channel is in use: %q", c.DMChannel())
	}
	if _, err := c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "secret"}); err == nil || len(srv.Posted()) != 0 {
		t.Fatalf("something was written to a channel that is not the user's: %v %v", err, srv.Posted())
	}
	if srv.Connected() {
		t.Fatal("the socket was opened with settings that are wrong")
	}
	if !strings.Contains(buf.String(), "not the direct message with the configured user") {
		t.Fatalf("the cause was not logged:\n%s", buf.String())
	}
}

func TestWithoutTheRightToOpenAChatTheConfiguredChannelIsUsedAndTheLogSaysSo(t *testing.T) {
	srv := slacktest.New(t)
	srv.FailNext("conversations.open", "missing_scope")
	buf := &syncBuf{}
	c, _ := New(Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: srv.APIURL(), ChannelID: slacktest.Channel,
		Log: slog.New(slog.NewJSONHandler(buf, nil)), ReconnectMin: 10 * time.Millisecond, ReconnectMax: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	srv.WaitConnected(t, 5*time.Second)
	if c.DMChannel() != slacktest.Channel || !strings.Contains(buf.String(), "no im:write right") {
		t.Fatalf("%q\n%s", c.DMChannel(), buf.String())
	}
}

func TestBadSettingsAreRefusedBeforeAnyRequest(t *testing.T) {
	good := Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID}
	for name, mut := range map[string]func(*Config){
		"app token as bot token": func(c *Config) { c.BotToken = slacktest.AppToken },
		"bot token as app token": func(c *Config) { c.AppToken = slacktest.BotToken },
		"no user":                func(c *Config) { c.UserID = "" },
		"a channel as the user":  func(c *Config) { c.UserID = "C0123ABCD" },
		"a public channel":       func(c *Config) { c.ChannelID = "C0123ABCD" },
		"plain http":             func(c *Config) { c.APIBase = "http://slack.example.com/api" },
		"another https host":     func(c *Config) { c.APIBase = "https://evil.example.com/api" },
		"a lookalike host":       func(c *Config) { c.APIBase = "https://slack.com.evil.example/api" },
		"a host that ends alike": func(c *Config) { c.APIBase = "https://evilslack.com/api" },
		"a loopback look-alike":  func(c *Config) { c.APIBase = "http://localhost.evil.example/api" },
		"a bad scheme":           func(c *Config) { c.APIBase = "ftp://slack.com/api" },
		"credentials in the url": func(c *Config) { c.APIBase = "https://user:pw@slack.com/api" },
		"a bad prefix":           func(c *Config) { c.CommandPrefix = "Bad Prefix" },
	} {
		c := good
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), slacktest.BotToken) || strings.Contains(err.Error(), slacktest.AppToken) {
			t.Errorf("%s: the error holds a token: %v", name, err)
		}
	}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	loop := good
	loop.APIBase = "http://127.0.0.1:1/api"
	if _, err := New(loop); err != nil {
		t.Fatalf("plain http to localhost refused: %v", err)
	}
}

// ---- messages from the user ----

func TestAMessageOfTheUserBecomesAnUpdate(t *testing.T) {
	r := newRig(t, nil)
	ts, env := r.srv.Say(slacktest.Msg{Text: "hello &lt;claude&gt;"})
	u := r.next()
	if u.ChatID != ChatID || u.UserID != UserID || u.Text != "hello <claude>" || u.MessageID != mustID(t, ts) || u.Media != nil || u.CallbackID != "" {
		t.Fatalf("%+v", u)
	}
	if d := time.Since(u.Date); d < 0 || d > 5*time.Minute {
		t.Fatalf("date %v", u.Date)
	}
	r.eventually("the envelope to be acknowledged", func() bool {
		for _, a := range r.srv.Acks() {
			if a == env {
				return true
			}
		}
		return false
	})
}

func TestAReplyInAThreadNamesTheMessageItAnswers(t *testing.T) {
	r := newRig(t, nil)
	parent, _ := r.srv.Say(slacktest.Msg{Text: "first"})
	r.next()
	r.srv.Say(slacktest.Msg{Text: "answer", ThreadTS: parent})
	u := r.next()
	if u.ReplyToID != mustID(t, parent) {
		t.Fatalf("reply to %d, want %d", u.ReplyToID, mustID(t, parent))
	}
	// A message that starts a thread (thread_ts equal to its own ts) answers nothing.
	ts := "1700000000.000009"
	r.srv.Say(slacktest.Msg{Text: "own thread", TS: ts, ThreadTS: ts})
	if u := r.next(); u.ReplyToID != 0 {
		t.Fatalf("reply to %d", u.ReplyToID)
	}
}

func TestOnlyTheAllowedUserInTheAllowedChatIsObeyed(t *testing.T) {
	r := newRig(t, nil)
	cases := []struct {
		name string
		m    slacktest.Msg
	}{
		{"another user in the same chat", slacktest.Msg{User: "U0OTHER01", Text: "do it"}},
		{"the user in another chat", slacktest.Msg{Channel: "D0OTHERDM1", Text: "do it"}},
		{"a user of another workspace", slacktest.Msg{Team: "T0OTHER01", Text: "do it"}},
	}
	for _, c := range cases {
		r.srv.Say(c.m)
		u := r.next()
		if u.ChatID == ChatID && u.UserID == UserID {
			t.Errorf("%s was taken for the user: %+v", c.name, u)
		}
		if u.ChatID < 1 || u.UserID < 1 || u.Text == "do it" {
			t.Errorf("%s: %+v (the text of a stranger must not be kept)", c.name, u)
		}
	}
	if r.c.Strangers() != 3 {
		t.Errorf("%d strangers counted", r.c.Strangers())
	}
	// and the user is still heard afterwards
	r.srv.Say(slacktest.Msg{Text: "real"})
	if u := r.next(); u.Text != "real" || u.ChatID != ChatID || u.UserID != UserID {
		t.Fatalf("%+v", u)
	}
}

func TestEventsThatAreNotANewMessageOfAPersonAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	for name, m := range map[string]slacktest.Msg{
		"the bot's own message":  {User: slacktest.BotUser, Text: "echo"},
		"a bot message":          {BotID: "B0OTHER01", Text: "from a bot"},
		"an edit":                {Subtype: "message_changed", Text: "edited"},
		"a deletion":             {Subtype: "message_deleted", Text: "deleted"},
		"a join":                 {Subtype: "channel_join", Text: "joined"},
		"a message in a channel": {ChannelType: "channel", Channel: "C0PUBLIC01", Text: "public"},
		"a group message":        {ChannelType: "group", Channel: "G0PRIVATE1", Text: "private"},
		"an empty message":       {Text: ""},
		"a bad timestamp":        {TS: "not-a-ts", Text: "bad"},
		"a reminder or workflow": {Subtype: "reminder_add", Text: "remind"},
	} {
		_ = name
		r.srv.Say(m)
	}
	r.quiet(300 * time.Millisecond)
	if r.c.Strangers() != 0 {
		t.Errorf("ignored events were counted as strangers: %d", r.c.Strangers())
	}
}

func TestAnEventThatIsSentAgainIsHandledOnce(t *testing.T) {
	r := newRig(t, nil)
	ev := map[string]any{"type": "message", "channel": slacktest.Channel, "channel_type": "im", "user": slacktest.UserID, "team": slacktest.Team,
		"text": "once", "ts": "1700000001.000100"}
	r.srv.SayRaw("Ev1", "env-first", ev)
	r.srv.SayRaw("Ev1", "env-retry", ev) // Slack repeats an event when the acknowledgement was late
	if u := r.next(); u.Text != "once" {
		t.Fatalf("%+v", u)
	}
	r.quiet(300 * time.Millisecond)
	r.eventually("both envelopes to be acknowledged", func() bool { return len(r.srv.Acks()) >= 2 })
}

func TestFilesOfAMessageBecomeUpdatesWithTheCaptionOnTheFirst(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Say(slacktest.Msg{Text: "two files", Subtype: "file_share", Files: []map[string]any{
		{"id": "F0AAAAAA1", "name": "a.txt", "mimetype": "text/plain", "size": 5, "mode": "hosted"},
		{"id": "F0BBBBBB2", "name": "b.png", "mimetype": "image/png", "size": 9, "mode": "hosted"},
		{"id": "F0EXTERN03", "name": "drive", "mode": "external"},
		{"id": "not-a-file-id", "name": "bad", "mode": "hosted"},
	}})
	ups := r.updates(2, 3*time.Second)
	if ups[0].Media == nil || ups[0].Media.ID != "F0AAAAAA1" || ups[0].Media.Name != "a.txt" || ups[0].Media.Size != 5 || ups[0].Text != "two files" {
		t.Fatalf("%+v %+v", ups[0], ups[0].Media)
	}
	if ups[1].Media == nil || ups[1].Media.ID != "F0BBBBBB2" || ups[1].Text != "" {
		t.Fatalf("%+v %+v", ups[1], ups[1].Media)
	}
	r.quiet(200 * time.Millisecond)
}

// ---- slash commands ----

func TestASlashCommandOfTheAppReachesTheHubAsATextCommand(t *testing.T) {
	r := newRig(t, nil)
	env := r.srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-key", "2", "trig-1")
	u := r.next()
	if u.Text != "/key 2" || u.ChatID != ChatID || u.UserID != UserID {
		t.Fatalf("%+v", u)
	}
	r.srv.Slash(slacktest.UserID, slacktest.Channel, "/PB-Status", "", "trig-2")
	if u := r.next(); u.Text != "/status" {
		t.Fatalf("%+v", u)
	}
	r.eventually("the acknowledgement", func() bool { return len(r.srv.Acks()) >= 2 })
	if p := r.srv.AckPayload(env); p != "" && p != "null" {
		t.Fatalf("a command that was accepted got a payload %q", p)
	}
}

func TestASlashCommandOutsideTheDirectMessageOrFromAStrangerIsNotObeyed(t *testing.T) {
	r := newRig(t, nil)
	env1 := r.srv.Slash(slacktest.UserID, "C0PUBLIC01", "/pb-lockdown", "", "trig-a")
	env2 := r.srv.Slash("U0OTHER01", slacktest.Channel, "/pb-lockdown", "", "trig-b")
	env3 := r.srv.Slash("U0OTHER01", "C0PUBLIC01", "/pb-unlock", "", "trig-c")
	r.srv.Slash(slacktest.UserID, slacktest.Channel, "/status", "", "trig-d") // not the app's command
	r.srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-", "", "trig-e")
	r.srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-../x", "", "trig-f")
	for _, u := range r.updates(2, 3*time.Second) { // the two strangers are counted; the user outside the chat is only told
		if u.ChatID == ChatID && u.UserID == UserID {
			t.Fatalf("obeyed %+v", u)
		}
	}
	r.quiet(200 * time.Millisecond)
	r.eventually("the acknowledgements", func() bool { return len(r.srv.Acks()) >= 6 })
	for _, env := range []string{env1, env2, env3} {
		p := r.srv.AckPayload(env)
		if !strings.Contains(p, "ephemeral") {
			t.Errorf("no private answer for %s: %q", env, p)
		}
	}
	if p := r.srv.AckPayload(env2); strings.Contains(strings.ToLower(p), "lockdown") || strings.Contains(strings.ToLower(p), "hub") {
		t.Errorf("a stranger was told what the app is: %q", p)
	}
}

// ---- buttons ----

func TestButtonsAreSentTappedAndRemoved(t *testing.T) {
	r := newRig(t, nil)
	ids, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "Choose an agent",
		Buttons: [][]telegram.Button{{{Text: "laptop", Data: "agent:laptop"}, {Text: "nas", Data: "agent:nas"}}, {{Text: "Cancel", Data: "x"}}}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
	posted := r.srv.Posted()
	if len(posted) != 1 || posted[0].Blocks == "" {
		t.Fatalf("%+v", posted)
	}
	var bl []map[string]any
	if err := json.Unmarshal([]byte(posted[0].Blocks), &bl); err != nil || len(bl) != 3 || bl[0]["type"] != "section" || bl[1]["type"] != "actions" || bl[2]["type"] != "actions" {
		t.Fatalf("blocks %s (%v)", posted[0].Blocks, err)
	}
	if !strings.Contains(posted[0].Blocks, `"value":"agent:laptop"`) || !strings.Contains(posted[0].Blocks, `"action_id":"pb0_0"`) || !strings.Contains(posted[0].Blocks, `"action_id":"pb1_0"`) {
		t.Fatalf("blocks %s", posted[0].Blocks)
	}

	ts := posted[0].TS
	r.srv.Tap(slacktest.UserID, slacktest.Channel, ts, "agent:laptop", "trig-tap")
	u := r.next()
	if u.CallbackID != "trig-tap" || u.CallbackData != "agent:laptop" || u.MessageID != ids[0] || u.ChatID != ChatID || u.UserID != UserID {
		t.Fatalf("%+v", u)
	}
	if err := r.c.AnswerCallback(context.Background(), u.CallbackID, "Active agent: laptop <b>"); err != nil {
		t.Fatal(err)
	}
	var hook struct {
		ResponseType string `json:"response_type"`
		Text         string `json:"text"`
	}
	if h := r.srv.Hooks(); len(h) != 1 || json.Unmarshal([]byte(h[0]), &hook) != nil || hook.ResponseType != "ephemeral" || hook.Text != "Active agent: laptop &lt;b&gt;" {
		t.Fatalf("hooks %q", h)
	}
	if err := r.c.AnswerCallback(context.Background(), u.CallbackID, "again"); err != nil || len(r.srv.Hooks()) != 1 {
		t.Fatal("the response address was used twice")
	}
	if err := r.c.AnswerCallback(context.Background(), "unknown", ""); err != nil {
		t.Fatal(err)
	}

	if err := r.c.ClearButtons(context.Background(), ChatID, ids[0]); err != nil {
		t.Fatal(err)
	}
	after := r.srv.Posted()[0]
	if !after.Updated || strings.Contains(after.Blocks, "actions") || !strings.Contains(after.Blocks, "section") {
		t.Fatalf("%+v", after)
	}
	if err := r.c.ClearButtons(context.Background(), ChatID, ids[0]); err != nil { // nothing left to clear
		t.Fatal(err)
	}
}

func TestATapOfAStrangerIsNotObeyed(t *testing.T) {
	r := newRig(t, nil)
	ids, _ := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x", Buttons: [][]telegram.Button{{{Text: "go", Data: "go"}}}})
	ts, _ := idToTS(ids[0])
	r.srv.Tap("U0OTHER01", slacktest.Channel, ts, "go", "trig-x")
	u := r.next()
	if u.UserID == UserID || u.CallbackData == "go" {
		t.Fatalf("%+v", u)
	}
	if err := r.c.AnswerCallback(context.Background(), "trig-x", "no"); err != nil || len(r.srv.Hooks()) != 0 {
		t.Fatal("a stranger's response address was used")
	}
}

// ---- sending ----

func TestTextIsSentEscapedSoThatItCannotPingAnyone(t *testing.T) {
	r := newRig(t, nil)
	evil := "<!channel> <!here> <@U0USER001> <#C123|general> <https://evil.example|click> a & b"
	for _, md := range []bool{false, true} {
		if _, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: evil, Markdown: md}); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range r.srv.Posted() {
		for _, bad := range []string{"<!channel>", "<!here>", "<@U0USER001>", "<#C123", "<https://evil"} {
			if strings.Contains(p.Text, bad) {
				t.Errorf("message %d holds %q: %q", i, bad, p.Text)
			}
		}
		if !strings.Contains(p.Text, "&lt;!channel&gt;") || !strings.Contains(p.Text, "a &amp; b") {
			t.Errorf("message %d: %q", i, p.Text)
		}
		if p.Unfurl {
			t.Errorf("message %d unfurls links", i)
		}
	}
	if posted := r.srv.Posted(); posted[0].Mrkdwn || !posted[1].Mrkdwn {
		t.Errorf("mrkdwn flags: plain %v, markdown %v", posted[0].Mrkdwn, posted[1].Mrkdwn)
	}
}

func TestMarkdownIsShownAsSlackFormatting(t *testing.T) {
	r := newRig(t, nil)
	if _, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "**bold** and `code` and [a link](https://example.com)", Markdown: true}); err != nil {
		t.Fatal(err)
	}
	got := r.srv.Posted()[0].Text
	for _, want := range []string{"*bold*", "`code`", "<https://example.com|a link (example.com)>"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not hold %q", got, want)
		}
	}
}

func TestALongTextGoesInSeveralMessagesInOrderAndEveryOneFitsABlock(t *testing.T) {
	r := newRig(t, nil)
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("line number ")
		b.WriteString(strings.Repeat("&<>", 3))
		b.WriteString(" ")
		b.WriteString(string(rune('A' + i%26)))
		b.WriteString("\n")
	}
	for _, md := range []bool{false, true} {
		before := len(r.srv.Posted())
		ids, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: b.String(), Markdown: md,
			Buttons: [][]telegram.Button{{{Text: "ok", Data: "ok"}}}})
		if err != nil {
			t.Fatal(err)
		}
		got := r.srv.Posted()[before:]
		if len(got) < 3 || len(ids) != len(got) {
			t.Fatalf("markdown=%v: %d messages, %d ids", md, len(got), len(ids))
		}
		for i, p := range got {
			if len(p.Text) > 3000 {
				t.Errorf("message %d is %d bytes", i, len(p.Text))
			}
			if hasActions := strings.Contains(p.Blocks, "actions"); hasActions != (i == len(got)-1) {
				t.Errorf("message %d: buttons %v", i, hasActions)
			}
			if i > 0 && mustID(t, p.TS) <= mustID(t, got[i-1].TS) {
				t.Errorf("messages out of order")
			}
		}
	}
}

func TestSendingToAnotherChatOrBeforeTheStartIsRefused(t *testing.T) {
	r := newRig(t, nil)
	_, err := r.c.SendMessage(context.Background(), 12345, telegram.Message{Text: "x"})
	var ae *telegram.APIError
	if !errors.As(err, &ae) || ae.Code != 400 {
		t.Fatalf("%v", err)
	}
	if len(r.srv.Posted()) != 0 {
		t.Fatal("something was sent to another chat")
	}
	c, err := New(Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: r.srv.APIURL()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"}); !errors.As(err, &ae) || ae.Code != 503 {
		t.Fatalf("%v", err)
	}
	if ids, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "  \n "}); err != nil || ids != nil {
		t.Fatalf("an empty message was sent: %v %v", ids, err)
	}
}

func TestRefusalsOfSlackAreToldInTheWordsTheHubUnderstands(t *testing.T) {
	r := newRig(t, nil)
	for _, c := range []struct {
		code string
		want int
	}{{"msg_too_long", 400}, {"invalid_blocks", 400}, {"no_text", 400}, {"channel_not_found", 403}, {"not_in_channel", 403}, {"missing_scope", 403}, {"invalid_auth", 403}, {"internal_error", 503}, {"something_new", 400}} {
		r.srv.FailNext("chat.postMessage", c.code)
		_, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"})
		var ae *telegram.APIError
		if !errors.As(err, &ae) || ae.Code != c.want || !strings.Contains(ae.Desc, c.code) {
			t.Errorf("%s: %v", c.code, err)
		}
	}
	r.srv.StatusNext("chat.postMessage", 502)
	if _, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"}); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("%v", err)
	}
	r.srv.FailNext("chat.postMessage", "msg_too_long")
	_, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"})
	var ae *telegram.APIError
	if !errors.As(err, &ae) || ae.Code != 400 {
		t.Fatalf("the hub would not drop the message: %v", err)
	}
}

func TestARateLimitIsWaitedOutWhenShortAndReportedWhenLong(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxRetryWait = 3 * time.Second })
	r.srv.RateLimitNext("chat.postMessage", 1)
	began := time.Now()
	if _, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "after a wait"}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d < 900*time.Millisecond || d > 2500*time.Millisecond {
		t.Fatalf("waited %v", d)
	}
	r.srv.RateLimitNext("chat.postMessage", 120)
	_, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "too long a wait"})
	var rl telegram.ErrRateLimited
	if !errors.As(err, &rl) || rl.After != 120*time.Second {
		t.Fatalf("%v", err)
	}
	if n := len(r.srv.Posted()); n != 1 {
		t.Fatalf("%d messages", n)
	}
}

func TestTwoMessagesWrittenAtOnceDoNotInterleave(t *testing.T) {
	r := newRig(t, nil)
	long := strings.Repeat("x\n", 3000)
	var wg sync.WaitGroup
	for _, tag := range []string{"A", "B"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: tag + "\n" + long}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	posted := r.srv.Posted()
	if len(posted) < 4 {
		t.Fatalf("%d messages", len(posted))
	}
	first := posted[0].Text[:1]
	half := len(posted) / 2
	for i, p := range posted {
		isFirst := strings.HasPrefix(p.Text, first)
		if (i < half) && !isFirst && i == 0 {
			t.Fatal("order")
		}
	}
	// the first part of each message carries its tag; the tags must not alternate part by part
	tags := ""
	for _, p := range posted {
		if strings.HasPrefix(p.Text, "A") || strings.HasPrefix(p.Text, "B") {
			tags += p.Text[:1]
		}
	}
	if tags != "AB" && tags != "BA" {
		t.Fatalf("tags %q", tags)
	}
	// every part after the first of a message is a run of x lines; they must come in one block per message
	for i := 1; i < len(posted); i++ {
		if i == half && strings.HasPrefix(posted[i].Text, "x") && !(strings.HasPrefix(posted[i-1].Text, "x") || strings.HasPrefix(posted[i-1].Text, "A") || strings.HasPrefix(posted[i-1].Text, "B")) {
			t.Fatal("interleaved")
		}
	}
}

// ---- marks ----

func TestAMarkReplacesTheEarlierMarkOfTheClient(t *testing.T) {
	r := newRig(t, nil)
	ids, _ := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "mark me"})
	ts, _ := idToTS(ids[0])
	if err := r.c.React(context.Background(), ChatID, ids[0], "👀"); err != nil {
		t.Fatal(err)
	}
	if got := r.srv.Reactions(ts); len(got) != 1 || got[0] != "eyes" {
		t.Fatalf("%v", got)
	}
	if err := r.c.React(context.Background(), ChatID, ids[0], "👎"); err != nil {
		t.Fatal(err)
	}
	if got := r.srv.Reactions(ts); len(got) != 1 || got[0] != "thumbsdown" {
		t.Fatalf("%v (log %v)", got, r.srv.ReactionLog())
	}
	if err := r.c.React(context.Background(), ChatID, ids[0], "👎"); err != nil { // the same mark again is no failure
		t.Fatal(err)
	}
	err := r.c.React(context.Background(), ChatID, ids[0], "🦄")
	if err == nil || !strings.Contains(err.Error(), "REACTION_INVALID") {
		t.Fatalf("%v", err)
	}
	if err := r.c.React(context.Background(), 99, ids[0], "👀"); err == nil {
		t.Fatal("a mark in another chat")
	}
	if err := r.c.React(context.Background(), ChatID, -3, "👀"); err == nil {
		t.Fatal("a mark on a message that cannot exist")
	}
}

// ---- files ----

func TestAFileIsUploadedWithItsCaptionAndNothingSecretIsSentToTheUploadAddress(t *testing.T) {
	r := newRig(t, nil)
	ids, err := r.c.SendFile(context.Background(), ChatID, telegram.OutFile{Name: "../../etc/pass\x00wd", Data: []byte("data!"), Caption: "from <laptop> & co",
		Buttons: [][]telegram.Button{{{Text: "delete", Data: "del"}}}})
	if err != nil {
		t.Fatal(err)
	}
	ups := r.srv.Uploads()
	if len(ups) != 1 || string(ups[0].Data) != "data!" || ups[0].Name != "passwd" || ups[0].Channel != slacktest.Channel {
		t.Fatalf("%+v", ups)
	}
	if ups[0].Comment != "from &lt;laptop&gt; &amp; co" {
		t.Fatalf("comment %q", ups[0].Comment)
	}
	if len(ids) != 1 {
		t.Fatalf("ids %v (the buttons' message)", ids)
	}
	if p := r.srv.Posted(); len(p) != 1 || !strings.Contains(p[0].Blocks, "actions") {
		t.Fatalf("%+v", p)
	}
	// without buttons there is nothing more to say
	if ids, err := r.c.SendFile(context.Background(), ChatID, telegram.OutFile{Name: "b.bin", Data: []byte{1}}); err != nil || ids != nil {
		t.Fatalf("%v %v", ids, err)
	}
	if _, err := r.c.SendFile(context.Background(), ChatID, telegram.OutFile{Name: "empty", Data: nil}); err == nil {
		t.Fatal("an empty file was sent")
	}
}

func TestUploadAndSocketAddressesThatAreNotSlacksAreRefused(t *testing.T) {
	other := slacktest.New(t)
	r := newRig(t, nil)
	r.srv.UploadURL = other.HTTP.URL + "/upload/F0UP00001"
	_, err := r.c.SendFile(context.Background(), ChatID, telegram.OutFile{Name: "a", Data: []byte("secret")})
	var ae *telegram.APIError
	if !errors.As(err, &ae) || ae.Code != 502 {
		t.Fatalf("%v", err)
	}
	if len(other.Uploads()) != 0 {
		t.Fatal("the file went to another host")
	}
	for _, bad := range []string{"https://evil.example.com/upload/F0UP00002", "ftp://127.0.0.1/x", "http://user:pw@" + strings.TrimPrefix(r.srv.HTTP.URL, "http://") + "/upload/x"} {
		r.srv.UploadURL = bad
		if _, err := r.c.SendFile(context.Background(), ChatID, telegram.OutFile{Name: "a", Data: []byte("secret")}); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	r.srv.SocketURL = "wss://evil.example.com/link?ticket=x"
	r.srv.DropSocket()
	r.eventually("a refused address", func() bool { return strings.Contains(r.log.String(), "not a Slack socket") })
	if strings.Contains(r.log.String(), "evil.example.com") || strings.Contains(r.log.String(), "ticket") {
		t.Fatalf("the address was logged: %s", r.log.String())
	}
}

func TestAFileTheUserSentIsDownloadedWithinTheLimit(t *testing.T) {
	r := newRig(t, nil)
	r.srv.AddFile("F0AAAAAA1", []byte("hello file"), nil)
	data, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "F0AAAAAA1", Size: 10}, 1000)
	if err != nil || string(data) != "hello file" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "F0AAAAAA1"}, 5); !errors.Is(err, telegram.ErrFileTooLarge) {
		t.Fatalf("%v", err)
	}
	// Slack says it is small and sends more
	r.srv.AddFile("F0LIAR0001", []byte(strings.Repeat("x", 100)), map[string]any{"size": 3})
	if _, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "F0LIAR0001"}, 10); !errors.Is(err, telegram.ErrFileTooLarge) {
		t.Fatalf("%v", err)
	}
	// a made-up id never reaches the network
	calls := r.srv.Calls("files.info")
	if _, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "../../etc/passwd"}, 10); err == nil || r.srv.Calls("files.info") != calls {
		t.Fatal("a bad file id was sent to Slack")
	}
	if _, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "F0MISSING1"}, 10); err == nil {
		t.Fatal("a missing file")
	}
}

func TestADownloadAddressThatIsNotSlacksIsNotUsedAndTheTokenIsNotSentThere(t *testing.T) {
	other := slacktest.New(t)
	other.AddFile("F0AAAAAA1", []byte("stolen"), nil)
	r := newRig(t, nil)
	r.srv.AddFile("F0ELSEWHE1", []byte("x"), map[string]any{"url_private_download": other.HTTP.URL + "/files/F0AAAAAA1/download"})
	if _, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "F0ELSEWHE1"}, 100); err == nil {
		t.Fatal("a download from another host")
	}
	r.srv.AddFile("F0HTMLLOGI", []byte("<html>sign in</html>"), map[string]any{"url_private_download": r.srv.HTTP.URL + "/files/F0HTMLLOGI/download?x=1"})
	// a page instead of the file (the right is missing) is reported, not passed on as the file
	r.srv.AddFile("F0PAGE0001", []byte("<html>sign in</html>"), nil)
	// the fake serves the page when the token is wrong; here the token is right, so the file is served as it is
	if data, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "F0PAGE0001"}, 100); err != nil || !strings.Contains(string(data), "sign in") {
		t.Fatalf("%q %v", data, err)
	}
}

// ---- the socket ----

func TestTheClientReconnectsWhenSlackAsksAndWhenTheConnectionDrops(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Disconnect("refresh_requested")
	r.eventually("a second connection", func() bool { return r.srv.Opens() >= 2 && r.srv.Connected() })
	r.srv.Say(slacktest.Msg{Text: "after the refresh"})
	if u := r.next(); u.Text != "after the refresh" {
		t.Fatalf("%+v", u)
	}
	before := r.srv.Opens()
	r.srv.DropSocket()
	r.eventually("a third connection", func() bool { return r.srv.Opens() > before && r.srv.Connected() })
	r.srv.Say(slacktest.Msg{Text: "after the drop"})
	if u := r.next(); u.Text != "after the drop" {
		t.Fatalf("%+v", u)
	}
}

func TestGetUpdatesReportsASocketThatStaysDownAndRecovers(t *testing.T) {
	r := newRig(t, func(c *Config) { c.DownAfter = 300 * time.Millisecond })
	for i := 0; i < 50; i++ {
		r.srv.FailNext("apps.connections.open", "ratelimited") // not a 429 status, but ok:false, which maps to the same
	}
	r.srv.DropSocket()
	r.eventually("the error", func() bool {
		_, err := r.c.GetUpdates(context.Background(), r.offset, 20*time.Millisecond)
		return err != nil && strings.Contains(err.Error(), "Socket Mode")
	})
}

func TestGetUpdatesForgetsWhatTheHubHandledAndWaitsForTheRest(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Say(slacktest.Msg{Text: "one"})
	r.srv.Say(slacktest.Msg{Text: "two"})
	var ups []telegram.Update
	r.eventually("both updates", func() bool {
		ups, _ = r.c.GetUpdates(context.Background(), 0, 20*time.Millisecond)
		return len(ups) == 2
	})
	again, _ := r.c.GetUpdates(context.Background(), ups[0].ID, 50*time.Millisecond) // the first is not confirmed yet
	if len(again) != 2 {
		t.Fatalf("%d", len(again))
	}
	again, _ = r.c.GetUpdates(context.Background(), ups[1].ID, 50*time.Millisecond) // the first is confirmed, the second is not
	if len(again) != 1 || again[0].Text != "two" {
		t.Fatalf("%+v", again)
	}
	again, _ = r.c.GetUpdates(context.Background(), ups[1].ID+1, 50*time.Millisecond) // both confirmed
	if len(again) != 0 {
		t.Fatalf("%d", len(again))
	}
	began := time.Now()
	r.c.GetUpdates(context.Background(), ups[1].ID+1, 200*time.Millisecond)
	if d := time.Since(began); d < 150*time.Millisecond {
		t.Fatalf("it did not wait: %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.c.GetUpdates(ctx, 0, time.Minute); err == nil {
		t.Fatal("a cancelled wait is not an error")
	}
}

func TestAFloodOfStrangersDoesNotPushOutTheUsersMessages(t *testing.T) {
	r := newRig(t, nil)
	for i := 0; i < maxStrangerQueue+40; i++ {
		ev := map[string]any{"type": "message", "channel": "D0STRANGER", "channel_type": "im", "user": "U0STRANGER", "team": slacktest.Team,
			"text": "spam", "ts": "1700000002.000" + strings.Repeat("0", 3-len(itoaN(i%1000))) + itoaN(i%1000)}
		ev["ts"] = "17000" + itoaN(10000+i) + ".000001"
		r.srv.SayRaw("EvS"+itoaN(i), "", ev)
	}
	r.srv.Say(slacktest.Msg{Text: "the user's message"})
	deadline := time.Now().Add(5 * time.Second)
	var got []telegram.Update
	for time.Now().Before(deadline) {
		ups, _ := r.c.GetUpdates(context.Background(), 0, 100*time.Millisecond)
		got = ups
		for _, u := range ups {
			if u.Text == "the user's message" {
				strangers := 0
				for _, v := range ups {
					if v.UserID != UserID {
						strangers++
					}
				}
				if strangers > maxStrangerQueue {
					t.Fatalf("%d strangers are queued", strangers)
				}
				return
			}
		}
	}
	t.Fatalf("the user's message was lost among %d updates", len(got))
}

func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ---- secrets ----

func TestNoTokenAndNoSocketTicketEverReachesTheLog(t *testing.T) {
	r := newRig(t, nil)
	r.srv.FailNext("chat.postMessage", "invalid_auth")
	r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"})
	r.srv.Disconnect("refresh_requested")
	r.eventually("a second connection", func() bool { return r.srv.Opens() >= 2 })
	r.srv.DropSocket()
	r.srv.WaitConnected(t, 5*time.Second)
	r.srv.Say(slacktest.Msg{Text: "secret words of the user"})
	r.next()
	time.Sleep(100 * time.Millisecond)
	out := r.log.String()
	for _, s := range []string{slacktest.BotToken, slacktest.AppToken, "one-time-secret-ticket", "secret words of the user"} {
		if strings.Contains(out, s) {
			t.Errorf("the log holds %q:\n%s", s, out)
		}
	}
}

// ---- the gap while the socket is closed ----

func TestWhatTheUserWroteWhileTheSocketWasClosedIsReadBackInOrder(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Say(slacktest.Msg{Text: "before"})
	if u := r.next(); u.Text != "before" {
		t.Fatalf("%+v", u)
	}
	// the socket is closed, and the user writes twice without an event reaching the client
	r.srv.DropSocket()
	first := r.srv.SayMissed(slacktest.Msg{Text: "while down one"})
	second := r.srv.SayMissed(slacktest.Msg{Text: "while down two", Subtype: "file_share", Files: []map[string]any{
		{"id": "F0MISSED01", "name": "m.txt", "mimetype": "text/plain", "size": 3, "mode": "hosted"}}})
	r.srv.WaitConnected(t, 5*time.Second)
	ups := r.updates(2, 5*time.Second)
	if ups[0].Text != "while down one" || ups[0].MessageID != mustID(t, first) || ups[1].Media == nil || ups[1].Media.ID != "F0MISSED01" || ups[1].MessageID != mustID(t, second) {
		t.Fatalf("%+v", ups)
	}
	for _, u := range ups {
		if u.ChatID != ChatID || u.UserID != UserID {
			t.Fatalf("a message read back is not the user's: %+v", u)
		}
	}
	// and a live message after it comes after it, once
	r.srv.Say(slacktest.Msg{Text: "after"})
	if u := r.next(); u.Text != "after" {
		t.Fatalf("%+v", u)
	}
	r.quiet(200 * time.Millisecond)
}

func TestMessagesAlreadyHeardAreNotDeliveredAgainAfterAReconnect(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Say(slacktest.Msg{Text: "one"})
	r.srv.Say(slacktest.Msg{Text: "two"})
	r.updates(2, 3*time.Second)
	before := r.srv.Calls("conversations.history")
	r.srv.Disconnect("refresh_requested")
	r.eventually("a second connection and its history call", func() bool { return r.srv.Opens() >= 2 && r.srv.Calls("conversations.history") > before })
	r.quiet(300 * time.Millisecond)
}

func TestNothingIsReadBackBeforeAnyMessageWasHeard(t *testing.T) {
	r := newRig(t, nil)
	r.srv.SayMissed(slacktest.Msg{Text: "old history before the hub knew anything"})
	r.srv.Disconnect("refresh_requested")
	r.eventually("a second connection", func() bool { return r.srv.Opens() >= 2 && r.srv.Connected() })
	if r.srv.Calls("conversations.history") != 0 {
		t.Fatal("the history was read although no message had been heard")
	}
	r.quiet(300 * time.Millisecond)
}

func TestAHistoryThatFailsDoesNotKeepTheSocketFromBeingRead(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Say(slacktest.Msg{Text: "one"})
	r.next()
	r.srv.FailNext("conversations.history", "internal_error")
	r.srv.DropSocket()
	r.srv.WaitConnected(t, 5*time.Second)
	r.srv.Say(slacktest.Msg{Text: "live after the failed history"})
	if u := r.next(); u.Text != "live after the failed history" {
		t.Fatalf("%+v", u)
	}
	r.eventually("the warning", func() bool { return strings.Contains(r.log.String(), "could not read back") })
}

func TestAThreadReplyAlsoSentToTheChannelIsTheUsersWordsAndNamesItsParent(t *testing.T) {
	r := newRig(t, nil)
	parent, _ := r.srv.Say(slacktest.Msg{Text: "first"})
	r.next()
	r.srv.Say(slacktest.Msg{Text: "broadcast answer", Subtype: "thread_broadcast", ThreadTS: parent})
	u := r.next()
	if u.Text != "broadcast answer" || u.ReplyToID != mustID(t, parent) || u.UserID != UserID {
		t.Fatalf("%+v", u)
	}
}

func TestEventsWithIdsThatAreNotIdsAreIgnoredAndLeaveNoKeyBehind(t *testing.T) {
	r := newRig(t, nil)
	big := strings.Repeat("D", 500000)
	for i, ev := range []map[string]any{
		{"type": "message", "channel": big, "channel_type": "im", "user": slacktest.UserID, "text": "x", "ts": "1700000003.000001"},
		{"type": "message", "channel": slacktest.Channel, "channel_type": "im", "user": big, "text": "x", "ts": "1700000003.000002"},
		{"type": "message", "channel": "d lowercase", "channel_type": "im", "user": slacktest.UserID, "text": "x", "ts": "1700000003.000003"},
	} {
		r.srv.SayRaw("EvBad"+itoaN(i), "", ev)
	}
	r.srv.Tap(slacktest.UserID, slacktest.Channel, "1700000003.000004", "v", big)
	r.srv.Tap(slacktest.UserID, slacktest.Channel, "1700000003.000004", "v", "bad trigger!")
	r.srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-key", "2", big)
	r.srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-key", "2", "")
	r.quiet(300 * time.Millisecond)
	r.c.mu.Lock()
	n := len(r.c.seen)
	r.c.mu.Unlock()
	if n != 0 || r.c.Strangers() != 0 {
		t.Fatalf("%d keys, %d strangers", n, r.c.Strangers())
	}
}

func TestStrangersDoNotPushTheUsersKeysOutOfTheDedupRing(t *testing.T) {
	r := newRig(t, nil)
	ev := map[string]any{"type": "message", "channel": slacktest.Channel, "channel_type": "im", "user": slacktest.UserID, "team": slacktest.Team, "text": "once", "ts": "1700000009.000100"}
	r.srv.SayRaw("EvOnce", "env-first", ev)
	if u := r.next(); u.Text != "once" {
		t.Fatalf("%+v", u)
	}
	for i := 0; i < 2300; i++ {
		r.c.push("m:D0STRANGER:"+itoaN(100000+i), telegram.Update{ChatID: 999, UserID: 999, Text: "-"})
	}
	r.srv.SayRaw("EvOnce", "env-retry", ev) // Slack sends the event again, long after
	time.Sleep(300 * time.Millisecond)
	ups, _ := r.c.GetUpdates(context.Background(), r.offset, 50*time.Millisecond)
	for _, u := range ups {
		if u.ChatID == ChatID && u.UserID == UserID {
			t.Fatalf("the event was handled twice: %+v", u)
		}
	}
}

func TestAGapLongerThanAPageIsReadBackInPagesAndInOrder(t *testing.T) {
	old := backfillPage
	backfillPage = 4
	t.Cleanup(func() { backfillPage = old })
	r := newRig(t, nil)
	r.srv.Say(slacktest.Msg{Text: "before"})
	r.next()
	r.srv.DropSocket()
	var want []string
	for i := 0; i < 10; i++ {
		text := "missed " + itoaN(i)
		want = append(want, text)
		r.srv.SayMissed(slacktest.Msg{Text: text})
	}
	r.srv.WaitConnected(t, 5*time.Second)
	ups := r.updates(10, 5*time.Second)
	for i, u := range ups {
		if u.Text != want[i] {
			t.Fatalf("update %d is %q, want %q", i, u.Text, want[i])
		}
	}
	r.quiet(200 * time.Millisecond)
}

func TestAGapLongerThanAllThePagesIsSaidInTheLog(t *testing.T) {
	old := backfillPage
	backfillPage = 1
	t.Cleanup(func() { backfillPage = old })
	r := newRig(t, nil)
	r.srv.Say(slacktest.Msg{Text: "before"})
	r.next()
	r.srv.DropSocket()
	for i := 0; i < backfillPages+3; i++ {
		r.srv.SayMissed(slacktest.Msg{Text: "missed " + itoaN(i)})
	}
	r.srv.WaitConnected(t, 5*time.Second)
	r.updates(backfillPages, 5*time.Second)
	r.eventually("the warning", func() bool { return strings.Contains(r.log.String(), "than could be read back") })
}

// ---- regression cases ----

func TestASlackThatKeepsAskingForANewConnectionIsNotAskedBackInATightLoop(t *testing.T) {
	srv := slacktest.New(t)
	srv.DisconnectOnHello = true
	c, _ := New(Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: srv.APIURL(),
		ReconnectMin: 100 * time.Millisecond, ReconnectMax: 400 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(1500 * time.Millisecond)
	// waits of 100, 200, 400, 400, ... ms: about eight connections in a second and a half, not thousands
	if n := srv.Opens(); n < 3 || n > 15 {
		t.Fatalf("%d connections in 1.5 s", n)
	}
}

func TestAHubThatCannotReachSlackIsNotReadyAndSaysSo(t *testing.T) {
	srv := slacktest.New(t)
	api := srv.APIURL()
	srv.HTTP.Close() // nothing answers
	c, _ := New(Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: api,
		ReconnectMin: 20 * time.Millisecond, ReconnectMax: 40 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, err := c.GetUpdates(context.Background(), 0, 150*time.Millisecond); err == nil || !strings.Contains(err.Error(), "no Socket Mode connection yet") {
		t.Fatalf("a poll with no socket at all passed: %v", err)
	}
	// the poll that only confirms (no waiting) is no failure, so that a stopping hub does not complain
	if _, err := c.GetUpdates(context.Background(), 5, 0); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestAHubWithWrongTokensIsNotReady(t *testing.T) {
	srv := slacktest.New(t)
	c, _ := New(Config{BotToken: "xoxb-9999999999-wrongtoken", AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: srv.APIURL(),
		ReconnectMin: 20 * time.Millisecond, ReconnectMax: 40 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, err := c.GetUpdates(context.Background(), 0, 200*time.Millisecond); err == nil {
		t.Fatal("a hub with a bot token Slack refuses looks ready")
	}
	if srv.Connected() {
		t.Fatal("a socket was opened")
	}
}

func TestButtonsOfAMarkdownMessageAreNotParsedBySlackAndAPlainOnesAreNotMarkdown(t *testing.T) {
	r := newRig(t, nil)
	rows := [][]telegram.Button{{{Text: "ok", Data: "ok"}}}
	r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "a **bold** word", Markdown: true, Buttons: rows})
	r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "a *star* word", Buttons: rows})
	p := r.srv.Posted()
	if !strings.Contains(p[0].Blocks, `"verbatim":true`) || !strings.Contains(p[0].Blocks, `"type":"mrkdwn"`) {
		t.Fatalf("a mrkdwn block lets Slack parse names and links by itself: %s", p[0].Blocks)
	}
	if !strings.Contains(p[1].Blocks, `"type":"plain_text"`) || strings.Contains(p[1].Blocks, "verbatim") {
		t.Fatalf("%s", p[1].Blocks)
	}
}

func TestAMessageWhoseButtonsSlackRefusesIsSentWithoutThemAndSaysSo(t *testing.T) {
	r := newRig(t, nil)
	r.srv.FailNext("chat.postMessage", "invalid_blocks")
	ids, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "Allow this command?", Buttons: [][]telegram.Button{{{Text: "Yes", Data: "y"}}}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
	p := r.srv.Posted()
	if len(p) != 1 || p[0].Blocks != "" || !strings.Contains(p[0].Text, "Allow this command?") || !strings.Contains(p[0].Text, "buttons could not be shown") {
		t.Fatalf("%+v", p)
	}
	// a refusal that is not about the blocks is still a refusal
	r.srv.FailNext("chat.postMessage", "msg_too_long")
	if _, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"}); err == nil {
		t.Fatal("a refusal was swallowed")
	}
}

func TestAnAnswerThatIsNotSlacksOwnIsToldByItsHTTPStatus(t *testing.T) {
	r := newRig(t, nil)
	for _, c := range []struct {
		status int
		body   string
		want   int
	}{{403, `{"message":"forbidden by the proxy"}`, 403}, {404, `{"ok":false}`, 404}, {408, `{"ok":false,"error":""}`, 408}, {502, `{"error":"bad gateway"}`, 502},
		{401, `{"ok":false,"error":"something_new"}`, 401}} {
		r.srv.AnswerNext("chat.postMessage", c.status, c.body)
		_, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"})
		var ae *telegram.APIError
		if !errors.As(err, &ae) || ae.Code != c.want {
			t.Errorf("%d %s: %v", c.status, c.body, err)
		}
	}
	// Slack's own answer with a code nobody knows, on HTTP 200: this request is wrong, as before
	r.srv.FailNext("chat.postMessage", "something_new")
	_, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"})
	var ae *telegram.APIError
	if !errors.As(err, &ae) || ae.Code != 400 {
		t.Errorf("%v", err)
	}
}

func TestOnlyHttpsAddressesOnTheNamedSlackHostsAreTrusted(t *testing.T) {
	c, err := New(Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID})
	if err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]bool{
		"https://files.slack.com/files-pri/T0123456789-F0123456789/download/x": true,
		"https://files.slack.com:443/x":                                        true,
		"http://files.slack.com/x":                                             false,
		"https://other.slack.com/x":                                            false,
		"https://files.slack.com.evil.example/x":                               false,
		"https://evilfiles.slack.com/x":                                        false,
		"https://files.slack.com:444/x":                                        false,
		"https://user:pw@files.slack.com/x":                                    false,
		"https://files.slack.com/x#frag":                                       false,
		"ftp://files.slack.com/x":                                              false,
		"https://files.slack.com./x":                                           false,
		"":                                                                     false,
	} {
		if _, ok := c.trustedHTTP(url, "files.slack.com"); ok != want {
			t.Errorf("%q: %v", url, ok)
		}
	}
	if _, ok := c.trustedHTTP("https://hooks.slack.com/actions/T/1/x", "hooks.slack.com"); !ok {
		t.Error("the response address")
	}
	for url, want := range map[string]bool{"wss://wss-primary.slack.com/link/?ticket=x": true, "wss://wss-backup.slack.com/link/?ticket=x": true,
		"ws://wss-primary.slack.com/link/?ticket=x": false, "https://wss-primary.slack.com/link": false, "wss://evil.example/link": false, "wss://slack.com.evil.example/": false} {
		if _, ok := c.trustedSocket(url); ok != want {
			t.Errorf("%q: %v", url, ok)
		}
	}
}

func TestAShortDownloadIsNotPassedOnAsTheFile(t *testing.T) {
	r := newRig(t, nil)
	r.srv.AddFile("F0SHORT001", []byte("abc"), map[string]any{"size": 100})
	_, err := r.c.Fetch(context.Background(), telegram.FileRef{ID: "F0SHORT001"}, 1000)
	var ae *telegram.APIError
	if !errors.As(err, &ae) || ae.Code != 502 {
		t.Fatalf("%v", err)
	}
}

func TestAFileStaysSentWhenOnlyItsButtonsCouldNotBeShown(t *testing.T) {
	r := newRig(t, nil)
	r.srv.FailNext("chat.postMessage", "internal_error")
	ids, err := r.c.SendFile(context.Background(), ChatID, telegram.OutFile{Name: "a.txt", Data: []byte("x"), Buttons: [][]telegram.Button{{{Text: "delete", Data: "d"}}}})
	if err != nil || ids != nil || len(r.srv.Uploads()) != 1 {
		t.Fatalf("%v %v %d", ids, err, len(r.srv.Uploads()))
	}
	if !strings.Contains(r.log.String(), "buttons of a file could not be shown") {
		t.Fatal("not logged")
	}
}

func TestInvisibleCharactersAreNotKeptInAFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report\u202etxt.exe":    "reporttxt.exe",
		"a\u200bb\u200ec.txt":    "abc.txt",
		"x\u2028y\u2029z":        "xyz",
		"soft\u00adhyphen.md":    "softhyphen.md",
		"\ufeffbom.txt":          "bom.txt",
		"\u2066iso\u2069.txt":    "iso.txt",
		"../../etc/passwd":       "passwd",
		"..\\..\\windows\\x.ini": "x.ini",
		"\u202e":                 "file",
		"..":                     "file",
		"   ":                    "file",
		"tab\there\x00.txt":      "tabhere.txt",
	} {
		if got := fileName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestTwoMessagesWrittenAtTheSameTimeGoOutOneAfterTheOther(t *testing.T) {
	r := newRig(t, nil)
	text := func(tag string) string {
		var b strings.Builder
		for i := 0; i < 400; i++ {
			b.WriteString(tag + " line " + itoaN(i) + " " + strings.Repeat("x", 20) + "\n")
		}
		return b.String()
	}
	var wg sync.WaitGroup
	for _, tag := range []string{"A", "B", "C"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: text(tag)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	posted := r.srv.Posted()
	if len(posted) < 9 {
		t.Fatalf("%d messages", len(posted))
	}
	var order []string
	for _, p := range posted {
		tag := p.Text[:1]
		if len(order) == 0 || order[len(order)-1] != tag {
			order = append(order, tag)
		}
	}
	if len(order) != 3 {
		t.Fatalf("the parts of the messages were mixed: %v", order)
	}
}

func TestAnErrorCodeThatLooksLikeATokenIsNotCopiedIntoErrorsOrLogs(t *testing.T) {
	r := newRig(t, nil)
	r.srv.FailNext("chat.postMessage", "oops "+slacktest.BotToken)
	_, err := r.c.SendMessage(context.Background(), ChatID, telegram.Message{Text: "x"})
	if err == nil || strings.Contains(err.Error(), slacktest.BotToken) || strings.Contains(err.Error(), "xoxb") {
		t.Fatalf("%v", err)
	}
	if got := safeWord("XAPP-1-secret"); got != "redacted" {
		t.Fatalf("%q", got)
	}
}
