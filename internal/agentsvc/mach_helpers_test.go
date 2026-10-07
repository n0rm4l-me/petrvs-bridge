package agentsvc

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// machIdleScreen is what Claude Code shows when it waits for the next request.
const machIdleScreen = "⏺ Done.\n\n────────────────────────────────────────────────────────────────────────────────\n ❯ \n────────────────────────────────────────────────────────────────────────────────\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"

const machBusyScreen = "✻ Thinking… (12s · esc to interrupt)\n\n────────────────────────────────────────────────────────────────────────────────\n ❯ \n────────────────────────────────────────────────────────────────────────────────\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"

const machPromptScreen = "⏺ Bash(rm -rf build)\n\n────────────────────────────────────────────────────────────────────────────────\n Bash command\n\n   rm -rf build\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again for rm commands\n   3. No, and tell Claude what to do differently (esc)\n"

// machPane stands in for the tmux session.
type machPane struct {
	mu       sync.Mutex
	prog     string // the program in the foreground
	gone     bool
	paneErr  error
	hang     bool // PaneCommand waits for its context
	screen   string
	capErr   error
	onCap    func() // called inside Capture
	startErr error
	starts   []string
	startFor time.Duration // how long Start takes
	polls    int
	captures int
	inMode   bool           // the pane is scrolled back (copy mode)
	modeErr  error          // PaneInMode fails with it
	onPane   func(call int) // called inside PaneCommand with the number of the call
}

func newMachPane() *machPane { return &machPane{prog: "claude", screen: machIdleScreen} }

func (f *machPane) set(fn func(*machPane)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

const machShellScreen = "user@host ~/project % "

// shell leaves the pane at a prompt of the named shell.
func (f *machPane) shell(name string) {
	f.set(func(f *machPane) { f.gone, f.prog, f.screen = false, name, machShellScreen })
}

// claude puts Claude Code back into the foreground.
func (f *machPane) claude() {
	f.set(func(f *machPane) { f.gone, f.prog, f.screen = false, "claude", machIdleScreen })
}

func (f *machPane) PaneCommand(ctx context.Context) (string, error) {
	f.mu.Lock()
	f.polls++
	gone, prog, err, hang, call, hook := f.gone, f.prog, f.paneErr, f.hang, f.polls, f.onPane
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	if gone {
		return "", screen.ErrNoSession
	}
	return prog, nil
}

func (f *machPane) PaneInMode(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inMode, f.modeErr
}

func (f *machPane) Capture(context.Context) (string, error) {
	f.mu.Lock()
	f.captures++
	gone, scr, err, hook := f.gone, f.screen, f.capErr, f.onCap
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return "", err
	}
	if gone {
		return "", screen.ErrNoSession
	}
	return scr, nil
}

func (f *machPane) Start(_ context.Context, workdir, command string) error {
	f.mu.Lock()
	d, err := f.startFor, f.startErr
	f.mu.Unlock()
	time.Sleep(d)
	if err != nil {
		f.mu.Lock()
		f.starts = append(f.starts, workdir+"|"+command+"|failed")
		f.mu.Unlock()
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, workdir+"|"+command)
	f.gone = false
	f.prog, _, _ = strings.Cut(command, " ")
	return nil
}

func (f *machPane) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...)
}

// machClock is a time the test moves.
type machClock struct {
	mu sync.Mutex
	t  time.Time
}

func newMachClock() *machClock { return &machClock{t: time.Unix(1_800_000_000, 0)} }

func (c *machClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *machClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// machRig is an agent with a fake pane, a clock and a spool.
type machRig struct {
	a    *Agent
	ex   *recExec
	pane *machPane
	clk  *machClock
	sp   *spool.Spool
	dir  string // the working directory
}

func newMachRig(t *testing.T, ext Extras) *machRig {
	t.Helper()
	r := &machRig{pane: newMachPane(), clk: newMachClock(), dir: t.TempDir()}
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.sp = sp
	if ext.Workdir == "" {
		ext.Workdir = r.dir
	}
	a, ex, _ := withSession(t, func(c *Config) {
		c.Screen, c.Spool, c.Now, c.Ext = r.pane, sp, r.clk.Now, ext
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	})
	r.a, r.ex = a, ex
	return r
}

// machHealExt is a typical heal setting.
func machHealExt() Extras {
	return Extras{Heal: Heal{Enabled: true, Command: "claude --continue", Grace: 20 * time.Second, MaxTries: 3, Window: 10 * time.Minute}}
}

// notices lists the bodies of the notices in the spool.
func (r *machRig) notices(t *testing.T) []string {
	t.Helper()
	es, err := r.sp.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Item.Type == wire.TypeNotice {
			out = append(out, e.Item.Body)
		}
	}
	return out
}

func (r *machRig) items(t *testing.T) []spool.Item {
	t.Helper()
	es, err := r.sp.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []spool.Item
	for _, e := range es {
		out = append(out, e.Item)
	}
	return out
}

// tick advances the clock by d and looks once.
func (r *machRig) heal(d time.Duration) {
	r.clk.add(d)
	r.a.healTick(context.Background())
}

func (r *machRig) stuck(d time.Duration) {
	r.clk.add(d)
	r.a.stuckTick(context.Background())
}

func (r *machRig) lock() {
	r.a.mu.Lock()
	r.a.locked = true
	r.a.mu.Unlock()
}

func (r *machRig) unlock() {
	r.a.mu.Lock()
	r.a.locked = false
	r.a.mu.Unlock()
}
