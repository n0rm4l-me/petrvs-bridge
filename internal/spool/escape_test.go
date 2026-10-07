package spool

import (
	"os"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Control bytes are written as six-byte escapes, so a body under MaxBody can grow past what read() accepts and would be set
// aside as .bad after `agent send` had said it worked. Add has to refuse it instead.
func TestABodyThatGrowsPastTheFileLimitWhenEscapedIsRefused(t *testing.T) {
	s := open(t)
	if err := s.Add(wire.TypeReply, strings.Repeat("\x1b", 100<<10)); err == nil {
		t.Error("a 100 KB body of escape bytes was accepted though it cannot be read back")
	}
	if entries, _ := os.ReadDir(s.dir); len(entries) != 0 {
		t.Errorf("%d files left behind", len(entries))
	}
}

// Code and HTML are full of "<", ">" and "&": they are stored as they are, not as six-byte escapes, so a big reply of
// that kind is queued and comes back unchanged.
func TestAngleBracketsAndAmpersandsAreNotExpanded(t *testing.T) {
	for name, body := range map[string]string{
		"angle brackets": strings.Repeat("<", 200<<10),
		"ampersands":     strings.Repeat("&", 200<<10),
		"mixed":          strings.Repeat("a<b>&c", 40<<10),
	} {
		s := open(t)
		if err := s.Add(wire.TypeReply, body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := s.List()
		if err != nil || len(got) != 1 || got[0].Item.Body != body {
			t.Fatalf("%s: read back %d entries, err %v", name, len(got), err)
		}
	}
}

func TestBodiesThatFitAreQueuedAndReadBack(t *testing.T) {
	for name, body := range map[string]string{
		"plain, at the limit":    strings.Repeat("a", wire.MaxBody),
		"a few escapes in a big": strings.Repeat("a", wire.MaxBody-100) + strings.Repeat("<", 100),
		"html and newlines":      strings.Repeat("<p>a & b</p>\n", 1000),
	} {
		s := open(t)
		if err := s.Add(wire.TypeReply, body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := s.List()
		if err != nil || len(got) != 1 || got[0].Item.Body != body {
			t.Fatalf("%s: read back %d entries, err %v", name, len(got), err)
		}
	}
}
