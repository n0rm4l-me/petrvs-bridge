package spool

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func TestMetaAndButtonsSurviveTheSpool(t *testing.T) {
	s := open(t)
	in := Item{Type: wire.TypeNotice, Body: "Claude needs your permission",
		Meta:    map[string]string{"k": "v"},
		Buttons: []wire.Button{{Label: "1. Yes", Key: "1", Prompt: "0a1b2c3d"}, {Label: "3. No", Key: "3", Prompt: "0a1b2c3d"}}}
	if err := s.AddItem(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.List()
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	out := got[0].Item
	if out.Body != in.Body || out.Meta["k"] != "v" || len(out.Buttons) != 2 || out.Buttons[1] != in.Buttons[1] {
		t.Fatalf("came back as %+v", out)
	}
}

func TestPartsAndFilesAreQueuedButTextAndKeysAreNot(t *testing.T) {
	s := open(t)
	for _, typ := range []wire.Type{wire.TypePart, wire.TypeFile, wire.TypeReply, wire.TypeNotice, wire.TypeAlert} {
		if err := s.AddItem(Item{Type: typ, Body: "x"}); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
	for _, typ := range []wire.Type{wire.TypeText, wire.TypeKey, wire.TypeScreen, wire.TypeStart, wire.TypeDone, wire.TypeHeartbeat} {
		if err := s.AddItem(Item{Type: typ, Body: "x"}); err == nil {
			t.Errorf("%s was queued", typ)
		}
	}
}

// A big transfer must not keep the assistant's answers out: it stops short of the limit, replies do not.
func TestATransferLeavesRoomForReplies(t *testing.T) {
	s := open(t)
	// Add syncs each file to disk, so the bulk is written directly; the places near the limit go through AddItem.
	fill := MaxFiles - FileReserve - 2
	for i := 0; i < fill; i++ {
		name := filepath.Join(s.dir, fmt.Sprintf("%020d-1.json", i))
		if err := os.WriteFile(name, []byte(`{"type":"part","body":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.AddItem(Item{Type: wire.TypePart, Body: "x"}); err != nil {
			t.Fatalf("part %d before the limit: %v", i, err)
		}
	}
	if err := s.AddItem(Item{Type: wire.TypePart, Body: "x"}); err != ErrFull {
		t.Fatalf("a part at the limit: %v, want ErrFull", err)
	}
	if err := s.AddItem(Item{Type: wire.TypeFile, Body: "caption"}); err != ErrFull {
		t.Fatalf("the file item that ends a transfer: %v, want ErrFull", err)
	}
	if err := s.Add(wire.TypeReply, "the answer"); err != nil {
		t.Fatalf("a reply was refused while a transfer filled the spool: %v", err)
	}
}

// A pass over the spool that only needs to know which messages there are must not read them: a file of a hundred parts is
// megabytes, and every receipt starts a pass.
func TestNamesDoNotReadTheFilesAndReadFindsOneByName(t *testing.T) {
	s := open(t)
	for i := 0; i < 3; i++ {
		if err := s.Add(wire.TypeReply, fmt.Sprint("body ", i)); err != nil {
			t.Fatal(err)
		}
	}
	names, err := s.Names()
	if err != nil || len(names) != 3 {
		t.Fatalf("%v %v", names, err)
	}
	// a file nobody may read is still listed by name, and only Read notices
	if err := os.Chmod(filepath.Join(s.dir, names[1]), 0); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Names(); err != nil || len(again) != 3 {
		t.Fatalf("names of a spool with an unreadable file: %v %v", again, err)
	}
	it, ok, err := s.Read(names[2])
	if err != nil || !ok || it.Body != "body 2" {
		t.Fatalf("%+v %v %v", it, ok, err)
	}
	if err := s.Remove(names[0]); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Read(names[0]); ok || err != nil {
		t.Fatalf("a file that is gone: ok %v, err %v", ok, err)
	}
}
