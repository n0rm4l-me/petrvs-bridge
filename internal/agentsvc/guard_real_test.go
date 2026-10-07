package agentsvc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/tmuxctl"
)

// The real tmux controller is the screen that can say whether the pane is scrolled back; if the method were renamed, heal
// would silently stop asking.
var _ paneModer = (*tmuxctl.Tmux)(nil)

// These run the real tmux, on sessions named zzfixb<pid>... that they create and kill.
func fixbSplitRig(t *testing.T, first, second string, ext Extras) (*machRig, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := fmt.Sprintf("zzfixb%dag%d", os.Getpid(), time.Now().UnixNano()%100000)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "50", first).CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	// the new pane is the active one, which is where send-keys and capture-pane go
	if out, err := exec.Command("tmux", "split-window", "-t", "="+name+":", second).CombinedOutput(); err != nil {
		t.Skipf("cannot split the window here: %v %s", err, out)
	}
	tm, err := tmuxctl.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := newMachRig(t, ext)
	r.a.cfg.Screen, r.a.cfg.Exec = tm, tm
	r.a.cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	r.a.log = r.a.cfg.Log
	return r, name
}

func fixbPane(name string, pane int) string {
	time.Sleep(300 * time.Millisecond)
	out, _ := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", fmt.Sprintf("=%s:.%d", name, pane)).Output()
	return string(out)
}

// waitForPrompt waits until the shell in the active pane has drawn a prompt.
func waitForPrompt(t *testing.T, r *machRig) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if scr, err := r.a.cfg.Screen.Capture(context.Background()); err == nil {
			if ls := nonEmptyLines(scr); len(ls) > 0 && endsWithPromptMark(ls[len(ls)-1]) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the shell drew no prompt")
}

// The guard asked about pane 0 (list-panes names it first) while the text goes to the active pane: with sleep in pane 0 and
// a shell in the active pane, the words of the chat were run by the shell.
func TestRealTmuxTheGuardLooksAtTheActivePaneOfASplitWindow(t *testing.T) {
	r, name := fixbSplitRig(t, "sleep 300", "sh", Extras{})
	waitProgram(t, r, "shell")
	waitForPrompt(t, r)
	r.a.execute(context.Background(), cmd(1, "echo text-the-shell-must-not-run"))
	if got := fixbPane(name, 1); strings.Contains(got, "text-the-shell-must-not-run") {
		t.Fatalf("the text was typed into the shell:\n%s", got)
	}
	if msg := userMsg(t, r.a.checkTextTarget(context.Background())); !strings.Contains(msg, "a shell, not Claude Code") {
		t.Fatalf("%q", msg)
	}
}

// Auto-heal asked about pane 0 as well: a shell there and a running program in the active pane made it type the restart
// command into the program.
func TestRealTmuxAutoHealLooksAtTheActivePaneOfASplitWindow(t *testing.T) {
	dir, err := os.MkdirTemp("", "zzfixb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ext := Extras{Workdir: dir, Heal: Heal{Enabled: true, Command: "sleep 300", MaxTries: 3, Window: time.Hour}}
	r, name := fixbSplitRig(t, "sh", "cat", ext)
	waitProgram(t, r, "cat")
	for i := 0; i < 3; i++ {
		r.heal(time.Minute)
	}
	if got := fixbPane(name, 1); strings.Contains(got, "exec sleep") {
		t.Fatalf("the restart command was typed into the program in the active pane:\n%s", got)
	}
	if n := r.notices(t); len(n) != 0 {
		t.Fatalf("%q", n)
	}
}
