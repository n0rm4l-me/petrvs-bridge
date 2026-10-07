package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/promptcmd"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/tmuxctl"
)

// dialogPane shows a screen in a real tmux pane and waits for one key, which it writes to a file. The session has a name
// of its own, and only that one is ever killed.
type dialogPane struct {
	t    *testing.T
	name string
	dir  string
	n    int
}

func newDialogPane(t *testing.T) *dialogPane {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	p := &dialogPane{t: t, name: fmt.Sprintf("zzw3%d", os.Getpid()), dir: t.TempDir()}
	t.Cleanup(p.kill)
	return p
}

func (p *dialogPane) kill() { exec.Command("tmux", "kill-session", "-t", "="+p.name).Run() }

// show replaces the session with one that prints the screen and then reads a single key into the returned file.
func (p *dialogPane) show(screenText string) (keyFile string) {
	p.t.Helper()
	p.kill()
	p.n++
	screenFile := filepath.Join(p.dir, fmt.Sprintf("screen%d.txt", p.n))
	keyFile = filepath.Join(p.dir, fmt.Sprintf("key%d.txt", p.n))
	script := filepath.Join(p.dir, fmt.Sprintf("run%d.sh", p.n))
	body := fmt.Sprintf("cat %q\nstty -icanon -echo min 1\ndd bs=1 count=1 of=%q 2>/dev/null\nsleep 60\n", screenFile, keyFile)
	if err := os.WriteFile(screenFile, []byte(screenText), 0o600); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		p.t.Fatal(err)
	}
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", p.name, "-x", "120", "-y", "40", "sh", script).CombinedOutput(); err != nil {
		p.t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	return keyFile
}

func (p *dialogPane) paneID() string {
	p.t.Helper()
	out, err := exec.Command("tmux", "display-message", "-p", "-t", "="+p.name+":", "#{pane_id}").Output()
	if err != nil {
		p.t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func readKey(file string) string {
	b, _ := os.ReadFile(file)
	return string(b)
}

// With a real tmux pane: the hook reads the real capture, the agent checks and presses through the real tmux. An old
// notice's button does nothing for another command, and still works when only the highlight has moved.
func TestWithARealTmuxPaneAnOldButtonPressesNothingForAnotherCommand(t *testing.T) {
	pane := newDialogPane(t)
	tm, err := tmuxctl.New(pane.name, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := testrig.New(t, testrig.Options{Agent: func(_ string, c *agentsvc.Config) { c.Exec, c.Screen = tm, tm }})
	r.StartAll()
	a := r.Agents["laptop"]

	// the dialog for "ls /tmp" is on the screen when the hook runs, twice (two notices, as after two notifications)
	pane.show(screentest.Bash(1, "ls /tmp"))
	r.Eventually("the dialog to be drawn", func() bool {
		s, err := tm.Capture(context.Background())
		_, ok := screen.ExtractPrompt(s)
		return err == nil && ok
	})
	for i := 0; i < 2; i++ {
		err := promptcmd.Run(context.Background(), promptcmd.Options{
			Stdin: strings.NewReader(`{"message":"Claude needs your permission","notification_type":"permission_prompt"}`),
			Pane:  pane.paneID(), Queue: a.Spool, // the real `tmux capture-pane`
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	r.Eventually("both notices", func() bool { return len(r.SentWithButtons()) == 2 })
	one, two := r.SentWithButtons()[0], r.SentWithButtons()[1]
	if button(one, "1. Yes") == "" || button(one, "✖ Esc") == "" || !strings.Contains(one.Text, "│ ls /tmp") {
		t.Fatalf("%+v", one)
	}

	// Another command, the same dialog: the old Yes must not reach it.
	other := pane.show(screentest.Bash(1, "ls /tmp && rm -rf ~"))
	r.Eventually("the other dialog to be drawn", func() bool {
		s, _ := tm.Capture(context.Background())
		return strings.Contains(s, "rm -rf")
	})
	r.PressOn(one.ID, button(one, "1. Yes"))
	r.WaitSent("no longer on the screen")
	time.Sleep(500 * time.Millisecond)
	if k := readKey(other); k != "" {
		t.Fatalf("the old button pressed %q in the pane of another command", k)
	}

	// The first command again, with the highlight moved on the laptop: the other notice still works, and the key arrives.
	moved := pane.show(screentest.Bash(3, "ls /tmp"))
	r.Eventually("the moved dialog to be drawn", func() bool {
		s, _ := tm.Capture(context.Background())
		return strings.Contains(s, "❯ 3. No")
	})
	r.PressOn(two.ID, button(two, "2. Yes"))
	r.Eventually("the key to reach the pane", func() bool { return readKey(moved) == "2" })
}
