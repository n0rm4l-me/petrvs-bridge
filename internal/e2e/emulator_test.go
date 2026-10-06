package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"claudebridge/internal/handshake"
	"claudebridge/internal/testrig"
	"claudebridge/internal/transport"
	"claudebridge/internal/wire"
)

func emuRig(t *testing.T, agents ...string) (*testrig.Rig, *transport.PubSub) {
	t.Helper()
	p, prefix := testrig.Emulator(t, agents...)
	r := testrig.New(t, testrig.Options{Agents: agents, Transport: p, Prefix: prefix, Slow: true, Alarm: true})
	return r, p
}

func TestEmulatorConnectRelayAndReply(t *testing.T) {
	r, _ := emuRig(t)
	r.StartAll()
	a := r.Agents["laptop"]
	r.WaitSent("laptop is connected")
	r.Say("hello over the real bus")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	r.Say("/enter")
	r.Eventually("key", func() bool { return len(a.Exec.Keys()) == 1 })
	a.Spool.Add(wire.TypeReply, "reply over the real bus")
	r.WaitSent("reply over the real bus")
}

func TestEmulatorReplayAndInjectionAreHarmless(t *testing.T) {
	r, p := emuRig(t)
	toAgent, toHub := r.Names.ToAgent("laptop"), r.Names.ToHub()
	tapAgent, tapHub := testrig.NewTap(t, p, toAgent), testrig.NewTap(t, p, toHub)
	r.StartAll()
	a := r.Agents["laptop"]
	r.Say("run once")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	a.Spool.Add(wire.TypeReply, "shown once")
	r.WaitSent("shown once")
	time.Sleep(2 * time.Second)

	ctx := context.Background()
	// republish everything that was captured, twice, in both directions
	for i := 0; i < 2; i++ {
		for _, m := range tapAgent.Messages() {
			if err := p.Publish(ctx, toAgent, m.Data, m.Attrs); err != nil {
				t.Fatal(err)
			}
		}
		for _, m := range tapHub.Messages() {
			if err := p.Publish(ctx, toHub, m.Data, m.Attrs); err != nil {
				t.Fatal(err)
			}
		}
	}
	// and plain junk, including a hello-shaped blob
	for i := 0; i < 50; i++ {
		p.Publish(ctx, toAgent, junk(900), wire.Attrs(wire.KindMsg, "laptop"))
		p.Publish(ctx, toHub, junk(900), wire.Attrs(wire.KindMsg, "laptop"))
		p.Publish(ctx, toHub, junk(300), wire.Attrs(wire.KindHello, "laptop"))
	}
	time.Sleep(3 * time.Second)
	if len(a.Exec.Texts()) != 1 {
		t.Fatalf("a replay ran a command: %d executions", len(a.Exec.Texts()))
	}
	n := 0
	for _, s := range r.SentTexts() {
		if s == "shown once" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the reply was shown %d times", n)
	}
	r.Say("after the attack")
	r.Eventually("the session survived", func() bool { return len(a.Exec.Texts()) == 2 })
}

func TestEmulatorReplayedHelloCannotResetTheSession(t *testing.T) {
	r, p := emuRig(t)
	tapHub := testrig.NewTap(t, p, r.Names.ToHub())
	r.StartAll()
	a := r.Agents["laptop"]
	time.Sleep(time.Second)
	before := r.Hub.Stats().Hellos
	for _, m := range tapHub.Messages() {
		if m.Attrs[wire.AttrKind] == wire.KindHello {
			if _, err := handshake.Decode(m.Data); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				p.Publish(context.Background(), r.Names.ToHub(), m.Data, m.Attrs)
			}
		}
	}
	time.Sleep(2 * time.Second)
	if got := r.Hub.Stats().Hellos; got != before {
		t.Fatalf("hellos %d -> %d: a replayed hello was accepted", before, got)
	}
	r.Say("still here")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
}

func TestEmulatorHubRestartIsHealed(t *testing.T) {
	r, _ := emuRig(t)
	r.StartAll()
	a := r.Agents["laptop"]
	r.Say("before")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
	first := a.Agent.Stats().Handshakes
	r.StartHub()
	r.Eventually("a new session", func() bool { return a.Agent.Stats().Handshakes > first && r.Hub.Online("laptop") })
	r.Say("after")
	r.Eventually("typed again", func() bool { return len(a.Exec.Texts()) == 2 })
}

func TestEmulatorReplyWrittenDuringAHubOutageIsDelivered(t *testing.T) {
	r, _ := emuRig(t)
	r.StartAll()
	a := r.Agents["laptop"]
	r.StopHub()
	a.Spool.Add(wire.TypeReply, "written while the hub was away")
	time.Sleep(time.Second)
	r.StartHub()
	r.WaitSent("written while the hub was away")
	r.Eventually("the spool file is removed once confirmed", func() bool { return a.Spool.Len() == 0 })
}

func TestEmulatorOfflineAndBack(t *testing.T) {
	r, _ := emuRig(t)
	r.StartAll()
	r.StopAgent("laptop")
	r.WaitSent("went offline")
	r.Say("sent while away")
	r.WaitSent("is offline")
	r.StartAgent("laptop")
	r.Eventually("back", func() bool { return r.Hub.Online("laptop") })
	r.Say("after return")
	r.Eventually("typed", func() bool { return len(r.Agents["laptop"].Exec.Texts()) == 1 })
}

func TestEmulatorAlarmEndToEnd(t *testing.T) {
	r, _ := emuRig(t)
	r.StartAll()
	a := r.Agents["laptop"]
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	time.Sleep(300 * time.Millisecond)
	a.Sensor.Touch()
	r.Eventually("the alert", func() bool { return r.Sent("ALARM") })
	r.Say("/alarm off")
	r.WaitSent("Alarm disarmed")
}

func TestEmulatorTwoAgents(t *testing.T) {
	r, _ := emuRig(t, "laptop", "nas")
	r.StartAll()
	r.Say("/agent nas")
	r.Say("to the nas")
	r.Eventually("nas typed", func() bool { return len(r.Agents["nas"].Exec.Texts()) == 1 })
	r.Say("@laptop to the laptop")
	r.Eventually("laptop typed", func() bool { return len(r.Agents["laptop"].Exec.Texts()) == 1 })
	r.Agents["nas"].Spool.Add(wire.TypeReply, "from the nas")
	r.WaitSent("[nas] from the nas")
	if strings.Contains(strings.Join(r.Agents["laptop"].Exec.Texts(), ""), "nas") {
		t.Fatal("cross delivery")
	}
}

func TestEmulatorLargeReply(t *testing.T) {
	r, _ := emuRig(t)
	r.StartAll()
	r.Agents["laptop"].Spool.Add(wire.TypeReply, strings.Repeat("y", 200_000))
	r.WaitSent("[cut:")
}
