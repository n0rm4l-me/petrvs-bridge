package hubsvc

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

func TestEveryCommandOfTheHelpCanBeRegisteredAndNoneIsMissing(t *testing.T) {
	names := map[string]bool{}
	valid := regexp.MustCompile(`^[a-z0-9_]{1,24}$`)
	for _, c := range ChatCommands() {
		if names[c.Name] {
			t.Errorf("%s twice", c.Name)
		}
		names[c.Name] = true
		if !valid.MatchString(c.Name) || len(c.Description) < 3 || len(c.Description) > 200 {
			t.Errorf("a chat would refuse %+v", c)
		}
	}
	// every command the help names is registered ...
	for _, m := range regexp.MustCompile(`/([a-z]+)`).FindAllStringSubmatch(baseHelp+chatHelp+machineHelp, -1) {
		if !names[m[1]] {
			t.Errorf("/%s is in the help and is not in ChatCommands", m[1])
		}
	}
	// ... and every registered command is named in the help (but the two that need no listing: the help itself, and /status, the
	// other name of /agents)
	for n := range names {
		if n == "help" || n == "status" {
			continue
		}
		if !strings.Contains(helpText, "/"+n) {
			t.Errorf("/%s is registered and is not in the help", n)
		}
	}
	// the Telegram menu is part of the list
	for _, c := range fullMenu() {
		if !names[c.Name] {
			t.Errorf("/%s of the menu is not registered", c.Name)
		}
	}
}

func TestEveryRegisteredCommandIsUnderstood(t *testing.T) {
	c := base(t)
	fake := c.Telegram.(*telegram.Fake)
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range ChatCommands() {
		h.HandleText(context.Background(), "/"+cmd.Name)
	}
	for _, s := range fake.Sent() {
		if strings.HasPrefix(s.Text, "Unknown command") {
			t.Errorf("a registered command is not understood: %q", s.Text)
		}
	}
}

func TestTheNoteOfTheChatIsAddedToTheHelpOnly(t *testing.T) {
	c := base(t)
	c.HelpNote = "In Slack every command has the prefix pb-."
	fake := c.Telegram.(*telegram.Fake)
	h, _ := New(c)
	h.HandleText(context.Background(), "/help")
	h.HandleText(context.Background(), "/nonsense")
	sent := fake.Sent()
	if len(sent) != 2 {
		t.Fatalf("%v", sent)
	}
	for _, s := range sent {
		if !strings.Contains(s.Text, helpText) || !strings.HasSuffix(s.Text, c.HelpNote) {
			t.Errorf("%q", s.Text)
		}
	}
	if !strings.HasPrefix(sent[1].Text, "Unknown command.") {
		t.Errorf("%q", sent[1].Text)
	}
}

func TestTheChatIsNamedInWhatTheHubSaysAndTelegramsLimitIsNotAttributedToSlack(t *testing.T) {
	h, _ := New(base(t))
	if h.cfg.ChatName != "Telegram" || !strings.Contains(h.fetchLimitNote(), "20 MB") {
		t.Fatalf("%q %q", h.cfg.ChatName, h.fetchLimitNote())
	}
	c := base(t)
	c.ChatName = "Slack"
	h, _ = New(c)
	if h.cfg.ChatName != "Slack" || h.fetchLimitNote() != "" {
		t.Fatalf("%q %q", h.cfg.ChatName, h.fetchLimitNote())
	}
}
