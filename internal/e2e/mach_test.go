package e2e

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// machRig starts a hub and one agent ("laptop") with a fake screen and the given settings of the machine parts.
func machRig(t *testing.T, o testrig.Options, ext agentsvc.Extras) (*testrig.Rig, *testrig.AgentRig) {
	t.Helper()
	o.Screen = true
	o.Agent = func(_ string, c *agentsvc.Config) { c.Ext = ext }
	r := testrig.New(t, o)
	r.StartAll()
	return r, r.Agents["laptop"]
}

// healing is the settings of auto-heal with a short grace, in real time, in a working directory that exists.
func machHealing(t *testing.T, tries int) agentsvc.Extras {
	return agentsvc.Extras{Workdir: t.TempDir(), Heal: agentsvc.Heal{Enabled: true, Command: "claude --continue", Grace: 160 * time.Millisecond, MaxTries: tries, Window: time.Hour}}
}

// machPolled counts the looks at the pane that are made through it.
type machPolled struct {
	*testrig.FakeScreen
	n atomic.Int64
}

func (p *machPolled) PaneCommand(ctx context.Context) (string, error) {
	p.n.Add(1)
	return p.FakeScreen.PaneCommand(ctx)
}

// machRigPolled is machRig with a screen that counts the looks at the pane.
func machRigPolled(t *testing.T, o testrig.Options, ext agentsvc.Extras) (*testrig.Rig, *testrig.AgentRig, *machPolled) {
	t.Helper()
	polled := &machPolled{}
	o.Screen = true
	o.Agent = func(_ string, c *agentsvc.Config) {
		c.Ext = ext
		polled.FakeScreen = c.Screen.(*testrig.FakeScreen)
		c.Screen = polled
	}
	r := testrig.New(t, o)
	r.StartAll()
	return r, r.Agents["laptop"], polled
}

// machGrace is auto-heal with the given grace period (the agent looks four times within it).
func machGrace(t *testing.T, grace time.Duration) agentsvc.Extras {
	ext := machHealing(t, 3)
	ext.Heal.Grace = grace
	return ext
}

// machStuckRig is a rig whose stuck-request time runs on a clock that the test moves.
func machStuckRig(t *testing.T) (*testrig.Rig, *testrig.AgentRig, *testrig.Clock) {
	t.Helper()
	clk := testrig.NewClock()
	r, a := machRig(t, testrig.Options{Clock: clk, SilenceLimit: 48 * time.Hour, OfflineAfter: 48 * time.Hour}, machStuck())
	return r, a, clk
}

func machCount(r *testrig.Rig, sub string) int {
	n := 0
	for _, s := range r.SentTexts() {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// waitCard asks for the status until a card shows sub (the facts come with a heartbeat, which may not have come yet).
func machCard(r *testrig.Rig, sub string) {
	r.T.Helper()
	deadline := time.Now().Add(r.Timeout())
	for time.Now().Before(deadline) {
		r.Say("/status")
		time.Sleep(150 * time.Millisecond)
		if r.Sent(sub) {
			return
		}
	}
	r.T.Fatalf("no status card showed %q\nchat so far: %q", sub, r.SentTexts())
}

func machFacts() map[string]string {
	return map[string]string{"bat": "83% charging", "up": "3d4h", "disk": "212G free of 460G"}
}

func TestTheStatusCardShowsTheFactsOfTheHeartbeat(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, agentsvc.Extras{Facts: machFacts})
	a.Spool.Add(wire.TypeNotice, "something waiting that the hub will take") // the queue is counted, not emptied at once
	want := "laptop — online, heard"
	machCard(r, "🔋 83% charging · ⏱ up 3d4h · 💾 212G free of 460G\n🖥 tmux ok · claude · auto mode · queue")
	if !r.Sent(want) || !r.Sent("🔓 unlocked") {
		t.Fatalf("%q", r.SentTexts())
	}
}

func TestTheCardShowsTheStateOfTheLockTheAlarmAndTheSession(t *testing.T) {
	r, a := machRig(t, testrig.Options{Alarm: true}, agentsvc.Extras{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	r.Say("/lockdown")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	a.Screen.SetMissing(true)
	machCard(r, "🔒 locked · 🚨 alarm armed")
	machCard(r, "🖥 tmux missing")
}

func TestAnOfflineAgentShowsWhatItLastReportedAsOld(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, agentsvc.Extras{Facts: machFacts})
	machCard(r, "🔋 83% charging")
	r.StopAgent("laptop")
	r.Eventually("the hub to call it offline", func() bool { return !r.Hub.Online("laptop") })
	time.Sleep(100 * time.Millisecond)
	before := len(r.TG.Sent())
	r.Say("/status")
	r.Eventually("the offline card", func() bool {
		for _, s := range r.SentTexts()[before:] {
			if strings.Contains(s, "laptop — offline, last heard") && strings.Contains(s, "🕓 last known:\n🔋 83% charging") {
				return true
			}
		}
		return false
	})
	_ = a
}

func TestASessionThatIsGoneIsStartedAgainAfterTheGraceAndTheChatIsTold(t *testing.T) {
	ext := machHealing(t, 3)
	r, a := machRig(t, testrig.Options{}, ext)
	time.Sleep(300 * time.Millisecond)
	if len(a.Screen.Started()) != 0 {
		t.Fatal("started although Claude Code runs")
	}
	a.Screen.SetMissing(true)
	r.Eventually("the start", func() bool { return len(a.Screen.Started()) == 1 })
	if got := a.Screen.Started()[0]; got != ext.Workdir+"|claude --continue" {
		t.Fatalf("%q", got)
	}
	r.WaitSent("♻️ Claude Code had stopped (session gone); started with claude --continue")
	r.Never("a second start of a Claude Code that runs again", 600*time.Millisecond, func() bool { return len(a.Screen.Started()) > 1 })
	if machCount(r, "♻️") != 1 {
		t.Fatalf("%q", r.SentTexts())
	}
}

func TestAShellLeftInThePaneGetsTheCommandTyped(t *testing.T) {
	ext := machHealing(t, 3)
	r, a := machRig(t, testrig.Options{}, ext)
	a.Screen.SetCommand("zsh")
	a.Screen.Set("user@host ~/project % ")
	want := "cd '" + ext.Workdir + "' && exec claude --continue"
	r.Eventually("the command typed", func() bool { return len(a.Exec.Texts()) == 1 })
	if got := a.Exec.Texts()[0]; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	r.WaitSent("♻️ Claude Code had stopped (a shell is in the foreground); started with claude --continue")
	if len(a.Screen.Started()) != 0 {
		t.Fatal("a second session was created")
	}
}

func TestClaudeCodeThatTheOwnerRestartsByHandWithinTheGraceIsNotRaced(t *testing.T) {
	clk := testrig.NewClock() // the grace period runs on a clock the test moves, so that a slow machine cannot change the story
	r, a, polls := machRigPolled(t, testrig.Options{Clock: clk, Heartbeat: time.Hour, SilenceLimit: 48 * time.Hour, OfflineAfter: 48 * time.Hour}, machGrace(t, 400*time.Millisecond))
	look := func() { // a whole look at the pane, after the change, has finished
		n := polls.n.Load()
		r.Eventually("looks at the pane", func() bool { return polls.n.Load() >= n+3 })
	}
	a.Screen.SetMissing(true)
	look() // first seen gone
	clk.Advance(300 * time.Millisecond)
	look() // still inside the grace period
	a.Screen.SetMissing(false)
	look() // the owner started it
	a.Screen.SetMissing(true)
	look() // gone again: a new grace period starts here
	clk.Advance(300 * time.Millisecond)
	look()
	if len(a.Screen.Started()) != 0 {
		t.Fatal("started although the owner's own restart was within the grace period")
	}
	clk.Advance(200 * time.Millisecond)
	r.Eventually("the start after a whole grace period", func() bool { return len(a.Screen.Started()) == 1 })
	r.WaitSent("♻️ Claude Code had stopped (session gone)")
}

func TestARestartLoopGivesUpWithOneNoticeAndStartsNothingMore(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, machHealing(t, 2))
	// Every start dies at once: the fake session disappears again as soon as it is started.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				if len(a.Screen.Started()) > 0 {
					a.Screen.SetMissing(true)
				}
			}
		}
	}()
	a.Screen.SetMissing(true)
	r.WaitSent("could not be kept running (2 starts in 60 min); use /continue when you want to try again")
	r.Never("a third start", 800*time.Millisecond, func() bool { return len(a.Screen.Started()) > 2 })
	if n := machCount(r, "could not be kept running"); n != 1 {
		t.Fatalf("the give-up notice was said %d times", n)
	}
	if n := len(a.Screen.Started()); n != 2 {
		t.Fatalf("%d starts", n)
	}
}

func TestALockedMachineIsNotHealedUntilItIsUnlockedAtTheMachine(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, machHealing(t, 3))
	r.Say("/lockdown")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	a.Screen.SetMissing(true)
	r.Never("a start on a locked machine", time.Second, func() bool { return len(a.Screen.Started()) > 0 })
	if r.Sent("♻️") {
		t.Fatal("a notice about a start that did not happen")
	}
	os.Remove(a.LockFile) // the owner unlocks at the machine
	r.Eventually("unlocked", func() bool { return !a.Agent.Locked() })
	r.Eventually("the start after the unlock", func() bool { return len(a.Screen.Started()) == 1 })
	r.WaitSent("♻️ Claude Code had stopped (session gone)")
}

func TestAgentsWithoutHealingLeaveAGoneSessionAlone(t *testing.T) {
	ext := machHealing(t, 3)
	ext.Heal.Enabled = false
	r, a := machRig(t, testrig.Options{}, ext)
	a.Screen.SetMissing(true)
	r.Never("a start with auto-heal off", 700*time.Millisecond, func() bool { return len(a.Screen.Started()) > 0 })
}

// ---- /continue ----

func TestContinueStartsASessionThatIsGoneAndTheMessageIsMarkedDelivered(t *testing.T) {
	ext := machHealing(t, 3)
	ext.Heal.Enabled = false // works without auto-heal
	r, a := machRig(t, testrig.Options{DeliveredReaction: "👍"}, ext)
	a.Screen.SetMissing(true)
	id := r.Say("/continue")
	r.Eventually("the start", func() bool { return len(a.Screen.Started()) == 1 })
	r.WaitSent("▶️ Claude Code was started (session gone) with claude --continue")
	r.Eventually("the delivered mark on /continue", func() bool {
		for _, m := range r.TG.Reactions() {
			if m.MessageID == id && m.Emoji == "👍" {
				return true
			}
		}
		return false
	})
}

func TestContinueOnARunningClaudeCodeSaysSoAndStartsNothing(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, machHealing(t, 3))
	r.Say("/continue")
	r.WaitSent("Claude Code is already running")
	if len(a.Screen.Started()) != 0 || len(a.Exec.Texts()) != 0 {
		t.Fatal("something was started")
	}
}

func TestContinueTypesTheCommandIntoAShell(t *testing.T) {
	ext := machHealing(t, 3)
	ext.Heal.Enabled = false
	r, a := machRig(t, testrig.Options{}, ext)
	a.Screen.SetCommand("bash")
	a.Screen.Set("user@host ~/project $ ")
	r.Say("/continue")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	if got := a.Exec.Texts()[0]; got != "cd '"+ext.Workdir+"' && exec claude --continue" {
		t.Fatalf("%q", got)
	}
	r.WaitSent("▶️ Claude Code was started (a shell is in the foreground)")
}

func TestContinueWithoutAScreenOrAWorkingDirectoryIsRefusedInTheChat(t *testing.T) {
	r := testrig.New(t, testrig.Options{}) // no screen
	r.StartAll()
	r.Say("/continue")
	r.WaitSent("cannot see its tmux session")

	r2, a := machRig(t, testrig.Options{}, agentsvc.Extras{}) // a screen, no working directory
	r2.Say("/continue")
	r2.WaitSent("no working directory is configured")
	if len(a.Screen.Started()) != 0 {
		t.Fatal("started without a directory")
	}
}

func TestContinueIsRefusedOnALockedAgentAndAnOldOneIsIgnored(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, machHealing(t, 3))
	a.Screen.SetMissing(true)
	r.SayAt("/continue", time.Now().Add(-2*time.Minute)) // commands of this kind live one minute
	r.WaitSent("already older than its validity")
	r.Say("/lockdown")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	r.Say("/unlock") // the hub relays again, the agent stays locked
	r.WaitSent("The hub relays again")
	r.Say("/continue")
	r.WaitSent("refused: this machine is locked")
	if len(a.Screen.Started()) != 0 {
		t.Fatal("started on a locked machine")
	}
}

func TestContinueIsNotInTheBotsMenu(t *testing.T) {
	r, _ := machRig(t, testrig.Options{}, agentsvc.Extras{})
	r.Eventually("the menu", func() bool { return len(r.TG.Commands()) > 0 })
	for _, c := range r.TG.Commands() {
		if c.Name == "continue" {
			t.Fatal("/continue is in the menu")
		}
	}
	r.Say("/help")
	r.WaitSent("/continue")
}

// ---- the stuck request ----

func machStuck() agentsvc.Extras { return agentsvc.Extras{StuckAfter: 200 * time.Millisecond} }

func TestARequestWithoutAReplyIsSaidInTheChat(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, machStuck())
	r.Say("please fix the build")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.WaitSent("⏳ No reply to «please fix the build» for 1 min. Claude Code looks idle. /screen to look, /stop to interrupt.")
}

func TestAReplyBeforeTheTimeMeansNoAlert(t *testing.T) {
	r, a, clk := machStuckRig(t)
	r.Say("question")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	a.Spool.Add(wire.TypeReply, "the answer") // what the Stop hook does
	r.WaitSent("the answer")
	clk.Advance(time.Hour)
	r.Never("an alert after the reply", 500*time.Millisecond, func() bool { return r.Sent("No reply") })
}

func TestAPromptThatWaitsPausesTheAlertUntilTheUserAnswers(t *testing.T) {
	r, a, clk := machStuckRig(t)
	r.Say("deploy it")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	// The notification hook: a prompt with its buttons. The screen is not a prompt, so the notice is the only sign.
	a.Spool.AddItem(spool.Item{Type: wire.TypeNotice, Body: "⏳ Claude needs permission: run the deploy", Buttons: []wire.Button{
		{Label: "1. Yes", Key: "1", Prompt: "0a1b2c3d"}, {Label: "✖ Esc", Key: "Escape", Prompt: "0a1b2c3d"}}})
	r.WaitSent("Claude needs permission")
	clk.Advance(3 * time.Hour)
	r.Never("an alert while a prompt waits", 500*time.Millisecond, func() bool { return r.Sent("No reply") })
	r.Say("/key 1")
	r.Eventually("the key pressed", func() bool { return len(a.Exec.Keys()) == 1 })
	r.Eventually("the alert once the time has run again after the answer", func() bool {
		clk.Advance(time.Second)
		return r.Sent("⏳ No reply to «deploy it»")
	})
}

func TestARequestIsSaidAtMostThreeTimes(t *testing.T) {
	r, a, clk := machStuckRig(t)
	r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.Eventually("three alerts", func() bool { clk.Advance(300 * time.Millisecond); return machCount(r, "No reply") == 3 })
	r.Never("a fourth alert", 600*time.Millisecond, func() bool { clk.Advance(time.Hour); return machCount(r, "No reply") > 3 })
}

func TestNoAlertIsEverSentWhenTheCheckIsOff(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, agentsvc.Extras{StuckAfter: 0})
	r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.Never("an alert with the check off", 1200*time.Millisecond, func() bool { return r.Sent("No reply") })
}

func TestStoppingARequestThatHadNoReplyEndsTheAlerts(t *testing.T) {
	r, a := machRig(t, testrig.Options{}, machStuck())
	r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.WaitSent("No reply")
	r.Say("/esc") // what the ✖ Stop button does
	r.Eventually("escape pressed", func() bool { return len(a.Exec.Keys()) == 1 })
	n := machCount(r, "No reply")
	r.Never("alerts after the interrupt", 1200*time.Millisecond, func() bool { return machCount(r, "No reply") > n })
}

// "@name /continue" starts Claude Code on that agent, like the key and chat commands take @name; it used to be typed into
// the session as plain text.
func TestContinueTakesAtName(t *testing.T) {
	workdir := t.TempDir()
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}, Screen: true,
		Agent: func(name string, c *agentsvc.Config) {
			c.Ext.Workdir = workdir
			c.Ext.Heal = agentsvc.Heal{Command: "claude --continue"}
		}})
	r.StartAll()
	laptop, nas := r.Agents["laptop"], r.Agents["nas"]
	nas.Screen.SetMissing(true)
	r.Say("@nas /continue")
	r.Eventually("Claude Code started on nas", func() bool { return len(nas.Screen.Started()) == 1 })
	if len(nas.Exec.Texts()) != 0 || len(laptop.Screen.Started()) != 0 {
		t.Fatalf("the command was typed as text or reached the laptop: %v %v", nas.Exec.Texts(), laptop.Screen.Started())
	}
}
