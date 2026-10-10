package e2e

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/slack"
	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slacktest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The hub and the agents of the rig with Slack as the chat: the real hub, the real Slack client, a fake Slack (Web API and Socket
// Mode on a local port), the in-memory bus and a fake tmux.

func slackRig(t *testing.T, o testrig.Options) (*testrig.Rig, *slacktest.Server, *slack.Client) {
	t.Helper()
	srv := slacktest.New(t)
	sl, err := slack.New(slack.Config{BotToken: slacktest.BotToken, AppToken: slacktest.AppToken, UserID: slacktest.UserID, APIBase: srv.APIURL(),
		Log: slog.New(slog.NewTextHandler(discardW{}, nil)), ReconnectMin: 10 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sl.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	srv.WaitConnected(t, 5*time.Second)
	if o.DeliveredReaction == "" {
		o.DeliveredReaction = "👀"
	}
	prev := o.Hub
	o.Hub = func(c *hubsvc.Config) {
		c.Telegram, c.ChatID, c.UserID, c.HelpNote, c.ChatName = sl, slack.ChatID, slack.UserID, slack.HelpNote("pb-"), "Slack"
		c.FailedReaction = "👎"
		c.PollTimeout = 100 * time.Millisecond
		if prev != nil {
			prev(c)
		}
	}
	r := testrig.New(t, o)
	return r, srv, sl
}

type discardW struct{}

func (discardW) Write(p []byte) (int, error) { return len(p), nil }

func posted(srv *slacktest.Server) string {
	var b strings.Builder
	for _, p := range srv.Posted() {
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func waitPosted(t *testing.T, r *testrig.Rig, srv *slacktest.Server, sub string) {
	t.Helper()
	r.Eventually("Slack to show "+sub, func() bool { return strings.Contains(posted(srv), sub) })
}

func TestSlackTextReachesTheSessionAndTheReplyComesBack(t *testing.T) {
	r, srv, _ := slackRig(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	waitPosted(t, r, srv, "laptop is connected")

	ts, _ := srv.Say(slacktest.Msg{Text: "hello claude &amp; friends"})
	r.Eventually("the text to be typed", func() bool { return len(a.Exec.Texts()) == 1 })
	if a.Exec.Texts()[0] != "hello claude & friends" {
		t.Fatalf("typed %q", a.Exec.Texts())
	}
	r.Eventually("the mark of the machine on the message", func() bool {
		for _, n := range srv.Reactions(ts) {
			if n == "eyes" {
				return true
			}
		}
		return false
	})

	a.Spool.Add(wire.TypeReply, "the answer, with *stars* and <angle brackets>")
	waitPosted(t, r, srv, "the answer")
	if strings.Contains(posted(srv), "<angle brackets>") {
		t.Fatal("angle brackets from the agent were sent as they are")
	}
}

func TestSlackCommandsAreSlashCommandsOfTheApp(t *testing.T) {
	r, srv, _ := slackRig(t, testrig.Options{})
	r.StartAll()
	waitPosted(t, r, srv, "laptop is connected")
	srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-agents", "", "t1")
	waitPosted(t, r, srv, "laptop")
	srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-help", "", "t2")
	waitPosted(t, r, srv, "/pb-status")
	srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-lockdown", "", "t3")
	waitPosted(t, r, srv, "Lockdown:")
}

func TestARepliedToMessageOfAnAgentGoesToThatAgentInSlack(t *testing.T) {
	r, srv, _ := slackRig(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.StartAll()
	r.WaitConnected("laptop")
	r.WaitConnected("nas")
	nas, laptop := r.Agents["nas"], r.Agents["laptop"]
	nas.Spool.Add(wire.TypeReply, "nas asks: overwrite the file?")
	waitPosted(t, r, srv, "nas asks")
	var ts string
	for _, p := range srv.Posted() {
		if strings.Contains(p.Text, "nas asks") {
			ts = p.TS
		}
	}
	srv.Say(slacktest.Msg{Text: "yes", ThreadTS: ts})
	r.Eventually("yes to reach nas", func() bool { return len(nas.Exec.Texts()) == 1 })
	if nas.Exec.Texts()[0] != "yes" || len(laptop.Exec.Texts()) != 0 {
		t.Fatalf("nas %q laptop %q", nas.Exec.Texts(), laptop.Exec.Texts())
	}
}

func TestChoosingAnAgentWithButtonsInSlack(t *testing.T) {
	r, srv, _ := slackRig(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.StartAll()
	r.WaitConnected("laptop")
	r.WaitConnected("nas")
	srv.Slash(slacktest.UserID, slacktest.Channel, "/pb-agent", "", "t-agent")
	r.Eventually("a message with buttons", func() bool {
		for _, p := range srv.Posted() {
			if strings.Contains(p.Blocks, `"value":"agent:nas"`) {
				return true
			}
		}
		return false
	})
	var ts string
	for _, p := range srv.Posted() {
		if strings.Contains(p.Blocks, `"value":"agent:nas"`) {
			ts = p.TS
		}
	}
	srv.Tap(slacktest.UserID, slacktest.Channel, ts, "agent:nas", "tap-1")
	waitPosted(t, r, srv, "Active agent: nas")
	srv.Say(slacktest.Msg{Text: "to nas"})
	r.Eventually("text on nas", func() bool { return len(r.Agents["nas"].Exec.Texts()) == 1 })
	if len(r.Agents["laptop"].Exec.Texts()) != 0 {
		t.Fatal("the text went to the wrong agent")
	}
	// a tap of a stranger changes nothing
	srv.Tap("U0INTRUDER", slacktest.Channel, ts, "agent:laptop", "tap-2")
	srv.Say(slacktest.Msg{Text: "still nas"})
	r.Eventually("text on nas again", func() bool { return len(r.Agents["nas"].Exec.Texts()) == 2 })
	if len(r.Agents["laptop"].Exec.Texts()) != 0 {
		t.Fatal("a stranger switched the agent")
	}
}

func TestStrangersInSlackReachNothing(t *testing.T) {
	r, srv, _ := slackRig(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	waitPosted(t, r, srv, "laptop is connected")
	srv.Say(slacktest.Msg{User: "U0INTRUDER", Text: "intruder"})
	srv.Say(slacktest.Msg{Channel: "D0INTRUDER", User: "U0INTRUDER", Text: "intruder two"})
	srv.Say(slacktest.Msg{Team: "T0OTHER001", Text: "other workspace"})
	srv.Slash("U0INTRUDER", slacktest.Channel, "/pb-lockdown", "", "t-bad")
	srv.Say(slacktest.Msg{Text: "the user"})
	r.Eventually("the user's text", func() bool { return len(a.Exec.Texts()) == 1 })
	if a.Exec.Texts()[0] != "the user" || a.Agent.Locked() || strings.Contains(posted(srv), "Lockdown") {
		t.Fatalf("typed %q, locked %v", a.Exec.Texts(), a.Agent.Locked())
	}
	if r.Hub.Stats().Unauthorized < 3 {
		t.Fatalf("%d strangers counted", r.Hub.Stats().Unauthorized)
	}
}

func TestAFileFromSlackBecomesAFileInTheInbox(t *testing.T) {
	inbox := map[string]string{}
	r, srv, _ := slackRig(t, testrig.Options{Agent: func(name string, c *agentsvc.Config) {
		inbox[name] = filepath.Join(t.TempDir(), "inbox-"+name)
		c.Ext.InboxDir = inbox[name]
	}})
	r.StartAll()
	a := r.Agents["laptop"]
	waitPosted(t, r, srv, "laptop is connected")
	data := downBytes(150 << 10) // two parts
	srv.AddFile("F0DOWN0001", data, nil)
	srv.Say(slacktest.Msg{Text: "see this", Subtype: "file_share", Files: []map[string]any{
		{"id": "F0DOWN0001", "name": "notes.bin", "mimetype": "application/octet-stream", "size": len(data), "mode": "hosted"}}})
	kind, path, caption, got := downSaved(t, r, a, 1)
	if kind != "file" || caption != "see this" || string(got) != string(data) || !strings.Contains(path, "inbox-laptop") {
		t.Fatalf("%s %s %q (%d bytes)", kind, path, caption, len(got))
	}
}

func TestACommandToOneAgentIsAPlainMessageInSlack(t *testing.T) {
	r, srv, _ := slackRig(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.StartAll()
	r.WaitConnected("laptop")
	r.WaitConnected("nas")
	// "@nas /enter" does not start with a slash, so Slack leaves it alone and the hub reads it as it does in Telegram
	srv.Say(slacktest.Msg{Text: "@nas /enter"})
	r.Eventually("Enter on nas", func() bool { return len(r.Agents["nas"].Exec.Keys()) == 1 })
	if len(r.Agents["laptop"].Exec.Keys()) != 0 || len(r.Agents["laptop"].Exec.Texts()) != 0 {
		t.Fatal("the key went to the wrong agent")
	}
}
