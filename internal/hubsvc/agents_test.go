package hubsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

func two(t *testing.T) Config {
	t.Helper()
	c := base(t)
	b, _ := identity.Generate("desk")
	c.Agents = append(c.Agents, b.Public())
	return c
}

func TestWithSeveralAgentsTextIsNotSentUntilOneIsChosen(t *testing.T) {
	c := two(t)
	fake := c.Telegram.(*telegram.Fake)
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"yes", "/enter", "/key 1", "/alarm on"} {
		h.HandleText(context.Background(), text)
	}
	sent := fake.Sent()
	if len(sent) != 4 {
		t.Fatalf("%v", sent)
	}
	for _, s := range sent {
		if !strings.Contains(s.Text, "No agent is chosen") || len(s.Buttons) != 2 {
			t.Errorf("the user was not asked to choose: %+v", s)
		}
	}
	if h.Stats().Relayed != 0 {
		t.Fatal("something was relayed without a chosen agent")
	}
}

func TestOneAgentIsAlwaysActiveAndHasNoButtons(t *testing.T) {
	c := base(t)
	fake := c.Telegram.(*telegram.Fake)
	h, _ := New(c)
	if h.activeName() != "laptop" {
		t.Fatalf("%q", h.activeName())
	}
	h.HandleText(context.Background(), "/agents")
	if s := fake.Sent(); len(s) != 1 || len(s[0].Buttons) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestADefaultAgentIsUsedAndMustBeKnown(t *testing.T) {
	c := two(t)
	c.DefaultAgent = "desk"
	h, err := New(c)
	if err != nil || h.activeName() != "desk" {
		t.Fatalf("%v %q", err, h.activeName())
	}
	c = two(t)
	c.DefaultAgent = "ghost"
	if _, err := New(c); err == nil {
		t.Fatal("a default agent that is not pinned was accepted")
	}
}

func TestTheButtonsNameEveryAgentAndMarkTheActiveOne(t *testing.T) {
	c := two(t)
	c.DefaultAgent = "desk"
	fake := c.Telegram.(*telegram.Fake)
	h, _ := New(c)
	h.HandleText(context.Background(), "/agent")
	h.HandleText(context.Background(), "/status")
	for _, s := range fake.Sent() {
		if len(s.Buttons) != 2 || s.Buttons[0][0] != (telegram.Button{Text: "desk", Data: "agent:desk"}) && s.Buttons[0][0] != (telegram.Button{Text: "● desk", Data: "agent:desk"}) {
			t.Fatalf("%+v", s.Buttons)
		}
		var marked []string
		for _, row := range s.Buttons {
			if strings.HasPrefix(row[0].Text, "● ") {
				marked = append(marked, row[0].Data)
			}
		}
		if len(marked) != 1 || marked[0] != "agent:desk" {
			t.Fatalf("the active agent is not the only one marked: %v", marked)
		}
	}
	if !strings.HasPrefix(fake.Sent()[0].Text, "Active agent: desk") {
		t.Fatalf("%q", fake.Sent()[0].Text)
	}
}

func TestTappingAButtonChoosesTheAgentButOnlyForTheAuthorisedUser(t *testing.T) {
	c := two(t)
	fake := c.Telegram.(*telegram.Fake)
	h := runHub(t, c)
	fake.Push(telegram.Update{ID: 1, ChatID: 9, UserID: 9, CallbackID: "stranger", CallbackData: "agent:desk"})
	fake.Push(telegram.Update{ID: 2, ChatID: 1, UserID: 1, CallbackID: "bad", CallbackData: "something:else"})
	fake.Push(telegram.Update{ID: 3, ChatID: 1, UserID: 1, CallbackID: "ghost", CallbackData: "agent:ghost"})
	eventually(t, "both answers", func() bool { return len(fake.Answered()) == 2 })
	if h.activeName() != "" {
		t.Fatalf("an agent was chosen by a stranger or by bad data: %q", h.activeName())
	}
	fake.Push(telegram.Update{ID: 4, ChatID: 1, UserID: 1, CallbackID: "ok", CallbackData: "agent:desk"})
	eventually(t, "the choice", func() bool { return h.activeName() == "desk" })
	eventually(t, "the confirmation", func() bool {
		for _, s := range fake.Sent() {
			if s.Text == "Active agent: desk" {
				return true
			}
		}
		return false
	})
	for _, a := range fake.Answered() {
		if strings.HasPrefix(a, "stranger") {
			t.Fatal("a stranger's tap was answered")
		}
	}
}

func TestOnlyTheLatestMessagesAreRememberedForReplies(t *testing.T) {
	h, _ := New(two(t))
	for i := int64(1); i <= 3*maxTrackedMessages; i++ {
		h.track("desk", []int64{i})
	}
	if h.agentOfMessage(1) != "" || h.agentOfMessage(0) != "" {
		t.Fatal("an old message is still remembered")
	}
	if h.agentOfMessage(3*maxTrackedMessages) != "desk" {
		t.Fatal("the latest message is forgotten")
	}
	if len(h.msgAgent) > maxTrackedMessages || len(h.msgRing) > maxTrackedMessages {
		t.Fatalf("%d %d", len(h.msgAgent), len(h.msgRing))
	}
}
