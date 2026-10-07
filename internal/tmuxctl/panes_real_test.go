package tmuxctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These run the real tmux, in sessions named zzfixb<pid>...; only those are touched and they are killed at the end.
func fixbSession(t *testing.T, command string) (*Tmux, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := fmt.Sprintf("zzfixb%d%d", os.Getpid(), time.Now().UnixNano()%100000)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "60", command).CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	tm, err := New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tm, name
}

func tmuxOK(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		t.Fatalf("tmux %s: %v %s", strings.Join(args, " "), err, out)
	}
}

// screenOf reads the pane of the session after a moment, joined lines so that nothing wraps.
func screenOf(name string) string {
	time.Sleep(200 * time.Millisecond)
	out, _ := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", "="+name+":").Output()
	return string(out)
}

func waitCommand(t *testing.T, tm *Tmux, want string) {
	t.Helper()
	var got string
	var err error
	for i := 0; i < 50; i++ {
		if got, err = tm.PaneCommand(context.Background()); err == nil && got == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the program in the foreground is %q (%v), want %q", got, err, want)
}

// send-keys and capture-pane use the active pane; list-panes names pane 0 first. With a split window the guard, auto-heal
// and /continue used to look at pane 0 while the text went to the other one.
func TestRealTmuxPaneCommandIsThatOfTheActivePaneOfASplitWindow(t *testing.T) {
	tm, name := fixbSession(t, "sleep 120")
	tmuxOK(t, "split-window", "-t", "="+name+":", "cat")
	waitCommand(t, tm, "cat") // the new pane is the active one; pane 0 runs sleep
	tmuxOK(t, "select-pane", "-t", "="+name+":.0")
	waitCommand(t, tm, "sleep")
	// and the text really goes where the answer points
	tmuxOK(t, "select-pane", "-t", "="+name+":.1")
	if err := tm.SendText(context.Background(), "typed-into-the-active-pane"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(screenOf(name), "typed-into-the-active-pane"); n != 2 { // typed and echoed by cat
		t.Fatalf("the active pane shows the text %d times, want 2", n)
	}
}

func TestRealTmuxPaneInModeIsThatOfTheActivePane(t *testing.T) {
	tm, name := fixbSession(t, "sleep 120")
	tmuxOK(t, "split-window", "-t", "="+name+":", "cat")
	waitCommand(t, tm, "cat")
	ctx := context.Background()
	if in, err := tm.PaneInMode(ctx); err != nil || in {
		t.Fatalf("a pane not in a mode: %v %v", in, err)
	}
	tmuxOK(t, "copy-mode", "-t", "="+name+":.0") // not the active pane
	if in, err := tm.PaneInMode(ctx); err != nil || in {
		t.Fatalf("pane 0 is in copy mode, the active pane is not: %v %v", in, err)
	}
	tmuxOK(t, "copy-mode", "-t", "="+name+":.1")
	if in, err := tm.PaneInMode(ctx); err != nil || !in {
		t.Fatalf("the active pane is in copy mode: %v %v", in, err)
	}
	tmuxOK(t, "kill-session", "-t", "="+name)
	if _, err := tm.PaneInMode(ctx); err == nil {
		t.Fatal("no error for a session that is gone")
	}
}

func inMode(name string) bool {
	out, _ := exec.Command("tmux", "display-message", "-p", "-t", "="+name+":", "#{pane_in_mode}").Output()
	return strings.TrimSpace(string(out)) == "1"
}

// After the owner scrolls with the mouse the pane is in copy mode; send-keys then goes to the copy-mode key table, tmux
// says "ok", and nothing reaches the program: the agent reported "done" for a text or a "1" that nobody got.
func TestRealTmuxTextAndKeysReachAPaneThatIsInCopyMode(t *testing.T) {
	tm, name := fixbSession(t, "cat")
	ctx := context.Background()
	tmuxOK(t, "copy-mode", "-t", "="+name+":")
	if !inMode(name) {
		t.Skip("cannot put the pane into copy mode here")
	}
	if err := tm.SendText(ctx, "text-sent-in-copy-mode"); err != nil {
		t.Fatal(err)
	}
	got := screenOf(name)
	if n := strings.Count(got, "text-sent-in-copy-mode"); n != 2 { // typed, then echoed by cat after Enter
		t.Fatalf("the text appears %d times, want 2:\n%s", n, got)
	}
	if inMode(name) {
		t.Error("the pane is still in copy mode")
	}

	tmuxOK(t, "copy-mode", "-t", "="+name+":")
	if err := tm.SendKey(ctx, "7"); err != nil {
		t.Fatal(err)
	}
	if err := tm.SendKey(ctx, "Enter"); err != nil {
		t.Fatal(err)
	}
	got = screenOf(name)
	seven := 0
	for _, l := range strings.Split(got, "\n") {
		if strings.TrimSpace(l) == "7" {
			seven++
		}
	}
	if seven != 2 { // typed, then echoed by cat after Enter
		t.Fatalf("the key 7 and Enter arrived %d times as a line, want 2:\n%s", seven, got)
	}
	if inMode(name) {
		t.Error("the pane is still in copy mode")
	}

	// A pane that is not in a mode is typed into as before, and the session that is gone is still an error.
	if err := tm.SendText(ctx, "plain-text"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(screenOf(name), "plain-text"); n != 2 {
		t.Fatalf("plain text appears %d times, want 2", n)
	}
	tmuxOK(t, "kill-session", "-t", "="+name)
	if err := tm.SendText(ctx, "x"); err == nil {
		t.Error("SendText reported success for a session that is gone")
	}
	if err := tm.SendKey(ctx, "1"); err == nil {
		t.Error("SendKey reported success for a session that is gone")
	}
}
