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

// startSession creates a detached session that runs cat, and kills exactly that one when the test ends.
func startSession(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "60", "cat").CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
}

func paneOf(name string) []string {
	time.Sleep(150 * time.Millisecond)
	out, _ := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", "="+name+":").Output()
	return lines(string(out))
}

// With PB_TMUX_SESSION=claude and that session gone, tmux used to take "-t claude" for a prefix and type into claude-dev
// (Enter, and the "1" that answers a permission prompt, included), and the agent reported success.
func TestRealTmuxDoesNotTypeIntoASessionWhoseNameOnlyStartsWithOurs(t *testing.T) {
	ours := fmt.Sprintf("zzfix%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	other := ours + "-other"
	startSession(t, other)
	tm, err := New(ours, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tm.SendText(ctx, "typed-into-the-wrong-session"); err == nil {
		t.Error("SendText reported success although no session has that name")
	}
	if err := tm.SendKey(ctx, "1"); err == nil {
		t.Error("SendKey reported success although no session has that name")
	}
	if err := tm.SendKey(ctx, "Enter"); err == nil {
		t.Error("SendKey reported success although no session has that name")
	}
	if got := paneOf(other); len(got) != 0 {
		t.Fatalf("something was typed into the other session:\n%s", strings.Join(got, "\n"))
	}

	// The exact name still works, and only it is written to.
	startSession(t, ours)
	if err := tm.SendText(ctx, "typed-into-ours"); err != nil {
		t.Fatal(err)
	}
	if err := tm.SendKey(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paneOf(ours), "\n"); !strings.Contains(got, "typed-into-ours") || !strings.Contains(got, "1") {
		t.Errorf("the exact session did not get the text and the key:\n%s", got)
	}
	if got := paneOf(other); len(got) != 0 {
		t.Fatalf("something was typed into the other session:\n%s", strings.Join(got, "\n"))
	}
}

// exec.CommandContext killed the tmux client when the context ended, and the server dropped the rest of the ";" sequence:
// the text was typed, Enter never came, and the next command was appended to it.
func TestRealTmuxFinishesTheSequenceWhenTheContextEndsDuringThePause(t *testing.T) {
	tm, pane := realSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond) // the pause is 300 ms
	defer cancel()
	if err := tm.SendText(ctx, "first-cancelled-text"); err != nil {
		t.Fatalf("SendText with a context that ended during the pause: %v", err)
	}
	if err := tm.SendText(context.Background(), "second-text"); err != nil {
		t.Fatal(err)
	}
	got := lines(pane())
	for _, want := range []string{"first-cancelled-text", "second-text"} {
		n := 0
		for _, l := range got {
			if l == want {
				n++
			}
		}
		if n != 2 { // typed, then echoed by cat after Enter
			t.Errorf("%q appears alone on %d lines, want 2 (Enter was lost?)\npane:\n%s", want, n, strings.Join(got, "\n"))
		}
	}
}

// BTab is tmux's name for Shift+Tab, which Claude Code uses to cycle its permission mode. On a terminal that is the
// sequence ESC [ Z; `cat -v` shows it as ^[[Z, which proves that the real key, and nothing else, was sent.
func TestRealTmuxBTabIsTheShiftTabSequence(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := fmt.Sprintf("zzfixbtab%d%d", os.Getpid(), time.Now().UnixNano()%1000)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "120", "-y", "20", "cat -v").CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	tm, err := New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tm.SendKey(context.Background(), "BTab"); err != nil {
		t.Fatal(err)
	}
	if err := tm.SendKey(context.Background(), "Enter"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	out, _ := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", "="+name+":").Output()
	if !strings.Contains(string(out), "^[[Z") {
		t.Fatalf("Shift+Tab did not reach the program as ESC [ Z:\n%s", out)
	}
}
