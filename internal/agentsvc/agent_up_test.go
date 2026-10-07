package agentsvc

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// upRig is an agent with a session nobody listens to, a spool, and a clock the test moves.
type upRig struct {
	a     *Agent
	dir   string
	sp    *spool.Spool
	mem   *transport.Mem
	names wire.Names
	lock  string
	now   time.Time
}

func newUpRig(t *testing.T) *upRig {
	t.Helper()
	id, _ := identity.Generate("laptop")
	hub, _ := identity.Generate("hub")
	mem := transport.NewMem()
	names := wire.DefaultNames()
	mem.Bind(names.ToHub(), names.HubSub())
	dir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &upRig{dir: dir, sp: sp, mem: mem, names: names, lock: filepath.Join(t.TempDir(), "locked"), now: time.Now()}
	a, err := New(Config{Identity: id, Hub: hub.Public(), Transport: mem, Names: names, Exec: noExec{}, Spool: sp, LockFile: r.lock,
		ReceiptTimeout: 30 * time.Second, MinTurn: -1, Now: func() time.Time { return r.now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var sk [32]byte
	rand.Read(sk[:])
	peer, _ := ecdh.X25519().GenerateKey(rand.Reader)
	rt, err := ratchet.NewInitiator(sk, peer.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	a.sess = &session{r: rt}
	r.a = a
	return r
}

// writeSpoolFile puts a file into the spool directory as something else than the spool would have written it.
func (r *upRig) writeSpoolFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *upRig) add(t *testing.T, typ wire.Type, body string) {
	t.Helper()
	it := spool.Item{Type: typ, Body: body}
	if typ == wire.TypePart || typ == wire.TypeFile {
		it.Meta = map[string]string{wire.MetaTransfer: "t1"}
	}
	if err := r.sp.AddItem(it); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // file names carry the time: keep the order
}

// sent returns the types of the queued items the agent has sent and is waiting a receipt for, in queue order.
func (r *upRig) sent(t *testing.T) (out []wire.Type) {
	t.Helper()
	entries, err := r.sp.List()
	if err != nil {
		t.Fatal(err)
	}
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for _, e := range entries {
		if _, ok := r.a.awaiting["sp-"+e.Name]; ok {
			out = append(out, e.Item.Type)
		}
	}
	return out
}

// confirm plays the hub's receipt for everything that was sent.
func (r *upRig) confirm(t *testing.T) {
	t.Helper()
	r.a.mu.Lock()
	for id, w := range r.a.awaiting {
		delete(r.a.awaiting, id)
		r.sp.Remove(w.file)
	}
	r.a.mu.Unlock()
}

func types(ts ...wire.Type) string {
	s := ""
	for _, t := range ts {
		s += string(t) + ","
	}
	return s
}

func TestFilesAreHeldWhileLockedAndRepliesStillGo(t *testing.T) {
	r := newUpRig(t)
	ctx := context.Background()
	r.add(t, wire.TypePart, "AAAA")
	r.add(t, wire.TypePart, "BBBB")
	r.add(t, wire.TypeFile, "caption")
	r.add(t, wire.TypeReply, "an answer")
	r.add(t, wire.TypeAlert, "someone touched the machine")
	if err := os.WriteFile(r.lock, []byte("locked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "reply,alert," {
		t.Fatalf("while locked the agent sent %q; only the reply and the alert may go", got)
	}
	if r.sp.Len() != 5 {
		t.Fatalf("the held parts left the spool: %d items", r.sp.Len())
	}

	// Nothing is lost: the lock is lifted on the machine and the transfer goes on, parts first, in their order.
	r.confirm(t)
	if err := os.Remove(r.lock); err != nil {
		t.Fatal(err)
	}
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "part,part," {
		t.Fatalf("after the unlock the agent sent %q", got)
	}
	r.confirm(t)
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "file," {
		t.Fatalf("the closing item did not follow its parts: %q", got)
	}
}

func TestALockThatComesBetweenTwoPartsHoldsTheRest(t *testing.T) {
	r := newUpRig(t)
	ctx := context.Background()
	r.add(t, wire.TypePart, "AAAA")
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "part," {
		t.Fatal(got)
	}
	r.confirm(t)
	r.add(t, wire.TypePart, "BBBB")
	r.add(t, wire.TypeFile, "")
	if err := os.WriteFile(r.lock, []byte("locked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "" {
		t.Fatalf("a locked agent sent %q", got)
	}
}

// The parts come before the closing item in the queue, but the bus does not keep the order and the hub cannot put the
// file together before its last part is in. The closing item therefore waits until nothing sent lately is unconfirmed.
func TestTheClosingItemWaitsForTheReceiptsOfTheParts(t *testing.T) {
	r := newUpRig(t)
	ctx := context.Background()
	r.add(t, wire.TypePart, "AAAA")
	r.add(t, wire.TypePart, "BBBB")
	r.add(t, wire.TypeFile, "caption")
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "part,part," {
		t.Fatalf("the first pass sent %q; the closing item must wait for the receipts of the parts", got)
	}
	r.a.spoolTick(ctx) // the parts are still unconfirmed
	if got := types(r.sent(t)...); got != "part,part," {
		t.Fatalf("the closing item went out before the receipts: %q", got)
	}
	r.confirm(t) // the receipts come
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "file," {
		t.Fatalf("the closing item did not go out once its parts were confirmed: %q", got)
	}
}

// A part that got no receipt is sent again after the receipt timeout, in the same pass that reaches the closing item, and
// that resend keeps the closing item back as well.
func TestAResentPartKeepsTheClosingItemBackToo(t *testing.T) {
	r := newUpRig(t)
	ctx := context.Background()
	r.add(t, wire.TypePart, "AAAA")
	r.add(t, wire.TypeFile, "caption")
	r.a.spoolTick(ctx)
	r.now = r.now.Add(31 * time.Second)
	r.a.spoolTick(ctx)
	if got := types(r.sent(t)...); got != "part," {
		t.Fatalf("%q", got)
	}
	if n := len(r.mem.Published(r.names.ToHub())); n != 2 {
		t.Fatalf("%d messages were published; the part should have been sent twice and the closing item not at all", n)
	}
}

// An entry that no spool file stands behind (its file was removed by hand) must not hold a file back for ever.
func TestAStaleEntryDoesNotHoldTheClosingItemBack(t *testing.T) {
	r := newUpRig(t)
	r.a.mu.Lock()
	r.a.awaiting["sp-gone.json"] = awaitingReceipt{file: "gone.json", sentAt: r.now.Add(-time.Hour)}
	r.a.mu.Unlock()
	r.add(t, wire.TypeFile, "caption")
	r.a.spoolTick(context.Background())
	if got := types(r.sent(t)...); got != "file," {
		t.Fatalf("%q", got)
	}
}
