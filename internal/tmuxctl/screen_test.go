package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
)

func TestCaptureAndPaneCommandAskTmuxForTheExactSession(t *testing.T) {
	var calls [][]string
	tm, _ := New("claude", (&recorder{}).run)
	tm.WithOutput(func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "list-panes" {
			return "0 sleep\n1 zsh\n", nil
		}
		return " text on the screen\n", nil
	})
	got, err := tm.Capture(context.Background())
	if err != nil || got != " text on the screen\n" {
		t.Fatalf("%q %v", got, err)
	}
	cmd, err := tm.PaneCommand(context.Background())
	if err != nil || cmd != "zsh" {
		t.Fatalf("%q %v", cmd, err)
	}
	want := [][]string{
		{"capture-pane", "-p", "-t", "=claude:"},
		{"list-panes", "-t", "=claude:", "-F", "#{pane_active} #{pane_current_command}"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
}

// send-keys and capture-pane go to the active pane, so PaneCommand and PaneInMode must speak about that one, whatever its
// place in the list.
func TestPaneCommandAndModeAreThoseOfTheActivePane(t *testing.T) {
	for name, c := range map[string]struct {
		out  string
		want string
	}{
		"active is second":     {"0 sleep\n1 sh\n", "sh"},
		"active is first":      {"1 claude\n0 zsh\n", "claude"},
		"active in the middle": {"0 a\n1 b\n0 c\n", "b"},
		"name with spaces":     {"0 x\n1 my prog\n", "my prog"},
	} {
		tm, _ := New("claude", (&recorder{}).run)
		tm.WithOutput(func(context.Context, ...string) (string, error) { return c.out, nil })
		if got, err := tm.PaneCommand(context.Background()); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, c.want)
		}
	}
	tm, _ := New("claude", (&recorder{}).run)
	var format string
	tm.WithOutput(func(_ context.Context, args ...string) (string, error) {
		format = args[len(args)-1]
		return "0 0\n1 1\n", nil
	})
	in, err := tm.PaneInMode(context.Background())
	if err != nil || !in || format != "#{pane_active} #{pane_in_mode}" {
		t.Fatalf("in mode: %v %v %q", in, err, format)
	}
	tm.WithOutput(func(context.Context, ...string) (string, error) { return "1 0\n0 1\n", nil })
	if in, err := tm.PaneInMode(context.Background()); err != nil || in {
		t.Fatalf("the other pane is in a mode, the active one is not: %v %v", in, err)
	}
}

// A window without an active pane in the answer is no answer: nothing is guessed from the first line.
func TestNoActivePaneInTheAnswerIsAnErrorAndATmuxErrorIsPassedOn(t *testing.T) {
	tm, _ := New("claude", (&recorder{}).run)
	tm.WithOutput(func(context.Context, ...string) (string, error) { return "0 sleep\n0 cat\n", nil })
	if got, err := tm.PaneCommand(context.Background()); err == nil {
		t.Fatalf("got %q", got)
	}
	if _, err := tm.PaneInMode(context.Background()); err == nil {
		t.Fatal("no answer about the mode")
	}
	tm.WithOutput(func(context.Context, ...string) (string, error) { return "", screen.ErrNoSession })
	if _, err := tm.PaneCommand(context.Background()); !errors.Is(err, screen.ErrNoSession) {
		t.Fatalf("%v", err)
	}
	if _, err := tm.PaneInMode(context.Background()); !errors.Is(err, screen.ErrNoSession) {
		t.Fatalf("%v", err)
	}
	tm.WithOutput(func(context.Context, ...string) (string, error) { return "1 maybe\n", nil })
	if _, err := tm.PaneInMode(context.Background()); err == nil {
		t.Fatal("an answer that is neither 0 nor 1 was taken")
	}
}

func TestStartRunsTheCommandInTheWorkingDirectoryThroughCd(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude-dev", r.run)
	if err := tm.Start(context.Background(), "/home/me/it's here", "claude --continue"); err != nil {
		t.Fatal(err)
	}
	want := []string{"new-session", "-d", "-s", "claude-dev", "-c", "/home/me/it's here", `cd '/home/me/it'\''s here' && exec claude --continue`}
	if len(r.calls) != 1 || !reflect.DeepEqual(r.calls[0], want) {
		t.Fatalf("calls = %q", r.calls)
	}
	for name, c := range map[string][2]string{
		"relative dir":    {"work", "claude"},
		"empty command":   {"/w", "  "},
		"newline in cmd":  {"/w", "claude\nrm -rf /"},
		"nul in dir":      {"/w\x00", "claude"},
		"newline in dir":  {"/w\n/x", "claude"},
		"carriage return": {"/w", "claude\r"},
	} {
		if err := tm.Start(context.Background(), c[0], c[1]); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if len(r.calls) != 1 {
		t.Errorf("tmux was run for a refused start: %v", r.calls)
	}
}

func TestATmuxThatCannotFindTheSessionIsErrNoSession(t *testing.T) {
	for _, msg := range []string{"can't find session: claude", "no server running on /private/tmp/tmux-501/default", "error connecting to /tmp/tmux-501/default (No such file or directory)", "can't find pane: =x:"} {
		if !noSession(msg) {
			t.Errorf("%q is not taken for a missing session", msg)
		}
	}
	if noSession("usage: capture-pane") {
		t.Error("a usage error is taken for a missing session")
	}
}

// Against the real tmux, in sessions of our own.
func TestRealTmuxReadsTheScreenTheProgramAndStartsASession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := fmt.Sprintf("zzscr%d%d", os.Getpid(), time.Now().UnixNano()%1000)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	tm, err := New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := tm.Capture(ctx); !errors.Is(err, screen.ErrNoSession) {
		t.Fatalf("capture of a session that does not exist: %v", err)
	}
	if _, err := tm.PaneCommand(ctx); !errors.Is(err, screen.ErrNoSession) {
		t.Fatalf("pane command of a session that does not exist: %v", err)
	}
	dir := t.TempDir()
	if err := tm.Start(ctx, dir, "sh -c 'echo screen-marker-42; pwd; exec sleep 60'"); err != nil {
		t.Skipf("cannot start tmux here: %v", err)
	}
	var got string
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if got, err = tm.Capture(ctx); err == nil && strings.Contains(got, "screen-marker-42") {
			break
		}
	}
	if !strings.Contains(got, "screen-marker-42") {
		t.Fatalf("the screen: %q (%v)", got, err)
	}
	// started in the directory asked for (macOS puts /private in front of temporary directories)
	if !strings.Contains(strings.ReplaceAll(got, "\n", ""), strings.TrimPrefix(dir, "/private")) { // a long path wraps on a narrow pane
		t.Errorf("not started in %s:\n%s", dir, got)
	}
	cmd, err := tm.PaneCommand(ctx)
	if err != nil || cmd != "sleep" {
		t.Errorf("the program in the foreground: %q, %v", cmd, err)
	}
}
