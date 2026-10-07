package hubsvc_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/promptcmd"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func uiRig(t *testing.T, agents ...string) *testrig.Rig {
	t.Helper()
	r := testrig.New(t, testrig.Options{Agents: agents, Screen: true})
	r.StartAll()
	return r
}

// hookNotice does what the Notification hook does on the agent's machine: it reads the screen and queues the notice.
func hookNotice(t *testing.T, a *testrig.AgentRig) {
	t.Helper()
	err := promptcmd.Run(context.Background(), promptcmd.Options{
		Stdin: strings.NewReader(`{"message":"Claude needs your permission","notification_type":"permission_prompt"}`), Pane: "%1",
		Capture: func(ctx context.Context, _ string) (string, error) { return a.Screen.Capture(ctx) }, Queue: a.Spool, Tries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// nthButtons waits for the n-th message with buttons (counting from 1) and returns it.
func nthButtons(r *testrig.Rig, n int) telegram.Sent {
	r.T.Helper()
	r.Eventually("a message with buttons", func() bool { return len(r.SentWithButtons()) >= n })
	return r.SentWithButtons()[n-1]
}

func dataOf(m telegram.Sent, label string) string {
	for _, row := range m.Buttons {
		for _, b := range row {
			if strings.HasPrefix(b.Text, label) {
				return b.Data
			}
		}
	}
	return ""
}

func spoolNotice(body string, buttons ...wire.Button) spool.Item {
	return spool.Item{Type: wire.TypeNotice, Body: body, Buttons: buttons}
}

func screenButton() wire.Button { return wire.Button{Label: "📺 Screen", Key: wire.ButtonScreen} }

func answered(r *testrig.Rig, sub string) bool {
	for _, a := range r.TG.Answered() {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

func TestATapOnAPromptButtonPressesTheKeyAndTakesTheButtonsOff(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls -la"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)
	if len(msg.Buttons) != 2 || len(msg.Buttons[0]) != 3 || len(msg.Buttons[1]) != 1 || !strings.Contains(msg.Text, "│ ls -la") {
		t.Fatalf("%+v", msg)
	}
	yes := dataOf(msg, "1. Yes")
	if !strings.HasPrefix(yes, "k:laptop:1:") || len(yes) > 64 {
		t.Fatalf("%q", yes)
	}

	r.PressOn(msg.ID, yes)
	r.Eventually("key 1 pressed", func() bool { return slices.Equal(a.Exec.Keys(), []string{"1"}) })
	r.Eventually("the tap answered", func() bool { return answered(r, "Sent 1") })
	r.Eventually("the buttons taken off", func() bool { return slices.Equal(r.TG.Cleared(), []int64{msg.ID}) })

	// the same message cannot answer twice, whichever button
	r.PressOn(msg.ID, dataOf(msg, "3. No"))
	r.PressOn(msg.ID, yes)
	r.Eventually("told so", func() bool { return answered(r, "Already answered") })
	r.Never("a second key for the same prompt", 400*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 1 })
}

// An old notice must not answer a new prompt: the prompt is on the screen, but it is another one.
func TestATapOnAnOldNoticeDoesNotAnswerANewPrompt(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls /tmp"))
	hookNotice(t, a)
	old := nthButtons(r, 1)

	a.Screen.Set(screentest.Bash(1, "rm -rf /tmp/x")) // the same dialog for another command
	r.PressOn(old.ID, dataOf(old, "1. Yes"))
	r.WaitSent("no longer on the screen")
	r.Never("the old button pressed a key", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 })

	// the prompt that is there now is answered with its own notice
	hookNotice(t, a)
	fresh := nthButtons(r, 2)
	if dataOf(fresh, "1. Yes") == dataOf(old, "1. Yes") {
		t.Fatal("two prompts for different commands have the same button")
	}
	r.PressOn(fresh.ID, dataOf(fresh, "1. Yes"))
	r.Eventually("the new prompt answered", func() bool { return slices.Equal(a.Exec.Keys(), []string{"1"}) })
}

// Someone moving the highlight on the laptop meanwhile must not make a valid button fail.
func TestMovingTheHighlightOnTheLaptopDoesNotBreakTheTap(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)
	a.Screen.Set(screentest.Bash(3, "ls"))
	r.PressOn(msg.ID, dataOf(msg, "3. No"))
	r.Eventually("pressed", func() bool { return slices.Equal(a.Exec.Keys(), []string{"3"}) })
}

func TestATapWhileTheHubIsLockedDownSendsNothing(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)
	r.Say("/lockdown")
	r.WaitSent("Lockdown:")
	r.Eventually("the agent locked", a.Agent.Locked)
	r.PressOn(msg.ID, dataOf(msg, "1. Yes"))
	r.Eventually("told it is locked", func() bool { return answered(r, "Locked") })
	r.PressOn(msg.ID, "s:laptop")
	r.Eventually("told again", func() bool { return strings.Count(strings.Join(r.TG.Answered(), "\n"), "Locked") == 2 })
	r.Never("a key pressed or a screen sent while locked down", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 || r.Sent("screen:") })
	if len(r.TG.Cleared()) != 0 {
		t.Fatal("the buttons were taken off although nothing was sent")
	}

	// The hub relays again, the agent stays locked until unlocked on its machine: the key is refused there.
	r.Say("/unlock")
	r.WaitSent("The hub relays again")
	r.PressOn(msg.ID, dataOf(msg, "1. Yes"))
	r.WaitSent("refused: this machine is locked")
	if len(a.Exec.Keys()) != 0 {
		t.Fatal("a locked agent pressed a key")
	}
}

func TestATapForAnAgentThatIsOfflineIsNotSentAndKeepsTheButtons(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)
	r.StopAgent("laptop")
	r.Eventually("the hub to see it offline", func() bool { return !r.Hub.Online("laptop") })
	r.PressOn(msg.ID, dataOf(msg, "1. Yes"))
	r.Eventually("told it is offline", func() bool { return answered(r, "laptop is offline") })
	if len(r.TG.Cleared()) != 0 || len(a.Exec.Keys()) != 0 || r.Sent("Not sent") {
		t.Fatalf("cleared %v, keys %v, chat %q", r.TG.Cleared(), a.Exec.Keys(), r.SentTexts())
	}
	// It comes back and the prompt is still there: the same notice works, since nothing used it up.
	r.StartAgent("laptop")
	r.WaitConnected("laptop")
	r.PressOn(msg.ID, dataOf(msg, "1. Yes"))
	r.Eventually("pressed", func() bool { return slices.Equal(a.Exec.Keys(), []string{"1"}) })
}

// Callback data from anything but the hub's own buttons: nothing is pressed, and the tap is answered.
func TestGarbageCallbackDataPressesNothing(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)
	bad := []string{
		"k:", "k::", "k:laptop", "k:laptop:", "k:laptop::", "k:laptop:1:a:b", "k:laptop:1:zz zz", "k:laptop:1:" + strings.Repeat("a", 17),
		"k:laptop:BTab:", "k:laptop:BTab:0123abcd", "k:laptop:btab:", "k:laptop:Delete:", "k:laptop:Enter;rm:x", "k:laptop:10:", "k:laptop:0:", "k:laptop:enter:",
		"k:ghost:1:0123abcd", "k:LAPTOP:1:0123abcd", "k: laptop:1:0123abcd", "k:laptop\n:1:0123abcd",
		"s:", "s:ghost", "s:laptop:extra", "s: laptop",
		"k:" + strings.Repeat("a", 1000), "x:laptop:1:", "key:laptop:1:", "K:laptop:1:", "\x00", "k:laptop:1:\x00", "é", strings.Repeat("k:", 500),
	}
	for _, data := range bad {
		r.PressOn(msg.ID, data)
	}
	r.Eventually("every tap answered", func() bool { return len(r.TG.Answered()) >= len(bad) })
	r.Never("a key, text or screen from garbage", 400*time.Millisecond, func() bool {
		return len(a.Exec.Keys())+len(a.Exec.Texts()) > 0 || len(r.SentWithButtons()) > 1
	})
	if len(r.TG.Cleared()) != 0 {
		t.Fatalf("buttons were taken off by garbage: %v", r.TG.Cleared())
	}
	// and the real button still works afterwards
	r.PressOn(msg.ID, dataOf(msg, "1. Yes"))
	r.Eventually("pressed", func() bool { return slices.Equal(a.Exec.Keys(), []string{"1"}) })
}

func TestATapFromAStrangerIsIgnored(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)
	r.PressAs(999, testrig.UserID, msg.ID, dataOf(msg, "1. Yes")) // another chat
	r.PressAs(testrig.ChatID, 999, msg.ID, dataOf(msg, "1. Yes")) // another user
	r.PressAs(testrig.ChatID, 999, msg.ID, "s:laptop")            // asks for the screen
	r.Eventually("counted", func() bool { return r.Hub.Stats().Unauthorized >= 3 })
	r.Never("a stranger's tap acted", 300*time.Millisecond, func() bool {
		return len(a.Exec.Keys()) > 0 || len(r.SentWithButtons()) > 1 || len(r.TG.Answered()) > 0 || len(r.TG.Cleared()) > 0
	})
}

// A button of one agent's notice goes to that agent.
func TestATapGoesToTheAgentWhoseNoticeItIs(t *testing.T) {
	r := uiRig(t, "laptop", "nas")
	laptop, nas := r.Agents["laptop"], r.Agents["nas"]
	r.Say("/agent laptop")
	r.WaitSent("Active agent: laptop")
	nas.Screen.Set(screentest.Question(1))
	hookNotice(t, nas)
	msg := nthButtons(r, 1)
	if !strings.HasPrefix(msg.Text, "[nas] ") || !strings.HasPrefix(dataOf(msg, "2. Blue"), "k:nas:2:") {
		t.Fatalf("%+v", msg)
	}
	r.PressOn(msg.ID, dataOf(msg, "2. Blue"))
	r.Eventually("nas pressed 2", func() bool { return slices.Equal(nas.Exec.Keys(), []string{"2"}) })
	if len(laptop.Exec.Keys()) != 0 {
		t.Fatal("the active agent got the key of the other agent's prompt")
	}
}

func TestTheScreenButtonAsksForTheScreenAndKeepsItsButtons(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Spool.AddItem(spoolNotice("⏳ No reply for a while", screenButton()))
	msg := nthButtons(r, 1)
	if dataOf(msg, "📺") != "s:laptop" {
		t.Fatalf("%+v", msg.Buttons)
	}
	a.Screen.Set(screentest.Resume())
	r.PressOn(msg.ID, "s:laptop")
	r.Eventually("told", func() bool { return answered(r, "Looking…") })
	r.WaitSent("laptop's screen:")
	if len(r.TG.Cleared()) != 0 {
		t.Fatal("asking for the screen used up the buttons")
	}
	r.PressOn(msg.ID, "s:laptop") // and may be asked again
	r.Eventually("asked twice", func() bool { return strings.Count(strings.Join(r.TG.Answered(), "\n"), "Looking…") == 2 })
}

func TestEachChatCommandReachesTheSession(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Resume())

	r.Say("/stop")
	r.Eventually("Escape", func() bool { return slices.Equal(a.Exec.Keys(), []string{"Escape"}) })
	r.Say("/clear")
	r.Say("/compact")
	r.Say("/compact keep the failing tests")
	r.Say("/sessions")
	r.Eventually("the texts", func() bool { return len(a.Exec.Texts()) == 4 })
	want := []string{"/clear", "/compact", "/compact keep the failing tests", "List all your sessions (use ListAgents) in a short table."}
	if got := a.Exec.Texts(); !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}

	r.Say("/screen")
	r.WaitSent("laptop's screen:")
	snapshot := r.SentWithButtons()[0]
	if !strings.Contains(snapshot.Text, "Resume Session") || dataOf(snapshot, "↓") != "k:laptop:Down:" || dataOf(snapshot, "⏎") != "k:laptop:Enter:" {
		t.Fatalf("%+v", snapshot)
	}
	if len(a.Exec.Keys()) != 1 || len(a.Exec.Texts()) != 4 {
		t.Fatal("asking for the screen typed something")
	}
}

// /resume types the command and then shows the list it opened, so that the session can be chosen from the phone.
func TestResumeShowsTheListOfSessions(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Resume())
	r.Say("/resume")
	r.Eventually("typed", func() bool { return slices.Equal(a.Exec.Texts(), []string{"/resume"}) })
	start := time.Now()
	r.WaitSent("laptop's screen:")
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Errorf("the screen was read %v after the command, before the session could draw the list", took)
	}
	if !r.Sent("fix the build") || len(r.SentWithButtons()) != 1 {
		t.Fatalf("%q", r.SentTexts())
	}
}

// The commands act on the agent the message is for: a swipe-reply, else the active one; with several agents and none
// active the hub asks and types nothing.
func TestChatCommandsFollowTheUsualRoutingOfCommands(t *testing.T) {
	r := uiRig(t, "laptop", "nas")
	laptop, nas := r.Agents["laptop"], r.Agents["nas"]
	for _, c := range []string{"/stop", "/clear", "/compact", "/resume", "/sessions", "/screen"} {
		r.Say(c)
	}
	r.Eventually("asked for each", func() bool { return strings.Count(strings.Join(r.SentTexts(), "\n"), "No agent is chosen") == 6 })
	r.Never("something reached an agent that was not chosen", 300*time.Millisecond, func() bool {
		return len(laptop.Exec.Keys())+len(laptop.Exec.Texts())+len(nas.Exec.Keys())+len(nas.Exec.Texts()) > 0 || r.Sent("screen:")
	})

	r.Say("/agent nas")
	r.WaitSent("Active agent: nas")
	r.Say("/stop")
	r.Eventually("nas got Escape", func() bool { return slices.Equal(nas.Exec.Keys(), []string{"Escape"}) })

	laptop.Spool.Add(wire.TypeReply, "laptop says hi")
	r.WaitSent("[laptop] laptop says hi")
	r.SayReplyTo("/clear", r.MessageID("laptop says hi"))
	r.Eventually("laptop got /clear", func() bool { return slices.Equal(laptop.Exec.Texts(), []string{"/clear"}) })
	if len(nas.Exec.Texts()) != 0 || len(laptop.Exec.Keys()) != 0 {
		t.Fatal("a command went to the wrong agent")
	}
}

// A command that waited in the chat (the hub was away) is not acted on when it is finally read.
func TestAnOldChatCommandIsNotCarriedOut(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	r.SayAt("/stop", time.Now().Add(-10*time.Minute))
	r.SayAt("/clear", time.Now().Add(-10*time.Minute))
	r.SayAt("/screen", time.Now().Add(-10*time.Minute))
	r.Eventually("told", func() bool { return strings.Count(strings.Join(r.SentTexts(), "\n"), "older than its validity") == 3 })
	r.Never("an old command carried out", 300*time.Millisecond, func() bool {
		return len(a.Exec.Keys())+len(a.Exec.Texts()) > 0 || len(r.SentWithButtons()) > 0
	})
}

func TestTheMenuOffersOnlyWhatIsHarmlessToTap(t *testing.T) {
	r := uiRig(t, "laptop")
	r.Eventually("the menu", func() bool { return len(r.TG.Commands()) > 0 })
	var names []string
	for _, c := range r.TG.Commands() {
		names = append(names, c.Name)
	}
	for _, want := range []string{"screen", "sessions", "stop"} {
		if !slices.Contains(names, want) {
			t.Errorf("/%s is not in the menu: %v", want, names)
		}
	}
	for _, not := range []string{"clear", "compact", "resume"} {
		if slices.Contains(names, not) {
			t.Errorf("/%s is in the menu: one tap would change the session", not)
		}
	}
	// what the menu sends is understood
	for _, n := range []string{"screen", "sessions", "stop"} {
		r.Say("/" + n)
	}
	r.WaitSent("laptop's screen:")
	r.Eventually("typed", func() bool {
		return len(r.Agents["laptop"].Exec.Texts()) == 1 && len(r.Agents["laptop"].Exec.Keys()) == 1
	})
	if r.Sent("Unknown command") {
		t.Fatalf("%q", r.SentTexts())
	}
}
