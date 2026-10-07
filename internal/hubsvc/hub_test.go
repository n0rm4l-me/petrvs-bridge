package hubsvc

import (
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
)

func base(t *testing.T) Config {
	t.Helper()
	hub, _ := identity.Generate("hub")
	a, _ := identity.Generate("laptop")
	return Config{Identity: hub, Agents: []identity.Public{a.Public()}, Transport: transport.NewMem(), Telegram: telegram.NewFake(), ChatID: 1, UserID: 1}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(base(t)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"no identity":   func(c *Config) { c.Identity = nil },
		"no transport":  func(c *Config) { c.Transport = nil },
		"no telegram":   func(c *Config) { c.Telegram = nil },
		"no agents":     func(c *Config) { c.Agents = nil },
		"no chat":       func(c *Config) { c.ChatID = 0 },
		"no user":       func(c *Config) { c.UserID = 0 },
		"bad agent key": func(c *Config) { c.Agents[0].Sign = nil },
		"agent twice":   func(c *Config) { c.Agents = append(c.Agents, c.Agents[0]) },
	} {
		c := base(t)
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNoAgentIsActiveOrOnlineAtStartWhenThereAreSeveral(t *testing.T) {
	c := base(t)
	b, _ := identity.Generate("desk")
	c.Agents = append(c.Agents, b.Public())
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if h.activeName() != "" { // the hub asks instead of guessing
		t.Fatalf("active = %q", h.activeName())
	}
	if h.Online("laptop") || h.Online("desk") || h.Online("ghost") {
		t.Fatal("an agent is online before it ever connected")
	}
}

func TestChunkTagging(t *testing.T) {
	h, _ := New(base(t))
	if h.tag("laptop") != "" {
		t.Fatal("a single agent must not be tagged")
	}
	c := base(t)
	b, _ := identity.Generate("desk")
	c.Agents = append(c.Agents, b.Public())
	h2, _ := New(c)
	if h2.tag("desk") != "[desk] " {
		t.Fatal("several agents must be tagged")
	}
}
