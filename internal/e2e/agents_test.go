package e2e

import (
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func pair(t *testing.T) (*testrig.Rig, *testrig.AgentRig, *testrig.AgentRig) {
	t.Helper()
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.StartAll()
	return r, r.Agents["laptop"], r.Agents["nas"]
}

func TestWithSeveralAgentsTheHubAsksBeforeItTypesAnything(t *testing.T) {
	r, laptop, nas := pair(t)
	r.Say("yes")
	r.WaitSent("No agent is chosen")
	r.Never("text typed although no agent was chosen", 400*time.Millisecond, func() bool {
		return len(laptop.Exec.Texts())+len(nas.Exec.Texts()) > 0
	})
	r.Say("/agent nas")
	r.WaitSent("Active agent: nas")
	r.Say("yes")
	r.Eventually("nas typed", func() bool { return len(nas.Exec.Texts()) == 1 })
	if len(laptop.Exec.Texts()) != 0 {
		t.Fatal("the other agent got the text")
	}
}

func TestARestartedHubForgetsTheChoiceAndAsksAgain(t *testing.T) {
	r, _, nas := pair(t)
	r.Say("/agent nas")
	r.Say("before")
	r.Eventually("typed", func() bool { return len(nas.Exec.Texts()) == 1 })
	r.StartHub()
	r.Eventually("sessions again", func() bool { return r.Hub.Online("laptop") && r.Hub.Online("nas") })
	r.Say("after")
	r.WaitSent("No agent is chosen")
	r.Never("text typed after the restart without a choice", 400*time.Millisecond, func() bool { return len(nas.Exec.Texts()) > 1 })
}

func TestTappingAnAgentButtonSwitchesWhereTextGoes(t *testing.T) {
	r, laptop, nas := pair(t)
	r.Say("/agents")
	r.WaitSent("nas — online")
	var buttons int
	for _, s := range r.TG.Sent() {
		buttons += len(s.Buttons)
	}
	if buttons != 2 {
		t.Fatalf("expected a button per agent, saw %d", buttons)
	}
	r.Press("agent:laptop")
	r.WaitSent("Active agent: laptop")
	r.Say("hello")
	r.Eventually("laptop typed", func() bool { return len(laptop.Exec.Texts()) == 1 })
	r.Press("agent:nas")
	r.WaitSent("Active agent: nas")
	r.Say("hello again")
	r.Eventually("nas typed", func() bool { return len(nas.Exec.Texts()) == 1 })
	if len(laptop.Exec.Texts()) != 1 {
		t.Fatal("text followed the wrong choice")
	}
}

func TestAnswerToAnAgentsMessageGoesToThatAgent(t *testing.T) {
	r, laptop, nas := pair(t)
	r.Say("/agent laptop")
	r.WaitSent("Active agent: laptop")
	nas.Spool.Add(wire.TypeReply, "nas asks: overwrite the file?")
	r.WaitSent("[nas] nas asks")
	id := r.MessageID("[nas] nas asks")
	if id == 0 {
		t.Fatal("the chat gave the message no id")
	}
	r.SayReplyTo("yes", id)
	r.Eventually("nas typed the answer", func() bool { return len(nas.Exec.Texts()) == 1 })
	r.WaitSent("→ nas")
	// a key press as a reply goes the same way
	r.SayReplyTo("/enter", id)
	r.Eventually("nas pressed Enter", func() bool { return len(nas.Exec.Keys()) == 1 })
	if nas.Exec.Texts()[0] != "yes" || nas.Exec.Keys()[0] != "Enter" {
		t.Fatalf("%v %v", nas.Exec.Texts(), nas.Exec.Keys())
	}
	// without a reply the text still goes to the chosen agent
	r.Say("plain")
	r.Eventually("laptop typed", func() bool { return len(laptop.Exec.Texts()) == 1 })
	if len(nas.Exec.Texts()) != 1 || len(laptop.Exec.Keys()) != 0 {
		t.Fatal("something went to the wrong agent")
	}
}

func TestAnAnswerToTheChosenAgentsOwnMessageNeedsNoNote(t *testing.T) {
	r, laptop, _ := pair(t)
	r.Say("/agent laptop")
	laptop.Spool.Add(wire.TypeReply, "laptop asks")
	r.WaitSent("[laptop] laptop asks")
	r.SayReplyTo("yes", r.MessageID("[laptop] laptop asks"))
	r.Eventually("laptop typed", func() bool { return len(laptop.Exec.Texts()) == 1 })
	if r.Sent("→") {
		t.Fatal("a note about the destination although the agent was already the chosen one")
	}
}

// With several agents the hub cannot tell which one wrote a message it has forgotten (after a restart, or when newer messages
// pushed it out), and the active agent is only a guess: typing "yes" into the wrong machine is worse than asking again.
func TestAnAnswerToAMessageTheHubForgotIsNotGuessedWithSeveralAgents(t *testing.T) {
	r, laptop, nas := pair(t)
	r.Say("/agent nas")
	r.WaitSent("Active agent: nas")
	r.SayReplyTo("hello", 424242)
	r.WaitSent("no longer knows which agent")
	r.Never("text typed anywhere", 400*time.Millisecond, func() bool {
		return len(nas.Exec.Texts())+len(laptop.Exec.Texts()) > 0
	})
}

// With one agent there is nothing to confuse: the answer goes to it.
func TestAnAnswerToAnUnknownMessageGoesToTheOnlyAgent(t *testing.T) {
	r, only := one(t, testrig.Options{})
	r.SayReplyTo("hello", 424242)
	r.Eventually("typed", func() bool { return len(only.Exec.Texts()) == 1 })
}

func TestAtNameWithAKeyCommandPressesTheKeyAndOtherSlashTextIsTyped(t *testing.T) {
	r, laptop, nas := pair(t)
	r.Say("@nas /enter")
	r.Eventually("Enter on nas", func() bool { return len(nas.Exec.Keys()) == 1 })
	r.Say("@nas /key 2")
	r.Eventually("key 2 on nas", func() bool { return len(nas.Exec.Keys()) == 2 })
	r.Say("@nas /compact now")
	r.Eventually("slash text typed on nas", func() bool { return len(nas.Exec.Texts()) == 1 })
	if nas.Exec.Keys()[0] != "Enter" || nas.Exec.Texts()[0] != "/compact now" {
		t.Fatalf("%v %v", nas.Exec.Keys(), nas.Exec.Texts())
	}
	if len(laptop.Exec.Keys())+len(laptop.Exec.Texts()) != 0 {
		t.Fatal("the laptop was touched")
	}
}

func TestAtNameAlarmCommandReachesOnlyThatAgent(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}, Alarm: true})
	r.StartAll()
	r.Say("@nas /alarm on")
	r.Eventually("nas armed", func() bool { return r.Agents["nas"].Alarm.Armed() })
	time.Sleep(200 * time.Millisecond)
	if r.Agents["laptop"].Alarm.Armed() {
		t.Fatal("the other agent was armed too")
	}
}

// Shift+Tab cycles Claude Code's permission mode, so it is a key of its own that has to be asked for by name: /key btab.
// It is not among the one-tap commands of the bot's menu, where a slip of the finger would change the mode.
func TestKeyBTabReachesOnlyTheNamedAgentAndOnlyUnderItsKeyName(t *testing.T) {
	r, laptop, nas := pair(t)
	r.Say("@nas /key btab")
	r.Eventually("BTab on nas", func() bool { return len(nas.Exec.Keys()) == 1 })
	r.Say("@nas /key BTab")
	r.Eventually("BTab again on nas", func() bool { return len(nas.Exec.Keys()) == 2 })
	r.Say("@nas /key Shift+Tab")
	r.Eventually("Shift+Tab on nas", func() bool { return len(nas.Exec.Keys()) == 3 })
	for i, k := range nas.Exec.Keys() {
		if k != "BTab" {
			t.Errorf("key %d is %q, want BTab", i, k)
		}
	}
	if len(laptop.Exec.Keys())+len(laptop.Exec.Texts()) != 0 {
		t.Fatal("the laptop was touched")
	}
	// no command for it in the menu, and no bare /btab
	r.Say("/btab")
	r.WaitSent("Unknown command")
	r.Never("a key typed by /btab", 300*time.Millisecond, func() bool { return len(nas.Exec.Keys()) != 3 })
}
