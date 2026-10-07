package e2e

import (
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/handshake"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func one(t *testing.T, opts testrig.Options) (*testrig.Rig, *testrig.AgentRig) {
	t.Helper()
	r := testrig.New(t, opts)
	r.StartAll()
	return r, r.Agents["laptop"]
}

func TestConnectRelayAndReply(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.WaitSent("laptop is connected")
	r.Say("hello claude")
	r.Eventually("the text to be typed", func() bool { return len(a.Exec.Texts()) == 1 })
	if a.Exec.Texts()[0] != "hello claude" {
		t.Fatalf("typed %q", a.Exec.Texts())
	}
	r.Say("/enter")
	r.Say("/esc")
	r.Say("/key 2")
	r.Eventually("three keys", func() bool { return len(a.Exec.Keys()) == 3 })
	if got := strings.Join(a.Exec.Keys(), ","); got != "Enter,Escape,2" {
		t.Fatalf("keys %s", got)
	}
	a.Spool.Add(wire.TypeReply, "the answer is 42")
	r.WaitSent("the answer is 42")
	if r.Sent("[laptop]") {
		t.Fatal("a single agent must not add a tag")
	}
}

func TestRepliesKeepOrder(t *testing.T) {
	r, a := one(t, testrig.Options{})
	for _, s := range []string{"first", "second", "third", "fourth"} {
		a.Spool.Add(wire.TypeReply, s)
	}
	r.Eventually("four replies", func() bool {
		n := 0
		for _, s := range r.SentTexts() {
			if s == "first" || s == "second" || s == "third" || s == "fourth" {
				n++
			}
		}
		return n == 4
	})
	var order []string
	for _, s := range r.SentTexts() {
		if s == "first" || s == "second" || s == "third" || s == "fourth" {
			order = append(order, s)
		}
	}
	if strings.Join(order, ",") != "first,second,third,fourth" {
		t.Fatalf("order: %v", order)
	}
}

func TestLargeReplyIsCut(t *testing.T) {
	r, a := one(t, testrig.Options{})
	a.Spool.Add(wire.TypeReply, strings.Repeat("x", 100_000))
	r.WaitSent("[cut:")
}

func TestOnlyTheAuthorisedUserIsObeyed(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.SayAs(999, testrig.UserID, "wrong chat")
	r.SayAs(testrig.ChatID, 999, "wrong user")
	r.SayAs(999, 999, "neither")
	r.Say("legit")
	r.Eventually("the legitimate text", func() bool { return len(a.Exec.Texts()) == 1 })
	if a.Exec.Texts()[0] != "legit" {
		t.Fatalf("typed %q", a.Exec.Texts())
	}
	r.Eventually("counted", func() bool { return r.Hub.Stats().Unauthorized == 3 })
}

func TestUnknownCommandsAndEmptyInput(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Say("   ")
	r.Say("/nonsense")
	r.Say("@ghost hello")
	r.Say("@laptop")
	r.Say("/agent ghost")
	r.WaitSent("Unknown command")
	r.WaitSent("Unknown agent")
	time.Sleep(200 * time.Millisecond)
	if len(a.Exec.Texts()) != 0 || len(a.Exec.Keys()) != 0 {
		t.Fatal("something was typed")
	}
}

// ---- attacks on the bus ----

func TestReplayOfEveryCapturedMessageIsHarmless(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Say("run exactly once")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	time.Sleep(300 * time.Millisecond)
	before := a.Agent.Stats().Rejected
	captured := r.Mem.Published(r.Names.ToAgent("laptop"))
	if len(captured) < 2 {
		t.Fatalf("captured only %d messages", len(captured))
	}
	for i := 0; i < 3; i++ {
		for _, p := range captured {
			r.Mem.Inject(r.Names.AgentSub("laptop"), p.Data, p.Attrs)
		}
	}
	r.Eventually("replays rejected", func() bool {
		return a.Agent.Stats().Rejected+a.Agent.Stats().Duplicates >= before+uint64(len(captured))
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(a.Exec.Texts()); n != 1 {
		t.Fatalf("a replayed command ran: %d executions", n)
	}
	r.Say("still works")
	r.Eventually("session survived the replay", func() bool { return len(a.Exec.Texts()) == 2 })
}

func TestReplayOfAgentMessagesToTheHub(t *testing.T) {
	r, a := one(t, testrig.Options{})
	a.Spool.Add(wire.TypeReply, "one-time reply")
	r.WaitSent("one-time reply")
	time.Sleep(300 * time.Millisecond)
	for _, p := range r.Mem.Published(r.Names.ToHub()) {
		if p.Attrs[wire.AttrKind] == wire.KindMsg {
			r.Mem.Inject(r.Names.HubSub(), p.Data, p.Attrs)
			r.Mem.Inject(r.Names.HubSub(), p.Data, p.Attrs)
		}
	}
	time.Sleep(400 * time.Millisecond)
	n := 0
	for _, s := range r.SentTexts() {
		if s == "one-time reply" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the reply was shown %d times", n)
	}
}

func junk(n int) []byte { b := make([]byte, n); rand.Read(b); return b }

func TestForgedFloodNeverBreaksTheSession(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Say("before")
	r.Eventually("before", func() bool { return len(a.Exec.Texts()) == 1 })
	// a real session id, so the junk gets past the cheap check and reaches the ratchet
	var sid []byte
	for _, p := range r.Mem.Published(r.Names.ToAgent("laptop")) {
		if p.Attrs[wire.AttrKind] == wire.KindMsg {
			sid = p.Data[1:9]
			break
		}
	}
	for i := 0; i < 300; i++ {
		data := append(append([]byte{wire.Version}, sid...), junk(1200)...)
		if i%2 == 0 {
			if k, err := ecdhKey(); err == nil {
				copy(data[9:], k) // a fresh public key in the ratchet header forces the key-step path
			}
		}
		r.Mem.Inject(r.Names.AgentSub("laptop"), data, wire.Attrs(wire.KindMsg, "laptop"))
		r.Mem.Inject(r.Names.HubSub(), data, wire.Attrs(wire.KindMsg, "laptop"))
		r.Mem.Inject(r.Names.AgentSub("laptop"), junk(50+i), wire.Attrs(wire.KindMsg, "laptop"))
		r.Mem.Inject(r.Names.AgentSub("laptop"), junk(50+i), map[string]string{"kind": "weird"})
	}
	r.Eventually("junk rejected", func() bool { return a.Agent.Stats().Rejected >= 600 })
	// The agent reported "before" as done, so the hub's next message carries a new key, and the flood has used up the
	// agent's budget of new-key agreements (20 at once, then 5 a second). A real message that needs a new key is refused
	// until the budget has refilled (see the limitations); the session itself is not harmed, which is what this checks.
	time.Sleep(time.Second)
	r.Say("after the flood")
	r.Eventually("the real session survived", func() bool { return len(a.Exec.Texts()) == 2 })
	if a.Exec.Texts()[1] != "after the flood" || a.Agent.Stats().Executed != 2 {
		t.Fatalf("typed %q", a.Exec.Texts())
	}
}

func TestForgedHellosAreRefusedAndTheSessionSurvives(t *testing.T) {
	r, a := one(t, testrig.Options{})
	stranger, _ := identity.Generate("stranger")
	impostor, _ := identity.Generate("laptop") // the right name with the wrong keys
	for _, who := range []*identity.Identity{stranger, impostor} {
		_, hello, err := handshake.NewInitiator(who, r.HubID.Public(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		data, _ := hello.Encode()
		for i := 0; i < 5; i++ {
			r.Mem.Inject(r.Names.HubSub(), data, wire.Attrs(wire.KindHello, "laptop"))
		}
	}
	r.Eventually("forged hellos refused", func() bool { return r.Hub.Stats().Rejected+r.Hub.Stats().HellosRefused >= 10 })
	r.Say("still mine")
	r.Eventually("session intact", func() bool { return len(a.Exec.Texts()) == 1 })
}

func TestReplayedRealHelloCannotResetTheSession(t *testing.T) {
	r, a := one(t, testrig.Options{})
	var hello []byte
	for _, p := range r.Mem.Published(r.Names.ToHub()) {
		if p.Attrs[wire.AttrKind] == wire.KindHello {
			hello = p.Data
			break
		}
	}
	if hello == nil {
		t.Fatal("no hello was captured")
	}
	hellosBefore := r.Hub.Stats().Hellos
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 10; i++ {
		r.Mem.Inject(r.Names.HubSub(), hello, wire.Attrs(wire.KindHello, "laptop"))
	}
	time.Sleep(400 * time.Millisecond)
	if got := r.Hub.Stats().Hellos; got != hellosBefore {
		t.Fatalf("a replayed hello was accepted (%d -> %d)", hellosBefore, got)
	}
	r.Say("alive")
	r.Eventually("session intact", func() bool { return len(a.Exec.Texts()) == 1 })
}

func TestAnAckForAnotherHelloIsIgnored(t *testing.T) {
	r, a := one(t, testrig.Options{})
	// a second, unrelated party asks the hub for a session and the (genuine) ack is replayed to the agent
	var ack []byte
	for _, p := range r.Mem.Published(r.Names.ToAgent("laptop")) {
		if p.Attrs[wire.AttrKind] == wire.KindAck {
			ack = p.Data
		}
	}
	if ack == nil {
		t.Fatal("no ack captured")
	}
	for i := 0; i < 5; i++ {
		r.Mem.Inject(r.Names.AgentSub("laptop"), ack, wire.Attrs(wire.KindAck, "laptop"))
	}
	time.Sleep(300 * time.Millisecond)
	r.Say("ok")
	r.Eventually("session intact", func() bool { return len(a.Exec.Texts()) == 1 })
	if a.Agent.Stats().Handshakes != 1 {
		t.Fatalf("handshakes = %d", a.Agent.Stats().Handshakes)
	}
}

func TestAWrongSessionCommandIsRefused(t *testing.T) {
	r, a := one(t, testrig.Options{})
	// an attacker with publish rights who negotiated their own ratchet cannot pass as the hub
	sk := [32]byte{}
	rand.Read(sk[:])
	key, _ := ecdhKeyPair()
	atk, _ := ratchet.NewInitiator(sk, key.PublicKey().Bytes())
	now := time.Now()
	var sid [8]byte
	copy(sid[:], "attacker")
	data, err := wire.Seal(atk, "hub", "laptop", sid, &wire.Payload{ID: "x", Type: wire.TypeText, Body: "rm -rf ~", TS: now.Unix(), Exp: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	r.Mem.Inject(r.Names.AgentSub("laptop"), data, wire.Attrs(wire.KindMsg, "laptop"))
	r.Eventually("rejected", func() bool { return a.Agent.Stats().Rejected >= 1 })
	time.Sleep(200 * time.Millisecond)
	if len(a.Exec.Texts()) != 0 {
		t.Fatal("an attacker's command was executed")
	}
}

// ---- time ----

func TestDelayedCommandsExpireInsteadOfRunning(t *testing.T) {
	clock := testrig.NewClock()
	r, a := one(t, testrig.Options{Clock: clock, SilenceLimit: 24 * time.Hour, OfflineAfter: 24 * time.Hour})
	r.Mem.Hold(r.Names.AgentSub("laptop"))
	r.Say("late command")
	r.Eventually("relayed", func() bool { return r.Hub.Stats().Relayed == 1 })
	clock.Advance(11 * time.Minute) // longer than the text time-to-live
	r.Mem.Release(r.Names.AgentSub("laptop"), nil)
	r.WaitSent("expired before it could run")
	time.Sleep(200 * time.Millisecond)
	if len(a.Exec.Texts()) != 0 {
		t.Fatal("a stale command was executed")
	}
	if a.Agent.Stats().Expired != 1 {
		t.Fatalf("expired = %d", a.Agent.Stats().Expired)
	}
}

func TestReorderedCommandsAreAllExecutedOnce(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Mem.Hold(r.Names.AgentSub("laptop"))
	r.Say("alpha")
	r.Eventually("1", func() bool { return r.Hub.Stats().Relayed == 1 })
	r.Say("beta")
	r.Eventually("2", func() bool { return r.Hub.Stats().Relayed == 2 })
	r.Mem.Release(r.Names.AgentSub("laptop"), func(n int) []int {
		idx := make([]int, n)
		for i := range idx {
			idx[i] = n - 1 - i
		}
		return idx
	})
	r.Eventually("both typed", func() bool { return len(a.Exec.Texts()) == 2 })
	got := strings.Join(a.Exec.Texts(), ",")
	if got != "alpha,beta" && got != "beta,alpha" {
		t.Fatalf("typed %s", got)
	}
}

func TestLostMessagesDoNotStopLaterOnes(t *testing.T) {
	r, a := one(t, testrig.Options{})
	n := 0
	r.Mem.SetDrop(func(p transportPublished) bool {
		if p.Topic == r.Names.ToAgent("laptop") && p.Attrs[wire.AttrKind] == wire.KindMsg {
			n++
			return n == 2 || n == 3 // lose two messages in a row
		}
		return false
	})
	for _, s := range []string{"m1", "m2", "m3", "m4"} {
		r.Say(s)
		time.Sleep(60 * time.Millisecond)
	}
	r.Eventually("later messages still arrive", func() bool { return len(a.Exec.Texts()) >= 1 })
	time.Sleep(600 * time.Millisecond)
	r.Mem.SetDrop(nil)
	r.Say("after the loss")
	r.Eventually("session usable after loss", func() bool {
		for _, s := range a.Exec.Texts() {
			if s == "after the loss" {
				return true
			}
		}
		return false
	})
}

// ---- lockdown ----

func TestLockdownIsEasyToStartAndNeedsALocalUnlock(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Say("/lockdown")
	r.WaitSent("Lockdown")
	r.Eventually("agent locked", func() bool { return a.Agent.Locked() })
	if _, err := os.Stat(a.LockFile); err != nil {
		t.Fatal("the lock did not persist to disk")
	}
	r.Say("/unlock") // the hub relays again, but the agent is still locked
	r.WaitSent("stay locked")
	r.Say("blocked text")
	r.Say("/enter")
	r.WaitSent("this machine is locked")
	time.Sleep(200 * time.Millisecond)
	if len(a.Exec.Texts()) != 0 || len(a.Exec.Keys()) != 0 {
		t.Fatal("a locked agent executed something")
	}
	if err := os.Remove(a.LockFile); err != nil { // the local unlock
		t.Fatal(err)
	}
	r.Say("after unlock")
	r.Eventually("typed after local unlock", func() bool { return len(a.Exec.Texts()) == 1 })
}

func TestLockSurvivesAnAgentRestart(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Say("/lockdown")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	r.StartAgent("laptop")
	r.WaitConnected("laptop")
	if !r.Agents["laptop"].Agent.Locked() {
		t.Fatal("the lock was lost on restart")
	}
}

func TestHubLockdownStopsRelayingEvenToAnUnlockedAgent(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Say("/lockdown")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	os.Remove(a.LockFile)
	r.Say("nothing should go out")
	r.WaitSent("Lockdown is on")
	time.Sleep(200 * time.Millisecond)
	if len(a.Exec.Texts()) != 0 {
		t.Fatal("the hub relayed during lockdown")
	}
}

// ---- liveness and recovery ----

func TestOfflineAndBackOnline(t *testing.T) {
	r, a := one(t, testrig.Options{OfflineAfter: 400 * time.Millisecond})
	r.StopAgent("laptop")
	r.WaitSent("went offline")
	r.Say("sent while away")
	r.WaitSent("is offline")
	if len(a.Exec.Texts()) != 0 {
		t.Fatal("something was typed")
	}
	r.StartAgent("laptop")
	r.Eventually("back", func() bool { return r.Sent("is connected") && r.Hub.Online("laptop") })
	n := 0
	for _, s := range r.SentTexts() {
		if strings.Contains(s, "went offline") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the offline notice was sent %d times", n)
	}
	r.Say("after return")
	r.Eventually("typed", func() bool { return len(r.Agents["laptop"].Exec.Texts()) == 1 })
}

func TestHubRestartIsHealedByANewHandshake(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.Say("before restart")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	first := a.Agent.Stats().Handshakes
	r.StartHub() // all in-memory sessions are gone
	r.Eventually("a new session", func() bool { return a.Agent.Stats().Handshakes > first && r.Hub.Online("laptop") })
	r.Say("after restart")
	r.Eventually("typed again", func() bool { return len(a.Exec.Texts()) == 2 })
}

func TestAgentRestartGivesAFreshSessionAndKeepsWorking(t *testing.T) {
	r, _ := one(t, testrig.Options{})
	r.StartAgent("laptop")
	r.WaitConnected("laptop")
	r.Say("new life")
	r.Eventually("typed", func() bool { return len(r.Agents["laptop"].Exec.Texts()) == 1 })
}

func TestExecutorFailureIsReportedNotFatal(t *testing.T) {
	r, a := one(t, testrig.Options{})
	a.Exec.Err = errors.New("tmux: no server running")
	r.Say("cannot type this")
	r.WaitSent("could not type it into the session")
	if strings.Contains(strings.Join(r.SentTexts(), "\n"), "no server running") {
		t.Fatal("an internal error text leaked to the chat")
	}
	a.Exec.Err = nil
	r.Say("now it works")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
}

func TestChatOutageDoesNotStopTheBridge(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.TG.SetSendErr(errors.New("chat service down"))
	a.Spool.Add(wire.TypeReply, "lost while the chat is down")
	time.Sleep(400 * time.Millisecond)
	r.TG.SetSendErr(nil)
	r.Say("still relaying")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	a.Spool.Add(wire.TypeReply, "reply after recovery")
	r.WaitSent("reply after recovery")
}

func TestSpoolSurvivesAHubOutageAndIsDeliveredLater(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.StopHub()
	a.Spool.Add(wire.TypeReply, "written while the hub was away")
	time.Sleep(300 * time.Millisecond)
	r.StartHub()
	r.WaitSent("written while the hub was away")
}

// ---- several agents ----

func TestTwoAgentsRoutingAndTags(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.StartAll()
	laptop, nas := r.Agents["laptop"], r.Agents["nas"]
	r.Say("/agent nas")
	r.WaitSent("Active agent: nas")
	r.Say("to the nas")
	r.Eventually("nas typed", func() bool { return len(nas.Exec.Texts()) == 1 })
	r.Say("@laptop to the laptop")
	r.Eventually("laptop typed", func() bool { return len(laptop.Exec.Texts()) == 1 })
	if nas.Exec.Texts()[0] != "to the nas" || laptop.Exec.Texts()[0] != "to the laptop" {
		t.Fatal("messages were routed to the wrong agent")
	}
	laptop.Spool.Add(wire.TypeReply, "from the laptop")
	r.WaitSent("[laptop] from the laptop")
	nas.Spool.Add(wire.TypeReply, "from the nas")
	r.WaitSent("[nas] from the nas")
	r.Say("/agents")
	r.WaitSent("laptop: online")
	r.WaitSent("nas: online")
}

func TestAgentsCannotReadOrActForEachOther(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.StartAll()
	nas := r.Agents["nas"]
	// a command sealed for the laptop's session, delivered to the nas
	r.Say("/agent laptop")
	r.Say("for the laptop only")
	r.Eventually("laptop typed", func() bool { return len(r.Agents["laptop"].Exec.Texts()) == 1 })
	for _, p := range r.Mem.Published(r.Names.ToAgent("laptop")) {
		r.Mem.Inject(r.Names.AgentSub("nas"), p.Data, p.Attrs)
	}
	time.Sleep(400 * time.Millisecond)
	if len(nas.Exec.Texts()) != 0 || len(nas.Exec.Keys()) != 0 {
		t.Fatal("the nas acted on a message meant for the laptop")
	}
}

func TestDisconnectedAgentIsReportedAtOnce(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.StartHub()
	r.StartAgent("laptop")
	r.WaitConnected("laptop")
	r.Say("/agent nas")
	r.Say("anyone there?")
	r.WaitSent("nas is not connected")
}
