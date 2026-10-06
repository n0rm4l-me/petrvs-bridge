package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudebridge/internal/testrig"
	"claudebridge/internal/wire"
)

// quiet options: no heartbeats, so the only traffic on the bus is what a test sends, and nobody is called offline.
func quiet() testrig.Options {
	return testrig.Options{Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour, ReorderWindow: 300 * time.Millisecond}
}

func typed(a *testrig.AgentRig) string { return strings.Join(a.Exec.Texts(), ",") }

// published returns the n most recent sealed messages the hub put on the agent's topic.
func lastMsgs(r *testrig.Rig, n int) []transportPublished {
	var all []transportPublished
	for _, p := range r.Mem.Published(r.Names.ToAgent("laptop")) {
		if p.Attrs[wire.AttrKind] == wire.KindMsg {
			all = append(all, p)
		}
	}
	return all[len(all)-n:]
}

func TestCommandsRunInTheOrderTheyWereTyped(t *testing.T) {
	for name, order := range map[string]func(n int) []int{
		"reversed": func(n int) []int {
			idx := make([]int, n)
			for i := range idx {
				idx[i] = n - 1 - i
			}
			return idx
		},
		"rotated": func(n int) []int {
			idx := make([]int, n)
			for i := range idx {
				idx[i] = (i + 2) % n
			}
			return idx
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, a := one(t, quiet())
			r.Mem.Hold(r.Names.AgentSub("laptop"))
			var want []string
			for i := 1; i <= 6; i++ {
				s := fmt.Sprintf("c%d", i)
				want = append(want, s)
				r.Say(s)
				n := uint64(i)
				r.Eventually("relayed", func() bool { return r.Hub.Stats().Relayed == n })
			}
			r.Mem.Release(r.Names.AgentSub("laptop"), order)
			r.Eventually("all typed", func() bool { return len(a.Exec.Texts()) == 6 })
			if got := typed(a); got != strings.Join(want, ",") {
				t.Fatalf("typed %s, want %s", got, strings.Join(want, ","))
			}
		})
	}
}

func TestKeysAndTextShareOneOrder(t *testing.T) {
	r, a := one(t, quiet())
	r.Mem.Hold(r.Names.AgentSub("laptop"))
	r.Say("first")
	r.Eventually("1", func() bool { return r.Hub.Stats().Relayed == 1 })
	r.Say("/enter")
	r.Eventually("2", func() bool { return r.Hub.Stats().Relayed == 2 })
	r.Say("second")
	r.Eventually("3", func() bool { return r.Hub.Stats().Relayed == 3 })
	r.Mem.Release(r.Names.AgentSub("laptop"), func(n int) []int { return []int{2, 1, 0} })
	r.Eventually("all done", func() bool { return len(a.Exec.Texts()) == 2 && len(a.Exec.Keys()) == 1 })
	if typed(a) != "first,second" || strings.Join(a.Exec.Keys(), ",") != "Enter" {
		t.Fatalf("typed %s keys %v", typed(a), a.Exec.Keys())
	}
}

func TestALostCommandIsReportedAndTheNextOnesStillRun(t *testing.T) {
	r, a := one(t, quiet())
	r.Say("one")
	r.Eventually("one typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.Mem.Hold(r.Names.AgentSub("laptop"))
	r.Say("two")
	r.Eventually("2", func() bool { return r.Hub.Stats().Relayed == 2 })
	r.Say("three")
	r.Eventually("3", func() bool { return r.Hub.Stats().Relayed == 3 })
	r.Mem.Release(r.Names.AgentSub("laptop"), func(n int) []int { return []int{1} }) // "two" never arrives
	r.WaitSent("command #2 did not arrive in time")
	r.Eventually("three typed", func() bool { return len(a.Exec.Texts()) == 2 })
	if typed(a) != "one,three" {
		t.Fatalf("typed %s", typed(a))
	}
	if a.Agent.Stats().Lost != 1 {
		t.Fatalf("lost = %d", a.Agent.Stats().Lost)
	}
}

func TestACommandThatArrivesAfterItsTurnIsNotRun(t *testing.T) {
	r, a := one(t, quiet())
	r.Mem.Hold(r.Names.AgentSub("laptop"))
	r.Say("early")
	r.Eventually("1", func() bool { return r.Hub.Stats().Relayed == 1 })
	r.Say("later")
	r.Eventually("2", func() bool { return r.Hub.Stats().Relayed == 2 })
	msgs := lastMsgs(r, 2)
	r.Mem.Release(r.Names.AgentSub("laptop"), func(n int) []int { return []int{1} }) // only the second gets through
	r.WaitSent("command #1 did not arrive in time")
	r.Eventually("later typed", func() bool { return typed(a) == "later" })
	// the first one turns up after all
	r.Mem.Inject(r.Names.AgentSub("laptop"), msgs[0].Data, msgs[0].Attrs)
	r.WaitSent("command #1 arrived after its turn had passed")
	time.Sleep(200 * time.Millisecond)
	if typed(a) != "later" {
		t.Fatalf("typed %s: a command ran after its turn", typed(a))
	}
	if a.Agent.Stats().Late != 1 {
		t.Fatalf("late = %d", a.Agent.Stats().Late)
	}
}

func TestReadyIsRepeatedWhenTheFirstOneIsLost(t *testing.T) {
	o := quiet()
	o.SilenceLimit = 30 * time.Second
	r := testrig.New(t, o)
	first := true
	r.Mem.SetDrop(func(p transportPublished) bool {
		if p.Topic == r.Names.ToHub() && p.Attrs[wire.AttrKind] == wire.KindMsg && first {
			first = false // the agent's first message of the session, "ready"
			return true
		}
		return false
	})
	r.StartHub()
	r.StartAgent("laptop")
	// with heartbeats an hour apart, only the repeated "ready" can tell the hub the session works
	r.Eventually("connected despite the lost ready", func() bool { return r.Hub.Online("laptop") && r.Agents["laptop"].Agent.Connected() })
	r.Say("works")
	r.Eventually("typed", func() bool { return typed(r.Agents["laptop"]) == "works" })
}

func TestSeveralHellosInFlightStillGiveOneWorkingSession(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartHub()
	sub := r.Names.AgentSub("laptop")
	r.Mem.Hold(sub) // the acks are held back, so the agent repeats its hello
	r.StartAgent("laptop")
	r.Eventually("three hellos answered", func() bool { return r.Hub.Stats().Hellos >= 3 })
	r.Mem.Release(sub, nil) // the first ack reaches the agent first
	r.WaitConnected("laptop")
	time.Sleep(300 * time.Millisecond)
	if n := r.Agents["laptop"].Agent.Stats().Handshakes; n != 1 {
		t.Fatalf("%d handshakes: the hub lost the session the agent chose and the agent had to start again", n)
	}
	r.Say("hello")
	r.Eventually("typed", func() bool { return typed(r.Agents["laptop"]) == "hello" })
}

// ---- lock ----

func TestALockThatCannotBeWrittenStillHoldsAndIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	r := testrig.New(t, testrig.Options{})
	a := r.Agents["laptop"]
	dir := t.TempDir()
	a.SetLockFile(filepath.Join(dir, "state", "locked"))
	os.Mkdir(filepath.Join(dir, "state"), 0o500) // nothing can be created inside
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "state"), 0o700) })
	r.StartAll()
	r.Say("/lockdown")
	r.WaitSent("in memory only")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	for i := 0; i < 3; i++ { // every later check, with no file to find, still says locked
		if !a.Agent.Locked() {
			t.Fatal("the lock vanished because its file could not be written")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.Say("/unlock")
	r.Say("text while locked")
	r.WaitSent("this machine is locked")
	if len(a.Exec.Texts()) != 0 {
		t.Fatal("a locked agent typed something")
	}
}

func TestALockWhoseFileCannotBeCheckedFailsClosed(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	a := r.Agents["laptop"]
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	os.WriteFile(notADir, []byte("x"), 0o600)
	a.SetLockFile(filepath.Join(notADir, "locked")) // stat says "not a directory", not "does not exist"
	r.StartAll()
	if !a.Agent.Locked() {
		t.Fatal("an agent that cannot check its lock must be locked")
	}
	r.Say("hello")
	r.WaitSent("this machine is locked")
}

func TestAHubLockdownIsRepeatedToAnAgentThatReconnects(t *testing.T) {
	o := testrig.Options{HubLockFile: true}
	r, a := one(t, o)
	r.Say("/lockdown")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	os.Remove(a.LockFile) // the owner unlocks at the machine ...
	r.Eventually("unlocked", func() bool { return !a.Agent.Locked() })
	r.StartHub() // ... and the hub, which still has the lockdown on disk, restarts
	r.WaitConnected("laptop")
	r.Eventually("the restarted hub locks the agent again", func() bool { return a.Agent.Locked() })
	r.Say("/status")
	r.WaitSent("lockdown")
}

func TestOldChatMessagesAreNotActedOn(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.SayAt("yesterday's command", time.Now().Add(-11*time.Minute))
	r.WaitSent("already older than its validity")
	r.SayAt("/enter", time.Now().Add(-2*time.Minute)) // keys live one minute
	r.Eventually("two refusals", func() bool {
		n := 0
		for _, s := range r.SentTexts() {
			if strings.Contains(s, "already older than its validity") {
				n++
			}
		}
		return n == 2
	})
	r.SayAt("recent enough", time.Now().Add(-9*time.Minute))
	r.Eventually("typed", func() bool { return typed(a) == "recent enough" })
	if len(a.Exec.Keys()) != 0 {
		t.Fatal("an old key press was sent")
	}
}

func TestLockdownIsObeyedWhateverItsAge(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.SayAt("/lockdown", time.Now().Add(-5*time.Hour))
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
}

func TestAnOldUnlockIsIgnored(t *testing.T) {
	r, _ := one(t, testrig.Options{})
	r.Say("/lockdown")
	r.WaitSent("Lockdown:")
	r.SayAt("/unlock", time.Now().Add(-time.Hour))
	r.WaitSent("Ignored an old /unlock")
	r.Say("x")
	r.WaitSent("Lockdown is on")
}

func TestExpiryCountsFromWhenTheUserTypedIt(t *testing.T) {
	r, a := one(t, testrig.Options{})
	// typed 9m50s ago, valid for 10 minutes: it reaches the agent with 10 seconds left, and the agent (whose skew
	// allowance is 30 s) runs it; typed 10m10s ago it would already be refused by the hub.
	r.SayAt("nearly stale", time.Now().Add(-9*time.Minute-50*time.Second))
	r.Eventually("typed", func() bool { return typed(a) == "nearly stale" })
}
