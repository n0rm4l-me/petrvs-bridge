package agentsvc

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/handshake"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The Pub/Sub client calls the handler from several goroutines, and the hub answers a burst of queued hellos with several
// acks. Two acks for two pending hellos, delivered at the same moment, must give one session, and it must be the one the
// hub is told about. (transport.Mem delivers one message at a time, which hides this, so the handler is called directly.)
func TestConcurrentAcksGiveOneSessionAndItIsTheOneReadyIsSentFor(t *testing.T) {
	hubID, _ := identity.Generate("hub")
	agentID, _ := identity.Generate("laptop")
	names := wire.DefaultNames()
	ctx := context.Background()

	for run := 0; run < 250; run++ {
		mem := transport.NewMem()
		mem.Bind(names.ToHub(), names.HubSub())
		a, err := New(Config{Identity: agentID, Hub: hubID.Public(), Transport: mem, Names: names, Exec: noExec{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		var acks []*transport.Message
		for i := 0; i < 2; i++ {
			ini, hello, err := handshake.NewInitiator(agentID, hubID.Public(), now)
			if err != nil {
				t.Fatal(err)
			}
			a.pending[ini.ID()] = pendingHello{ini: ini, at: now}
			ack, _, err := handshake.Respond(hubID, agentID.Public(), hello, now, nil)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := ack.Encode()
			acks = append(acks, &transport.Message{Data: data, Attrs: wire.Attrs(wire.KindAck, "hub")})
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, m := range acks {
			wg.Add(1)
			go func(m *transport.Message) {
				defer wg.Done()
				<-start
				a.handle(ctx, m)
			}(m)
		}
		close(start)
		wg.Wait()

		if n := a.Stats().Handshakes; n != 1 {
			t.Fatalf("run %d: %d sessions were set up from one burst of acks, want 1", run, n)
		}
		ready := mem.Published(names.ToHub())
		if len(ready) != 1 {
			t.Fatalf("run %d: %d ready messages, want 1", run, len(ready))
		}
		sid, err := wire.SessionID(ready[0].Data)
		if err != nil {
			t.Fatal(err)
		}
		if s := a.session(); s == nil || s.sid != sid {
			t.Fatalf("run %d: the agent ended on another session than the one ready was sent for", run)
		}
	}
}
