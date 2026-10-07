package agentsvc

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

type recExec struct {
	mu   sync.Mutex
	runs []string
}

func (r *recExec) SendText(_ context.Context, s string) error { r.add("t:" + s); return nil }
func (r *recExec) SendKey(_ context.Context, s string) error  { r.add("k:" + s); return nil }
func (r *recExec) add(s string)                               { r.mu.Lock(); r.runs = append(r.runs, s); r.mu.Unlock() }
func (r *recExec) got() string                                { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.runs, ",") }

// withSession returns an agent that believes it has a session with the hub; nothing it publishes goes anywhere.
func withSession(t *testing.T, mutate func(*Config)) (*Agent, *recExec, *session) {
	t.Helper()
	id, _ := identity.Generate("laptop")
	hub, _ := identity.Generate("hub")
	mem := transport.NewMem()
	names := wire.DefaultNames()
	mem.Bind(names.ToHub(), names.HubSub())
	ex := &recExec{}
	cfg := Config{Identity: id, Hub: hub.Public(), Transport: mem, Names: names, Exec: ex, ReorderWindow: 40 * time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sk [32]byte
	rand.Read(sk[:])
	peerKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	r, err := ratchet.NewInitiator(sk, peerKey.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	s := &session{r: r}
	a.sess = s
	return a, ex, s
}

func cmd(n int, body string) *wire.Payload {
	now := time.Now()
	return &wire.Payload{ID: "id" + strconv.Itoa(n) + body, Type: wire.TypeText, Body: body, TS: now.Unix(), Exp: now.Add(time.Minute).Unix(),
		Meta: map[string]string{wire.MetaSeq: strconv.Itoa(n)}}
}

func TestCommandsRunInTheHubsOrderWhateverTheArrivalOrder(t *testing.T) {
	a, ex, s := withSession(t, nil)
	ctx := context.Background()
	for _, n := range []int{3, 1, 4, 2} {
		a.enqueue(ctx, s, cmd(n, strconv.Itoa(n)))
	}
	a.runQueue(ctx)
	if ex.got() != "t:1,t:2,t:3,t:4" {
		t.Fatalf("ran %s", ex.got())
	}
}

func TestAGapHoldsLaterCommandsForTheWindowAndThenIsReported(t *testing.T) {
	a, ex, s := withSession(t, nil)
	ctx := context.Background()
	a.enqueue(ctx, s, cmd(1, "1"))
	a.enqueue(ctx, s, cmd(3, "3"))
	a.enqueue(ctx, s, cmd(5, "5"))
	a.runQueue(ctx)
	if ex.got() != "t:1" {
		t.Fatalf("a later command ran before the missing one had its chance: %s", ex.got())
	}
	time.Sleep(60 * time.Millisecond)
	a.runQueue(ctx)
	if ex.got() != "t:1,t:3" || a.Stats().Lost != 1 {
		t.Fatalf("ran %s, lost %d: #2 should be given up on, and #4 should get its own wait", ex.got(), a.Stats().Lost)
	}
	time.Sleep(60 * time.Millisecond)
	a.runQueue(ctx)
	if ex.got() != "t:1,t:3,t:5" || a.Stats().Lost != 2 {
		t.Fatalf("ran %s, lost %d", ex.got(), a.Stats().Lost)
	}
	a.enqueue(ctx, s, cmd(2, "2")) // too late now
	a.runQueue(ctx)
	if ex.got() != "t:1,t:3,t:5" || a.Stats().Late != 1 {
		t.Fatalf("a late command ran: %s (late %d)", ex.got(), a.Stats().Late)
	}
}

func TestACommandWithoutANumberIsRefused(t *testing.T) {
	a, ex, s := withSession(t, nil)
	ctx := context.Background()
	for _, meta := range []map[string]string{nil, {wire.MetaSeq: "x"}, {wire.MetaSeq: "0"}, {wire.MetaSeq: "-1"}, {wire.MetaSeq: ""}} {
		p := cmd(1, "x")
		p.Meta = meta
		a.enqueue(ctx, s, p)
	}
	a.runQueue(ctx)
	if ex.got() != "" || a.Stats().Refused != 5 {
		t.Fatalf("ran %q, refused %d", ex.got(), a.Stats().Refused)
	}
}

func TestTheQueueCannotBeMadeToGrowWithoutLimit(t *testing.T) {
	a, _, s := withSession(t, nil)
	ctx := context.Background()
	a.enqueue(ctx, s, cmd(1_000_000, "far"))
	for n := 2; n < 400; n++ {
		a.enqueue(ctx, s, cmd(n, "x"))
	}
	a.mu.Lock()
	size := len(a.cmdQ)
	a.mu.Unlock()
	if size > 256 {
		t.Fatalf("queue holds %d commands", size)
	}
}

func TestACommandFromAReplacedSessionIsDropped(t *testing.T) {
	a, ex, old := withSession(t, nil)
	ctx := context.Background()
	a.sess = &session{r: old.r} // a newer session took over
	a.enqueue(ctx, old, cmd(1, "stale session"))
	a.runQueue(ctx)
	if ex.got() != "" {
		t.Fatalf("ran %s", ex.got())
	}
}

func TestExpiryIsCheckedWhenACommandIsAboutToRunNotWhenItArrived(t *testing.T) {
	now := time.Now()
	clock := &now
	a, ex, s := withSession(t, func(c *Config) { c.Now = func() time.Time { return *clock } })
	ctx := context.Background()
	p := cmd(2, "waits for #1")
	p.Exp = now.Add(5 * time.Second).Unix() // valid when it arrives
	a.enqueue(ctx, s, p)
	a.runQueue(ctx)
	*clock = now.Add(10 * time.Minute) // #1 never comes; by the time #2 is let through it is stale
	time.Sleep(60 * time.Millisecond)
	a.runQueue(ctx)
	if ex.got() != "" || a.Stats().Expired != 1 {
		t.Fatalf("ran %q, expired %d", ex.got(), a.Stats().Expired)
	}
}

func TestLockIsCheckedWhenACommandIsAboutToRunNotWhenItArrived(t *testing.T) {
	a, ex, s := withSession(t, nil)
	ctx := context.Background()
	a.enqueue(ctx, s, cmd(2, "queued before the lock"))
	a.lock(ctx)
	a.enqueue(ctx, s, cmd(1, "queued after the lock"))
	a.runQueue(ctx)
	if ex.got() != "" || a.Stats().Refused != 2 {
		t.Fatalf("ran %q, refused %d", ex.got(), a.Stats().Refused)
	}
}

func TestLockWithoutAFileHoldsInMemory(t *testing.T) {
	a, _, _ := withSession(t, nil)
	a.lock(context.Background())
	if !a.Locked() {
		t.Fatal("not locked")
	}
}

func TestLockFileIsWrittenPrivatelyAndRemovingItUnlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "locked")
	a, _, _ := withSession(t, func(c *Config) { c.LockFile = path })
	a.lock(context.Background())
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	if !a.Locked() {
		t.Fatal("not locked")
	}
	os.Remove(path)
	if a.Locked() {
		t.Fatal("still locked after the file was removed on this machine")
	}
}
