package agentsvc

import (
	"context"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
)

// The facts a heartbeat carries for the status card: battery, uptime and disk (from Ext.Facts), whether the tmux session
// is there, what runs in it, the permission mode shown, and how many messages wait in the spool.

const (
	// factsBudget is all the time the heartbeat spends on collecting them. What is not known by then is left out.
	factsBudget = 2 * time.Second
	// modePoll is how often the screen is read for the permission mode; in between the last reading is reported.
	modePoll = 30 * time.Second

	maxMetaEntries = 16  // wire.Payload.Validate
	maxFactBytes   = 100 // a value; the wire allows 256
)

// addFacts adds the facts to the Meta of a heartbeat, within the limits of the wire. It never takes longer than
// factsBudget and never overrides a key that is already there (locked, alarm, host).
func (a *Agent) addFacts(ctx context.Context, meta map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, factsBudget)
	defer cancel()
	put := func(k, v string) {
		v = factClip(oneLineOf(v), maxFactBytes)
		if v == "" || len(meta) >= maxMetaEntries {
			return
		}
		if _, taken := meta[k]; !taken {
			meta[k] = v
		}
	}
	if a.cfg.Screen != nil {
		switch p := a.readPaneState(ctx); p.kind {
		case paneNoSession:
			put("tmux", "missing")
		case paneRunning, paneShell:
			// Claude Code may run under a wrapper that makes the pane look like a shell: the mode tells.
			put("tmux", "ok")
			put("cc", p.prog)
			put("mode", a.paneMode(ctx, p.kind))
		}
	}
	if a.cfg.Spool != nil {
		put("q", strconv.Itoa(a.cfg.Spool.Len()))
	}
	facts := a.machineFacts(ctx)
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(k) <= 32 {
			put(k, facts[k])
		}
	}
}

// machineFacts calls Ext.Facts, waiting for it at most until ctx ends. A call that is slow goes on in the background and
// the heartbeats report the last result until it has come back; a second call is not started meanwhile.
func (a *Agent) machineFacts(ctx context.Context) map[string]string {
	f := a.cfg.Ext.Facts
	if f == nil {
		return nil
	}
	m := &a.mach
	m.factsMu.Lock()
	if m.factsBusy {
		last := m.facts
		m.factsMu.Unlock()
		return last
	}
	m.factsBusy = true
	m.factsMu.Unlock()
	done := make(chan map[string]string, 1)
	go func() {
		var v map[string]string
		defer func() {
			if recover() != nil { // facts are not worth the agent
				a.log.Error("the source of the machine facts panicked")
			}
			m.factsMu.Lock()
			m.facts, m.factsBusy = v, false
			m.factsMu.Unlock()
			done <- v
		}()
		v = f()
	}()
	select {
	case v := <-done:
		return v
	case <-ctx.Done():
		a.log.Debug("the machine facts are slow; the last ones are reported")
		m.factsMu.Lock()
		last := m.facts
		m.factsMu.Unlock()
		return last
	}
}

// paneMode is the permission mode on the screen, read at most every modeEvery, and at once when what runs in the pane
// has changed from one kind to another (Claude Code is back).
func (a *Agent) paneMode(ctx context.Context, kind paneStateKind) string {
	m := &a.mach
	m.factsMu.Lock()
	if !m.modeAt.IsZero() && m.modeKind == kind && time.Since(m.modeAt) < m.modeEvery {
		mode := m.mode
		m.factsMu.Unlock()
		return mode
	}
	m.factsMu.Unlock()
	scr, err := a.cfg.Screen.Capture(ctx)
	if err != nil {
		return ""
	}
	mode := screen.Mode(scr)
	m.factsMu.Lock()
	m.mode, m.modeAt, m.modeKind = mode, time.Now(), kind
	m.factsMu.Unlock()
	return mode
}

// clipBytes cuts s to at most n bytes without splitting a character.
func factClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
