package hubsvc

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// A message that the ratchet refuses because the agent offered new keys faster than the budget allows is acknowledged to the
// bus and lost. The log line is the only trace that something was lost, so it says so and says what to do about it; and it
// is not written for every message, since whoever can publish to the bus can make the ratchet refuse as many as he likes.
func TestAMessageTheRatchetRefusesForItsBudgetIsLoggedClearlyAndNotForEveryOne(t *testing.T) {
	cfg := base(t)
	buf := &lockedBuf{}
	cfg.Log = slog.New(slog.NewTextHandler(buf, nil))
	now := time.Unix(1_800_000_000, 0)
	cfg.Now = func() time.Time { return now }
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sk [32]byte
	rand.Read(sk[:])
	hubKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hubSide := ratchet.NewResponder(sk, hubKey)
	hubSide.SetDHStepLimit(0, 0, nil) // no budget: the first key the agent offers is one too many
	agentSide, err := ratchet.NewInitiator(sk, hubKey.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sid := [8]byte{7, 7, 7}
	h.agents["laptop"].cur = &sess{sid: sid, r: hubSide}
	send := func(i int) {
		p := &wire.Payload{ID: fmt.Sprint("m", i), Type: wire.TypeHeartbeat, TS: now.Unix(), Exp: now.Add(time.Minute).Unix()}
		data, err := wire.Seal(agentSide, "laptop", cfg.Identity.Name, sid, p)
		if err != nil {
			t.Fatal(err)
		}
		acked := false
		m := transport.NewMessage(fmt.Sprint("bus", i), data, wire.Attrs(wire.KindMsg, "laptop"), func() { acked = true }, func() {})
		h.onBus(context.Background(), m)
		if !acked {
			t.Fatalf("message %d was not acknowledged: the bus would deliver it again and again", i)
		}
	}
	for i := 0; i < 5; i++ {
		send(i)
	}
	if h.Stats().Rejected != 5 {
		t.Fatalf("%d rejected", h.Stats().Rejected)
	}
	out := buf.String()
	if n := strings.Count(out, "ratchet refused new keys"); n != 1 {
		t.Fatalf("%d log lines for a burst of five:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "agent=laptop") || !strings.Contains(out, "also_since_last_log=0") {
		t.Fatalf("the line does not say who, or is not a warning:\n%s", out)
	}
	now = now.Add(rateLogEvery + time.Second)
	send(5)
	out = buf.String()
	if strings.Count(out, "ratchet refused new keys") != 2 || !strings.Contains(out, "also_since_last_log=4") {
		t.Fatalf("the second line should say how many were held back:\n%s", out)
	}
}

func TestAMessageThatFailsToAuthenticateIsNotTakenForOneRefusedForItsBudget(t *testing.T) {
	cfg := base(t)
	buf := &lockedBuf{}
	cfg.Log = slog.New(slog.NewTextHandler(buf, nil))
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sk [32]byte
	rand.Read(sk[:])
	hubKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	sid := [8]byte{8}
	h.agents["laptop"].cur = &sess{sid: sid, r: ratchet.NewResponder(sk, hubKey)}
	junk := append(append([]byte{wire.Version}, sid[:]...), make([]byte, 2000)...)
	m := transport.NewMessage("x", junk, wire.Attrs(wire.KindMsg, "laptop"), func() {}, func() {})
	h.onBus(context.Background(), m)
	if strings.Contains(buf.String(), "ratchet refused new keys") {
		t.Fatalf("junk was reported as a message refused for the budget:\n%s", buf.String())
	}
}
