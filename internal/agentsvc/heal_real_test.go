package agentsvc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/tmuxctl"
)

// These run the real tmux, on a session of their own (never one that is there already). They are skipped when tmux is
// missing or cannot start a session here.
func realHealRig(t *testing.T, workdir string) (*machRig, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := fmt.Sprintf("zzw4%d", os.Getpid())
	tm, err := tmuxctl.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	r := newMachRig(t, Extras{Workdir: workdir, Heal: Heal{Enabled: true, Command: "sleep 300", MaxTries: 3, Window: time.Hour}})
	r.a.cfg.Screen, r.a.cfg.Exec = tm, tm
	r.a.cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	r.a.log = r.a.cfg.Log
	return r, name
}

// waitProgram waits until the program in the pane is want, or any shell when want is "shell" (sh is bash on some systems).
func waitProgram(t *testing.T, r *machRig, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if prog, err := r.a.cfg.Screen.PaneCommand(context.Background()); err == nil && (prog == want || want == "shell" && isShell(prog)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	prog, err := r.a.cfg.Screen.PaneCommand(context.Background())
	t.Fatalf("the pane runs %q (%v), want %q", prog, err, want)
}

func paneDir(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("tmux", "display-message", "-p", "-t", "="+name+":", "#{pane_current_path}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestRealTmuxASessionThatIsGoneIsCreatedAgainInTheWorkingDirectory(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, name := realHealRig(t, dir)
	ctx := context.Background()
	if p := r.a.readPaneState(ctx); p.kind != paneNoSession {
		t.Skipf("a session named %s exists or tmux misbehaves: %+v", name, p)
	}
	r.a.healTick(ctx) // the look that starts the grace period (zero here) and the start, in one
	waitProgram(t, r, "sleep")
	if got := paneDir(t, name); got != dir+"\n" {
		t.Fatalf("the pane is in %q, want %q", got, dir)
	}
	if n := r.notices(t); len(n) != 1 || n[0] != "♻️ Claude Code had stopped (session gone); started with sleep 300" {
		t.Fatalf("%q", n)
	}
	// It runs: nothing more happens. Then the session is killed and it is created again.
	r.a.healTick(ctx)
	if n := len(r.notices(t)); n != 1 {
		t.Fatalf("%d notices while it runs", n)
	}
	if out, err := exec.Command("tmux", "kill-session", "-t", "="+name).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	r.a.healTick(ctx)
	waitProgram(t, r, "sleep")
	if n := len(r.notices(t)); n != 2 {
		t.Fatalf("%d notices after the second start", n)
	}
}

func TestRealTmuxAShellInThePaneGetsTheCommandTypedWithAnAwkwardDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "it's a $HOME `dir`")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r, name := realHealRig(t, dir)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "50", "sh").CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	ctx := context.Background()
	waitProgram(t, r, "shell")
	waitForPrompt(t, r) // heal types only into a prompt that is there and idle
	r.a.healTick(ctx)
	waitProgram(t, r, "sleep")
	if got := paneDir(t, name); got != dir+"\n" {
		t.Fatalf("the pane is in %q, want %q", got, dir)
	}
	if n := r.notices(t); len(n) != 1 || n[0] != "♻️ Claude Code had stopped (a shell is in the foreground); started with sleep 300" {
		t.Fatalf("%q", n)
	}
}
