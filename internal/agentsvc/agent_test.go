package agentsvc

import (
	"context"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
)

type noExec struct{}

func (noExec) SendText(context.Context, string) error { return nil }
func (noExec) SendKey(context.Context, string) error  { return nil }

func TestNewValidates(t *testing.T) {
	id, _ := identity.Generate("laptop")
	hub, _ := identity.Generate("hub")
	ok := Config{Identity: id, Hub: hub.Public(), Transport: transport.NewMem(), Exec: noExec{}}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"no identity":  func(c *Config) { c.Identity = nil },
		"no transport": func(c *Config) { c.Transport = nil },
		"no executor":  func(c *Config) { c.Exec = nil },
		"bad hub key":  func(c *Config) { c.Hub.Static = c.Hub.Static[:5] },
	} {
		c := ok
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	id, _ := identity.Generate("laptop")
	hub, _ := identity.Generate("hub")
	a, err := New(Config{Identity: id, Hub: hub.Public(), Transport: transport.NewMem(), Exec: noExec{}})
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.HeartbeatEvery == 0 || a.cfg.SilenceLimit != 3*a.cfg.HeartbeatEvery || a.cfg.Names.Prefix != "pb" || a.cfg.ReceiptTimeout == 0 {
		t.Fatalf("%+v", a.cfg)
	}
	if a.Connected() || a.Locked() {
		t.Fatal("a new agent must start disconnected and unlocked")
	}
}

func TestRememberIsBounded(t *testing.T) {
	id, _ := identity.Generate("laptop")
	hub, _ := identity.Generate("hub")
	a, _ := New(Config{Identity: id, Hub: hub.Public(), Transport: transport.NewMem(), Exec: noExec{}})
	for i := 0; i < 5000; i++ {
		if a.remember(string(rune('a'+i%26)) + string(rune(i))) {
			t.Fatalf("id %d reported as a duplicate", i)
		}
	}
	if len(a.seenIDs) > 1024 || len(a.seenRing) > 1024 {
		t.Fatalf("seen set grew to %d", len(a.seenIDs))
	}
	a.seenIDs, a.seenRing = map[string]struct{}{}, nil
	a.remember("x")
	if !a.remember("x") {
		t.Fatal("a duplicate was not detected")
	}
}
