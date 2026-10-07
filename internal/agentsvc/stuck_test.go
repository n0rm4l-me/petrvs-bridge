package agentsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

const machStuckAfter = 10 * time.Minute

func machStuckRig(t *testing.T) *machRig {
	t.Helper()
	return newMachRig(t, Extras{StuckAfter: machStuckAfter})
}

// ask is a text that the hub sent and the agent typed: the clock starts before the typing, as execute does it.
func (r *machRig) ask(text string) {
	r.a.stuckRequested(text)
}

func (r *machRig) reply() {
	r.a.noteSpooled(spool.Item{Type: wire.TypeReply, Body: "the answer"})
}

// stuckNotices are the alerts in the spool.
func (r *machRig) stuckNotices(t *testing.T) []spool.Item {
	t.Helper()
	var out []spool.Item
	for _, it := range r.items(t) {
		if it.Type == wire.TypeNotice && strings.HasPrefix(it.Body, "⏳ No reply") {
			out = append(out, it)
		}
	}
	return out
}

func TestARequestWithoutAReplyIsSaidAfterTheTimeWithTheTwoButtons(t *testing.T) {
	r := machStuckRig(t)
	r.ask("please refactor the parser")
	r.stuck(machStuckAfter - time.Second)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("said before the time: %q", n[0].Body)
	}
	r.stuck(2 * time.Second)
	n := r.stuckNotices(t)
	if len(n) != 1 {
		t.Fatalf("%d alerts", len(n))
	}
	want := "⏳ No reply to «please refactor the parser» for 10 min. Claude Code looks idle. /screen to look, /stop to interrupt."
	if n[0].Body != want {
		t.Fatalf("%q", n[0].Body)
	}
	if len(n[0].Buttons) != 2 || n[0].Buttons[0] != (wire.Button{Label: "📺 Screen", Key: wire.ButtonScreen}) || n[0].Buttons[1] != (wire.Button{Label: "✖ Stop", Key: "Escape"}) {
		t.Fatalf("%+v", n[0].Buttons)
	}
	if err := (&wire.Payload{ID: "x", Type: wire.TypeNotice, Body: n[0].Body, Buttons: n[0].Buttons, Exp: 1}).Validate(true); err != nil {
		t.Fatalf("the hub would refuse it: %v", err)
	}
}

func TestTheAlertSaysBusyWhenClaudeCodeOffersToBeInterrupted(t *testing.T) {
	r := machStuckRig(t)
	r.pane.set(func(f *machPane) { f.screen = machBusyScreen })
	r.ask("build it")
	r.stuck(machStuckAfter)
	n := r.stuckNotices(t)
	if len(n) != 1 || !strings.Contains(n[0].Body, "Claude Code looks busy.") {
		t.Fatalf("%+v", n)
	}
}

func TestTheAlertSaysWhenTheSessionIsGoneOrTheScreenCannotBeRead(t *testing.T) {
	r := machStuckRig(t)
	r.pane.set(func(f *machPane) { f.gone = true })
	r.ask("x")
	r.stuck(machStuckAfter)
	if n := r.stuckNotices(t); len(n) != 1 || !strings.Contains(n[0].Body, "The tmux session is gone.") || strings.Contains(n[0].Body, "looks") {
		t.Fatalf("%+v", n)
	}
	r = machStuckRig(t)
	r.pane.set(func(f *machPane) { f.capErr = errors.New("tmux failed") })
	r.ask("x")
	r.stuck(machStuckAfter)
	if n := r.stuckNotices(t); len(n) != 1 || !strings.Contains(n[0].Body, "The screen cannot be read.") {
		t.Fatalf("%+v", n)
	}
}

func TestWithoutAScreenTheAlertStillGoesButSaysNothingAboutTheScreen(t *testing.T) {
	r := machStuckRig(t)
	r.a.cfg.Screen = nil
	r.ask("x")
	r.stuck(machStuckAfter)
	n := r.stuckNotices(t)
	if len(n) != 1 || strings.Contains(n[0].Body, "looks") || strings.Contains(n[0].Body, "screen cannot") {
		t.Fatalf("%+v", n)
	}
}

func TestAReplyStopsTheClock(t *testing.T) {
	r := machStuckRig(t)
	r.ask("question")
	r.stuck(5 * time.Minute)
	r.reply()
	r.stuck(time.Hour)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("said after the reply: %q", n[0].Body)
	}
	// A reply with no request waiting is harmless, and the next request starts a fresh clock.
	r.reply()
	r.ask("another")
	r.stuck(machStuckAfter - time.Minute)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatal("the clock of the earlier request was kept")
	}
}

func TestAReplyThatArrivesWhileTheScreenIsBeingReadCancelsTheAlert(t *testing.T) {
	r := machStuckRig(t)
	r.ask("x")
	r.pane.set(func(f *machPane) { f.onCap = r.reply })
	r.stuck(machStuckAfter)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("said although the reply came first: %q", n[0].Body)
	}
}

func TestANewRequestWhileTheScreenIsBeingReadIsNotAlertedForAsTheOldOne(t *testing.T) {
	r := machStuckRig(t)
	r.ask("old")
	r.pane.set(func(f *machPane) { f.onCap = func() { r.ask("new") } })
	r.stuck(machStuckAfter)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("%q", n[0].Body)
	}
}

func machPromptNotice() spool.Item {
	return spool.Item{Type: wire.TypeNotice, Body: "⏳ Claude needs your permission:\n...", Buttons: []wire.Button{
		{Label: "1. Yes", Key: "1", Prompt: "ab12cd34"}, {Label: "✖ Esc", Key: "Escape", Prompt: "ab12cd34"}}}
}

func TestAPromptThatWaitsForTheUserPausesTheClockUntilTheNextCommand(t *testing.T) {
	r := machStuckRig(t)
	r.ask("deploy")
	r.a.noteSpooled(machPromptNotice())
	r.stuck(3 * time.Hour)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("said while a prompt waited: %q", n[0].Body)
	}
	// Navigating inside the prompt does not answer it.
	r.a.noteCommand(&wire.Payload{Type: wire.TypeKey, Body: "Down"})
	r.stuck(3 * time.Hour)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatal("Down resumed the clock")
	}
	// The answer resumes it, from that moment.
	r.a.noteCommand(&wire.Payload{Type: wire.TypeKey, Body: "1"})
	r.stuck(machStuckAfter - time.Minute)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatal("the clock did not start again from the answer")
	}
	r.stuck(2 * time.Minute)
	if n := r.stuckNotices(t); len(n) != 1 {
		t.Fatalf("%d alerts after the answer and the time", len(n))
	}
}

func TestAnAnsweredPromptThatIsSentAgainDoesNotPauseTheClockAgain(t *testing.T) {
	r := machStuckRig(t)
	r.ask("deploy")
	r.a.noteSpooled(machPromptNotice())
	r.a.noteCommand(&wire.Payload{Type: wire.TypeKey, Body: "Enter"})
	r.a.noteSpooled(machPromptNotice()) // the receipt was lost and the spool sends it again
	r.stuck(machStuckAfter)
	if n := r.stuckNotices(t); len(n) != 1 {
		t.Fatalf("the resend paused the clock: %d alerts", len(n))
	}
}

func TestAPromptThatIsOnTheScreenMeansNoAlertAndTheAlertComesWhenItIsGone(t *testing.T) {
	r := machStuckRig(t)
	r.pane.set(func(f *machPane) { f.screen = machPromptScreen })
	r.ask("remove the build")
	r.stuck(machStuckAfter)
	r.stuck(time.Hour)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("said while the user had a prompt: %q", n[0].Body)
	}
	r.pane.set(func(f *machPane) { f.screen = machBusyScreen })
	r.stuck(time.Second)
	if n := r.stuckNotices(t); len(n) != 1 {
		t.Fatal("no alert once the prompt was gone")
	}
}

func TestTheAlertIsRepeatedThreeTimesEachAfterTheTimeAndThenNeverAgainUntilANewRequest(t *testing.T) {
	r := machStuckRig(t)
	r.ask("x")
	for i := 1; i <= 3; i++ {
		r.stuck(machStuckAfter - time.Second)
		if n := len(r.stuckNotices(t)); n != i-1 {
			t.Fatalf("round %d: %d alerts before the time", i, n)
		}
		r.stuck(time.Second)
		if n := len(r.stuckNotices(t)); n != i {
			t.Fatalf("round %d: %d alerts", i, n)
		}
		// The agent's spool sends the alert; its buttons must not look like a prompt that pauses the clock.
		es, _ := r.sp.List()
		r.a.noteSpooled(es[len(es)-1].Item)
	}
	for i := 0; i < 100; i++ {
		r.stuck(machStuckAfter)
	}
	n := r.stuckNotices(t)
	if len(n) != 3 {
		t.Fatalf("%d alerts, want 3", len(n))
	}
	for i, want := range []string{"for 10 min", "for 20 min", "for 30 min"} {
		if !strings.Contains(n[i].Body, want) {
			t.Errorf("alert %d: %q", i+1, n[i].Body)
		}
	}
	r.ask("a new one")
	r.stuck(machStuckAfter)
	if n := r.stuckNotices(t); len(n) != 4 {
		t.Fatalf("a new request did not get its alert: %d", len(n))
	}
}

func TestNothingHappensWhenTheCheckIsOff(t *testing.T) {
	r := newMachRig(t, Extras{})
	r.ask("x")
	if r.a.mach.req != nil {
		t.Fatal("a request is kept although the check is off")
	}
	r.stuck(48 * time.Hour)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatal("an alert with StuckAfter == 0")
	}
	// The loop is not even started.
	ctx, cancel := context.WithCancel(context.Background())
	r.a.mach.stuckEvery = 5 * time.Millisecond
	r.a.startExtras(ctx)
	time.Sleep(40 * time.Millisecond)
	cancel()
	r.a.mach.wg.Wait()
	if r.pane.captures != 0 {
		t.Fatal("the screen was read")
	}
}

func TestKeysAndFilesDoNotStartAClock(t *testing.T) {
	r := machStuckRig(t)
	r.a.noteCommand(&wire.Payload{Type: wire.TypeKey, Body: "Enter"})
	r.a.noteCommand(&wire.Payload{Type: wire.TypeFile, Body: "a caption"})
	r.a.noteCommand(&wire.Payload{Type: wire.TypeStart})
	r.stuck(24 * time.Hour)
	if r.a.mach.req != nil || len(r.stuckNotices(t)) != 0 {
		t.Fatal("a key, a file or a start made a request")
	}
}

func TestEscapeDropsTheRequestBecauseNothingWillAnswerIt(t *testing.T) {
	r := machStuckRig(t)
	r.ask("x")
	r.stuck(machStuckAfter)
	if len(r.stuckNotices(t)) != 1 {
		t.Fatal("no first alert")
	}
	r.a.noteCommand(&wire.Payload{Type: wire.TypeKey, Body: "Escape"}) // the ✖ Stop button
	for i := 0; i < 5; i++ {
		r.stuck(machStuckAfter)
	}
	if n := r.stuckNotices(t); len(n) != 1 {
		t.Fatalf("alerts went on after the interrupt: %d", len(n))
	}
}

func TestAnotherKeyLeavesTheClockRunning(t *testing.T) {
	r := machStuckRig(t)
	r.ask("x")
	r.a.noteCommand(&wire.Payload{Type: wire.TypeKey, Body: "Up"})
	r.stuck(machStuckAfter)
	if len(r.stuckNotices(t)) != 1 {
		t.Fatal("a key that is no interrupt stopped the clock")
	}
}

func TestClaudeCodesOwnCommandsStartNoClockButOthersDo(t *testing.T) {
	for text, clock := range map[string]bool{
		"/clear": false, "/compact": false, "/resume": false, "/CLEAR now": false, "  /model opus": false,
		"/review 12": true, "/init": true, "/my-command do it": true, "hello": true, "look at /clear": true,
		"": false, "   ": false,
		// Claude Code's bash mode runs the command itself; no turn of the assistant follows
		"!ls -la": false, "  !pwd": false, "!": false, "hello!": true, "what does !! do": true,
	} {
		r := machStuckRig(t)
		r.ask(text)
		if got := r.a.mach.req != nil; got != clock {
			t.Errorf("%q: clock %v, want %v", text, got, clock)
		}
	}
}

func TestTheRequestInTheAlertIsOneShortLineWithSecretsHidden(t *testing.T) {
	r := machStuckRig(t)
	r.ask("use the token=abcd1234 to call\nthe api and then write a very long explanation of what you did step by step for me")
	r.stuck(machStuckAfter)
	n := r.stuckNotices(t)
	if len(n) != 1 {
		t.Fatal("no alert")
	}
	line := strings.SplitN(n[0].Body, "»", 2)[0]
	if strings.Contains(line, "abcd1234") || strings.Contains(line, "\n") || !strings.Contains(line, "<hidden>") {
		t.Fatalf("%q", line)
	}
	if got := strings.TrimPrefix(line, "⏳ No reply to «"); len([]rune(got)) > previewChars+1 {
		t.Fatalf("%d characters: %q", len([]rune(got)), got)
	}
}

func TestALockedMachineDropsTheRequestInsteadOfAlerting(t *testing.T) {
	r := machStuckRig(t)
	r.ask("x")
	r.lock()
	r.stuck(machStuckAfter)
	r.unlock()
	r.stuck(24 * time.Hour)
	if len(r.stuckNotices(t)) != 0 {
		t.Fatal("alerted about a request from before the lock")
	}
}

func TestTheWaitingFlagOnlyComesFromPromptNotices(t *testing.T) {
	plain := spool.Item{Type: wire.TypeNotice, Body: "♻️ restarted"}
	stuck := spool.Item{Type: wire.TypeNotice, Buttons: []wire.Button{{Label: "📺 Screen", Key: wire.ButtonScreen}, {Label: "✖ Stop", Key: "Escape"}}}
	reply := spool.Item{Type: wire.TypeReply, Buttons: []wire.Button{{Label: "1", Key: "1"}}}
	if waitsForUser(plain) || waitsForUser(stuck) || waitsForUser(reply) || !waitsForUser(machPromptNotice()) {
		t.Fatal("waitsForUser")
	}
}

func TestTheLoopAlertsInTheBackground(t *testing.T) {
	r := machStuckRig(t)
	r.a.cfg.Ext.StuckAfter = 40 * time.Millisecond
	r.a.cfg.Now = time.Now
	r.a.mach.stuckEvery = 10 * time.Millisecond
	r.ask("x")
	ctx, cancel := context.WithCancel(context.Background())
	r.a.startExtras(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for len(r.stuckNotices(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	r.a.mach.wg.Wait()
	if len(r.stuckNotices(t)) == 0 {
		t.Fatal("no alert from the loop")
	}
}
