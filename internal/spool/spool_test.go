package spool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func open(t *testing.T) *Spool {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAddAndDrainKeepOrder(t *testing.T) {
	s := open(t)
	for _, b := range []string{"one", "two", "three"} {
		if err := s.Add(wire.TypeReply, b); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	n, err := s.Drain(context.Background(), func(_ context.Context, it Item) error { got = append(got, it.Body); return nil })
	if err != nil || n != 3 || strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("%d %v %v", n, got, err)
	}
	if s.Len() != 0 {
		t.Fatal("files were not removed")
	}
}

func TestNothingIsLostWhenPublishFails(t *testing.T) {
	s := open(t)
	s.Add(wire.TypeReply, "a")
	s.Add(wire.TypeNotice, "b")
	s.Add(wire.TypeReply, "c")
	boom := errors.New("bus down")
	calls := 0
	n, err := s.Drain(context.Background(), func(_ context.Context, it Item) error {
		calls++
		if it.Body == "b" {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || n != 1 || calls != 2 || s.Len() != 2 {
		t.Fatalf("n=%d calls=%d left=%d err=%v", n, calls, s.Len(), err)
	}
	var got []string
	s.Drain(context.Background(), func(_ context.Context, it Item) error { got = append(got, it.Body); return nil })
	if strings.Join(got, ",") != "b,c" {
		t.Fatalf("order after retry: %v", got)
	}
}

func TestOnlyReplyAndNoticeCanBeQueued(t *testing.T) {
	s := open(t)
	for _, bad := range []wire.Type{wire.TypeText, wire.TypeKey, wire.TypeLockdown, wire.TypeHeartbeat, "", "anything"} {
		if err := s.Add(bad, "x"); err == nil {
			t.Errorf("type %q was queued", bad)
		}
	}
}

func TestSizeAndCountLimits(t *testing.T) {
	s := open(t)
	if err := s.Add(wire.TypeReply, strings.Repeat("x", wire.MaxBody+1)); err == nil {
		t.Fatal("an oversize body was queued")
	}
	// Add syncs each file to disk, which is slow to do a thousand times over; all but the last are written directly.
	for i := 0; i < MaxFiles-1; i++ {
		name := filepath.Join(s.dir, fmt.Sprintf("%020d-1.json", i))
		if err := os.WriteFile(name, []byte(`{"type":"reply","body":"x"}`), 0o600); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if err := s.Add(wire.TypeReply, "x"); err != nil {
		t.Fatalf("the last place: %v", err)
	}
	if err := s.Add(wire.TypeReply, "x"); !errors.Is(err, ErrFull) {
		t.Fatalf("got %v, want ErrFull", err)
	}
}

func TestBadFilesAreSetAsideNotBlocking(t *testing.T) {
	s := open(t)
	os.WriteFile(filepath.Join(s.dir, "00000000000000000001-1.json"), []byte("not json"), 0o600)
	os.WriteFile(filepath.Join(s.dir, "00000000000000000002-1.json"), []byte(`{"type":"text","body":"inject"}`), 0o600)
	s.Add(wire.TypeReply, "good")
	var got []string
	n, err := s.Drain(context.Background(), func(_ context.Context, it Item) error { got = append(got, it.Body); return nil })
	if err != nil || n != 1 || got[0] != "good" {
		t.Fatalf("%d %v %v", n, got, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "00000000000000000001-1.json.bad")); err != nil {
		t.Fatal("the bad file was not set aside")
	}
}

func TestPermissions(t *testing.T) {
	s := open(t)
	s.Add(wire.TypeReply, "x")
	st, _ := os.Stat(s.dir)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	names, _ := s.files()
	fst, _ := os.Stat(filepath.Join(s.dir, names[0]))
	if fst.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fst.Mode().Perm())
	}
}

func TestDrainStopsOnCancelledContext(t *testing.T) {
	s := open(t)
	s.Add(wire.TypeReply, "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Drain(ctx, func(context.Context, Item) error { t.Fatal("published after cancel"); return nil }); err == nil {
		t.Fatal("no error for a cancelled context")
	}
	if s.Len() != 1 {
		t.Fatal("message lost")
	}
}

func TestOversizedAndCorruptFilesAreSetAside(t *testing.T) {
	s := open(t)
	big := filepath.Join(s.dir, "00000000000000000001-1-aaaaaaaa.json")
	os.WriteFile(big, []byte(`{"type":"reply","body":"`+strings.Repeat("x", 2*wire.MaxBody+10000)+`"}`), 0o600)
	os.WriteFile(filepath.Join(s.dir, "00000000000000000002-1-bbbbbbbb.json"), []byte("{"), 0o600)
	s.Add(wire.TypeReply, "good")
	es, err := s.List()
	if err != nil || len(es) != 1 || es[0].Item.Body != "good" {
		t.Fatalf("%v %v", es, err)
	}
	if _, err := os.Stat(big + ".bad"); err != nil {
		t.Fatal("the oversized file was not set aside")
	}
}

func TestConcurrentAddsNeverShareAFile(t *testing.T) {
	s := open(t)
	done := make(chan error, 100)
	for i := 0; i < 100; i++ {
		go func() { done <- s.Add(wire.TypeReply, "m") }()
	}
	for i := 0; i < 100; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Len(); n != 100 {
		t.Fatalf("%d files for 100 messages: names collided", n)
	}
}
