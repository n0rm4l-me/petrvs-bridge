package spool

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// What the hub refuses must not get into the queue: it would be acknowledged and dropped there, and sent again every
// receipt timeout for ever.
func TestAnItemTheHubWouldRefuseIsNotQueued(t *testing.T) {
	s := open(t)
	long := func(n int) string { return strings.Repeat("x", n) }
	for name, it := range map[string]Item{
		"a label of 41 bytes":  {Type: wire.TypeNotice, Body: "b", Buttons: []wire.Button{{Label: long(41), Key: "1"}}},
		"an empty label":       {Type: wire.TypeNotice, Body: "b", Buttons: []wire.Button{{Label: "", Key: "1"}}},
		"a key of 17 bytes":    {Type: wire.TypeNotice, Body: "b", Buttons: []wire.Button{{Label: "ok", Key: long(17)}}},
		"a prompt of 17 bytes": {Type: wire.TypeNotice, Body: "b", Buttons: []wire.Button{{Label: "ok", Key: "1", Prompt: long(17)}}},
		"a value of 257 bytes": {Type: wire.TypePart, Body: "b", Meta: map[string]string{"t": long(257)}},
		"a key of 33 bytes":    {Type: wire.TypePart, Body: "b", Meta: map[string]string{long(33): "v"}},
	} {
		if err := s.AddItem(it); err == nil {
			t.Errorf("%s was queued", name)
		}
	}
	if s.Len() != 0 {
		t.Fatalf("%d files were written for items that were refused", s.Len())
	}
	// the largest that passes
	ok := Item{Type: wire.TypeNotice, Body: "b", Meta: map[string]string{long(32): long(256)}, Buttons: []wire.Button{{Label: long(40), Key: long(16), Prompt: long(16)}}}
	if err := s.AddItem(ok); err != nil {
		t.Fatalf("an item at the limits was refused: %v", err)
	}
}

// A file that something else put in the directory, or that an older program wrote, is set aside when it is read, with a
// line in the log, so that it does not take a place among the messages that are out for ever.
func TestAFileOnDiskThatBreaksTheHubsRulesIsSetAsideAndTakesNoPlace(t *testing.T) {
	s := open(t)
	var logged bytes.Buffer
	s.SetLog(slog.New(slog.NewTextHandler(&logged, nil)))
	bad := Item{Type: wire.TypeNotice, Body: "b", Buttons: []wire.Button{{Label: strings.Repeat("x", 41), Key: "1"}}}
	raw, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	badName := "00000000000000000001-1-deadbeef.json"
	if err := os.WriteFile(filepath.Join(s.dir, badName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(wire.TypeReply, "good"); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List()
	if err != nil || len(entries) != 1 || entries[0].Item.Body != "good" {
		t.Fatalf("%+v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, badName+".bad")); err != nil {
		t.Fatalf("the bad file was not set aside: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("%d messages queued, want only the good one", s.Len())
	}
	if !strings.Contains(logged.String(), "set a queued message aside") || strings.Contains(logged.String(), strings.Repeat("x", 41)) {
		t.Fatalf("log: %q", logged.String())
	}
	// Read by name, as the agent's pass does
	if err := os.WriteFile(filepath.Join(s.dir, badName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Read(badName); ok || err != nil {
		t.Fatalf("Read: ok %v, err %v", ok, err)
	}
}
