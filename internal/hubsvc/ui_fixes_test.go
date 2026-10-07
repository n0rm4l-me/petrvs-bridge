package hubsvc_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// The clock is the hub's and the agent's; moving it ages the buttons without waiting. It stands still otherwise, so the
// pacing of batches (which follows it) is off: it would hold the second notice for ever.
func clockRig(t *testing.T) (*testrig.Rig, *testrig.Clock) {
	t.Helper()
	clk := testrig.NewClock()
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop"}, Screen: true, Clock: clk, MinTurn: -1, SilenceLimit: 24 * time.Hour, OfflineAfter: 24 * time.Hour})
	r.StartAll()
	return r, clk
}

const expired = "Those buttons have expired; use /screen."

func TestTheButtonsOfAMessageExpireAfterFifteenMinutes(t *testing.T) {
	r, clk := clockRig(t)
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	hookNotice(t, a)
	old, fresh := nthButtons(r, 1), nthButtons(r, 2)

	clk.Advance(14 * time.Minute)
	r.PressOn(old.ID, dataOf(old, "3. No"))
	r.Eventually("a tap within fifteen minutes is obeyed", func() bool { return slices.Equal(a.Exec.Keys(), []string{"3"}) })

	clk.Advance(2 * time.Minute) // 16 minutes since the second notice
	r.PressOn(fresh.ID, dataOf(fresh, "1. Yes"))
	r.Eventually("told the buttons have expired", func() bool { return answered(r, expired) })
	r.PressOn(fresh.ID, "s:laptop") // the screen button of an old message is just as old
	r.Eventually("told again", func() bool { return strings.Count(strings.Join(r.TG.Answered(), "\n"), expired) == 2 })
	r.Never("an expired tap pressed a key or asked for the screen", 300*time.Millisecond, func() bool {
		return len(a.Exec.Keys()) != 1 || r.Sent("screen**")
	})
	if got := r.TG.Cleared(); !slices.Equal(got, []int64{old.ID}) {
		t.Fatalf("buttons taken off %v, want only the message that was answered", got)
	}

	// what is waiting now is shown with fresh buttons, and they work
	r.Say("/screen")
	snapshot := nthButtons(r, 3)
	r.PressOn(snapshot.ID, dataOf(snapshot, "1. Yes"))
	r.Eventually("the fresh buttons work", func() bool { return slices.Equal(a.Exec.Keys(), []string{"3", "1"}) })
}

// After a restart the hub remembers nothing of what it sent: a tap on an old message, or on one that names none, is
// refused until the message is sent again.
func TestATapOnAMessageTheHubDoesNotRememberIsRefused(t *testing.T) {
	r := uiRig(t, "laptop")
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)
	yes := dataOf(msg, "1. Yes")

	r.PressOn(987654, yes) // a message that was never the hub's
	r.PressOn(0, yes)      // Telegram names the message of a tap; one that does not is not one of ours
	r.PressOn(987654, "s:laptop")
	r.Eventually("told so three times", func() bool { return strings.Count(strings.Join(r.TG.Answered(), "\n"), expired) == 3 })
	r.Never("a key pressed on the word of a message nobody sent", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 0 })

	// the message that was sent is still good: the refusals used up nothing
	r.PressOn(msg.ID, yes)
	r.Eventually("pressed", func() bool { return slices.Equal(a.Exec.Keys(), []string{"1"}) })

	// and once the hub has restarted it is not
	hookNotice(t, a)
	second := nthButtons(r, 2)
	r.StartHub()
	r.WaitConnected("laptop")
	r.PressOn(second.ID, dataOf(second, "1. Yes"))
	r.Eventually("told so after the restart", func() bool { return strings.Count(strings.Join(r.TG.Answered(), "\n"), expired) == 4 })
	r.Never("a key pressed from a message of before the restart", 300*time.Millisecond, func() bool { return len(a.Exec.Keys()) > 1 })
}

// A tap that sent nothing must not say "Sent", must not take the buttons off, and must not use the message up: the
// prompt is still waiting.
func TestATapThatSentNothingKeepsTheButtonsAndSaysSo(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop"}, Screen: true, FaultyBus: true, SilenceLimit: time.Hour, OfflineAfter: time.Hour})
	r.StartAll()
	a := r.Agents["laptop"]
	a.Screen.Set(screentest.Bash(1, "ls"))
	hookNotice(t, a)
	msg := nthButtons(r, 1)

	toAgent := r.Names.ToAgent("laptop")
	r.Faulty.FailPublish(func(topic string) error {
		if topic == toAgent {
			return errors.New("bus down")
		}
		return nil
	})
	r.PressOn(msg.ID, dataOf(msg, "1. Yes"))
	r.PressOn(msg.ID, "s:laptop")
	r.Eventually("told nothing was sent", func() bool { return strings.Count(strings.Join(r.TG.Answered(), "\n"), "Not sent") == 2 })
	r.WaitSent("The message bus did not accept the message")
	if answered(r, "Sent 1") || answered(r, "Looking") || len(r.TG.Cleared()) != 0 || len(a.Exec.Keys()) != 0 {
		t.Fatalf("answers %q, cleared %v, keys %v", r.TG.Answered(), r.TG.Cleared(), a.Exec.Keys())
	}

	// the bus is back: the same buttons work, once
	r.Faulty.FailPublish(nil)
	r.PressOn(msg.ID, dataOf(msg, "1. Yes"))
	r.Eventually("pressed", func() bool { return answered(r, "Sent 1") && slices.Equal(a.Exec.Keys(), []string{"1"}) })
	r.Eventually("the buttons taken off", func() bool { return slices.Equal(r.TG.Cleared(), []int64{msg.ID}) })
	r.PressOn(msg.ID, dataOf(msg, "3. No"))
	r.Eventually("used up now", func() bool { return answered(r, "Already answered") })
}

// "@name /stop" and the other chat commands act on that agent; only what the hub does not know is typed as text.
func TestAtNameCommandsActOnThatAgent(t *testing.T) {
	r := uiRig(t, "laptop", "nas")
	laptop, nas := r.Agents["laptop"], r.Agents["nas"]
	r.Say("/agent laptop")
	r.WaitSent("Active agent: laptop")
	r.Say("@nas /stop")
	r.Say("@nas /compact keep the tests")
	r.Say("@nas /review this")
	r.Eventually("nas got the key and the texts", func() bool {
		return slices.Equal(nas.Exec.Keys(), []string{"Escape"}) && len(nas.Exec.Texts()) == 2
	})
	if got := nas.Exec.Texts(); got[0] != "/compact keep the tests" || got[1] != "/review this" {
		t.Fatalf("%q", got)
	}
	if len(laptop.Exec.Keys())+len(laptop.Exec.Texts()) != 0 {
		t.Fatal("the active agent got what was meant for another")
	}
}
