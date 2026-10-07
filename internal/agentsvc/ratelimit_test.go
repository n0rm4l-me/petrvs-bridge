package agentsvc

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The agent's side of the hub's test of the same name: a message refused for the ratchet's budget is acknowledged and lost,
// the log line says so, and it is written now and then, not for every message.
func TestAMessageTheRatchetRefusesForItsBudgetIsLoggedClearlyAndNotForEveryOne(t *testing.T) {
	r := newUpRig(t)
	buf := &safeBuf{}
	r.a.log = slog.New(slog.NewTextHandler(buf, nil))
	hubName, name := r.a.cfg.Hub.Name, r.a.cfg.Identity.Name
	var sk [32]byte
	rand.Read(sk[:])
	hubKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentR, err := ratchet.NewInitiator(sk, hubKey.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	hubR := ratchet.NewResponder(sk, hubKey)
	sid := [8]byte{4, 5, 6}
	r.a.sess = &session{sid: sid, r: agentR}
	agentR.SetDHStepLimit(0, 0, nil) // no budget: the first key the hub offers is one too many
	// the hub has to hear from the agent before it can answer
	first, err := wire.Seal(agentR, name, hubName, sid, &wire.Payload{ID: "a", Type: wire.TypeReady, TS: 1, Exp: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hubR.Decrypt(first[9:], wire.AD(name, hubName, sid)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	send := func(i int) {
		p := &wire.Payload{ID: fmt.Sprint("h", i), Type: wire.TypeHBAck, TS: now.Unix(), Exp: now.Add(time.Minute).Unix()}
		data, err := wire.Seal(hubR, hubName, name, sid, p)
		if err != nil {
			t.Fatal(err)
		}
		acked := false
		m := transport.NewMessage(fmt.Sprint("bus", i), data, wire.Attrs(wire.KindMsg, name), func() { acked = true }, func() {})
		r.a.handle(context.Background(), m)
		if !acked {
			t.Fatalf("message %d was not acknowledged", i)
		}
	}
	for i := 0; i < 5; i++ {
		send(i)
	}
	if got := r.a.Stats().Rejected; got != 5 {
		t.Fatalf("%d rejected", got)
	}
	out := buf.String()
	if n := strings.Count(out, "ratchet refused new keys"); n != 1 {
		t.Fatalf("%d log lines for a burst of five:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "also_since_last_log=0") {
		t.Fatalf("not a warning that says how many were held back:\n%s", out)
	}
	r.now = time.Now().Add(rateLogEvery + time.Second)
	send(5)
	out = buf.String()
	if strings.Count(out, "ratchet refused new keys") != 2 || !strings.Contains(out, "also_since_last_log=4") {
		t.Fatalf("the second line should say how many were held back:\n%s", out)
	}
}
