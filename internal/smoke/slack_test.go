package smoke

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slacktest"
)

// sentText joins what the bot has written in the fake Slack.
func sentText(s *slacktest.Server) string {
	var b strings.Builder
	for _, p := range s.Posted() {
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// The real binaries with Slack as the chat: the hub talks to the fake Slack (Web API and Socket Mode over a local port), the
// agent types into a real tmux session.
func TestRealBinariesEndToEndInSlack(t *testing.T) {
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("PUBSUB_EMULATOR_HOST not set")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, c := range []string{"hub", "agent", "bridgekeys"} {
		must(t, exec.Command("go", "build", "-o", filepath.Join(bin, c), "github.com/n0rm4l-me/petrvs-bridge/cmd/"+c))
	}
	keys := filepath.Join(dir, "keys")
	agents := filepath.Join(dir, "agents")
	os.MkdirAll(agents, 0o700)
	for _, n := range []string{"hub", "laptop"} {
		must(t, exec.Command(filepath.Join(bin, "bridgekeys"), "gen", "-name", n, "-out", keys))
	}
	must(t, exec.Command("cp", filepath.Join(keys, "laptop.pub.json"), agents))

	sl := slacktest.New(t)
	session := fmt.Sprintf("cbslack%d", os.Getpid())
	must(t, exec.Command("tmux", "new-session", "-d", "-s", session, "-x", "200", "-y", "50", "cat"))
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })
	pane := func() string {
		out, _ := exec.Command("tmux", "capture-pane", "-p", "-t", session).Output()
		return string(out)
	}

	prefix := fmt.Sprintf("slsmoke%d", time.Now().UnixNano()%1_000_000)
	state := filepath.Join(dir, "state")
	common := []string{"PB_PROJECT=pb-smoke", "PB_PREFIX=" + prefix, "PB_CREATE_RESOURCES=true", "PB_LOG_LEVEL=debug"}
	hub := start(t, filepath.Join(bin, "hub"), append(common,
		"PB_IDENTITY="+filepath.Join(keys, "hub.identity.json"), "PB_AGENTS_DIR="+agents,
		"PB_CHAT=slack", "SLACK_BOT_TOKEN="+slacktest.BotToken, "SLACK_APP_TOKEN="+slacktest.AppToken, "PB_SLACK_USER_ID="+slacktest.UserID,
		"PB_SLACK_API_BASE="+sl.APIURL(), "PB_POLL_TIMEOUT=1s", "PB_OFFLINE_AFTER=1m"))
	agentEnv := append(common, "PB_NAME=laptop", "PB_IDENTITY="+filepath.Join(keys, "laptop.identity.json"), "PB_HUB_PUB="+filepath.Join(keys, "hub.pub.json"),
		"PB_TMUX_SESSION="+session, "PB_STATE_DIR="+state, "PB_ALARM=false", "PB_HEARTBEAT=300ms", "PB_HANDSHAKE_RETRY=500ms",
		"PB_SILENCE_LIMIT=5s", "PB_RECEIPT_TIMEOUT=1s")
	agent := start(t, filepath.Join(bin, "agent"), agentEnv)

	eventually(t, "the hub to report the agent connected in Slack\nhub:\n"+hub.output()+"\nagent:\n"+agent.output(), 30*time.Second, func() bool { return strings.Contains(sentText(sl), "laptop is connected") })

	// Slack to tmux
	ts, _ := sl.Say(slacktest.Msg{Text: "hello from slack"})
	eventually(t, "the text in the tmux pane\n"+pane(), 15*time.Second, func() bool { return strings.Contains(pane(), "hello from slack") })
	// and the mark of the machine on the message
	eventually(t, "the mark on the message", 15*time.Second, func() bool {
		for _, n := range sl.Reactions(ts) {
			if n == "eyes" {
				return true
			}
		}
		return false
	})

	// someone else is ignored
	sl.Say(slacktest.Msg{User: "U0INTRUDER", Text: "intruder text"})
	sl.Say(slacktest.Msg{Channel: "D0INTRUDER", User: "U0INTRUDER", Text: "intruder text two"})
	sl.Say(slacktest.Msg{Text: "second line"})
	eventually(t, "the second line", 15*time.Second, func() bool { return strings.Contains(pane(), "second line") })
	if strings.Contains(pane(), "intruder") {
		t.Fatal("an unauthorised message reached the terminal")
	}

	// a hook on the laptop queues a reply, the hub shows it
	must(t, withEnv(exec.Command(filepath.Join(bin, "agent"), "send", "the answer from claude"), "PB_STATE_DIR="+state))
	eventually(t, "the reply in the chat", 15*time.Second, func() bool { return strings.Contains(sentText(sl), "the answer from claude") })

	// a notice with text that would ping people in Slack if it were not escaped
	c := withEnv(exec.Command(filepath.Join(bin, "agent"), "send", "-type", "notice", "-"), "PB_STATE_DIR="+state)
	c.Stdin = strings.NewReader("ping <!channel> and <@U0USER001> & more\n")
	must(t, c)
	eventually(t, "the notice in the chat", 15*time.Second, func() bool { return strings.Contains(sentText(sl), "&lt;!channel&gt;") })
	if strings.Contains(sentText(sl), "<!channel>") {
		t.Fatal("a mention that would ping the channel was sent as one")
	}

	// a file from the machine is uploaded to Slack
	report := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(report, []byte("report body"), 0o600); err != nil {
		t.Fatal(err)
	}
	must(t, withEnv(exec.Command(filepath.Join(bin, "agent"), "send-file", "-caption", "the report <for you>", report), "PB_STATE_DIR="+state))
	eventually(t, "the file in Slack", 20*time.Second, func() bool { return len(sl.Uploads()) == 1 })
	if up := sl.Uploads()[0]; string(up.Data) != "report body" || up.Name != "report.txt" || !strings.Contains(up.Comment, "the report &lt;for you&gt;") {
		t.Fatalf("%+v", up)
	}

	// lockdown from Slack (a slash command of the app), local unlock
	sl.Slash(slacktest.UserID, slacktest.Channel, "/pb-lockdown", "", "trig-lock")
	eventually(t, "the lock file", 15*time.Second, func() bool { _, err := os.Stat(filepath.Join(state, "locked")); return err == nil })
	sl.Slash(slacktest.UserID, slacktest.Channel, "/pb-unlock", "", "trig-unlock")
	sl.Say(slacktest.Msg{Text: "blocked while locked"})
	eventually(t, "the refusal", 15*time.Second, func() bool { return strings.Contains(sentText(sl), "this machine is locked") })
	if strings.Contains(pane(), "blocked while locked") {
		t.Fatal("a locked agent typed text")
	}
	out := must(t, withEnv(exec.Command(filepath.Join(bin, "agent"), "unlock"), "PB_STATE_DIR="+state))
	if !strings.Contains(out, "unlocked") {
		t.Fatal(out)
	}
	sl.Say(slacktest.Msg{Text: "typed after the local unlock"})
	eventually(t, "text after unlock", 15*time.Second, func() bool { return strings.Contains(pane(), "typed after the local unlock") })

	// neither token, nor the socket ticket, nor a message body in the logs
	for name, p := range map[string]*proc{"hub": hub, "agent": agent} {
		o := p.output()
		for _, s := range []string{slacktest.BotToken, slacktest.AppToken, "one-time-secret-ticket", "hello from slack", "the answer from claude"} {
			if strings.Contains(o, s) {
				t.Fatalf("%s log leaks %q:\n%s", name, s, o)
			}
		}
	}

	for name, p := range map[string]*proc{"agent": agent, "hub": hub} {
		p.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s did not stop cleanly: %v\n%s", name, err, p.output())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not stop after SIGTERM", name)
		}
	}
}
