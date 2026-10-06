package handshake

import (
	"errors"
	"testing"
)

func TestVerifyHelloRecordsNothing(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	for i := 0; i < 3; i++ {
		if err := VerifyHello(w.hub, w.agent.Public(), hello, t0); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	g := NewGuard(10)
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0, g); err != nil {
		t.Fatalf("the hello was spent by looking at it: %v", err)
	}
}

func TestVerifyHelloRefusesForgeriesAndStaleHellos(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	if err := VerifyHello(w.hub, w.agent.Public(), hello, t0.Add(10*MaxSkew)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	hello.Nonce[0] ^= 1
	if err := VerifyHello(w.hub, w.agent.Public(), hello, t0); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered: %v", err)
	}
}

func TestOneSenderCannotFillTheGuardForTheOthers(t *testing.T) {
	a := newWorld(t)
	b := newWorld(t)
	b.agent.Name = "desktop" // a second pinned party
	g := NewGuardPerSender(100, 3)
	for i := 0; i < 3; i++ {
		_, hello, _ := NewInitiator(a.agent, a.hub.Public(), t0)
		if err := g.Check(hello, t0); err != nil {
			t.Fatal(err)
		}
	}
	_, hello, _ := NewInitiator(a.agent, a.hub.Public(), t0)
	if err := g.Check(hello, t0); !errors.Is(err, ErrTooMany) {
		t.Fatalf("a sender went past its own limit: %v", err)
	}
	_, other, _ := NewInitiator(b.agent, b.hub.Public(), t0)
	other.From = "desktop"
	if err := g.Check(other, t0); err != nil {
		t.Fatalf("another sender was locked out: %v", err)
	}
}
