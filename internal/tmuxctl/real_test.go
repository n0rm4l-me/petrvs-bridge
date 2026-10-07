package tmuxctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// These run the real tmux. They are skipped when tmux is missing.
func realSession(t *testing.T) (*Tmux, func() string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := fmt.Sprintf("cbreal%d%d", os.Getpid(), time.Now().UnixNano()%1000)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "60", "cat").CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", name).Run() })
	tm, err := New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	pane := func() string {
		time.Sleep(150 * time.Millisecond)
		out, _ := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", name).Output()
		return string(out)
	}
	return tm, pane
}

func lines(pane string) []string {
	var out []string
	for _, l := range strings.Split(pane, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, " "))
		}
	}
	return out
}

// cat echoes every line it receives, so each typed line appears twice: once as typed, once echoed back.
func TestRealTmuxTypesTheTextExactly(t *testing.T) {
	tm, pane := realSession(t)
	cases := []string{
		"echo hi;",                     // a trailing semicolon used to be eaten
		`find . -exec rm {} \;`,        // a trailing backslash-semicolon used to lose the backslash
		`already\;`,                    // double backslash before the final semicolon
		";",                            // nothing but a semicolon
		"a;b;c",                        // inner semicolons
		"x ;; y;;",                     // several at the end
		"-l --help -t",                 // looks like options
		"$(echo no) `echo no` ${HOME}", // shell syntax that must stay literal
		"quotes \" ' and \\ backslash",
		"unicode: привет 你好 🙂",
		"tab\tseparated",
	}
	for _, c := range cases {
		if err := tm.SendText(context.Background(), c); err != nil {
			t.Fatalf("%q: %v", c, err)
		}
	}
	got := lines(pane())
	for _, c := range cases {
		n := 0
		for _, l := range got {
			if strings.TrimSpace(l) == strings.TrimSpace(strings.ReplaceAll(c, "\t", "        ")) || l == c {
				n++
			}
		}
		if n < 1 {
			t.Errorf("the text %q did not arrive exactly as sent\npane:\n%s", c, strings.Join(got, "\n"))
		}
	}
}

// Enter has to follow the text after a pause, not with it (see enterDelay): Claude Code took a quick burst plus Enter for a
// paste and left the text in its input box. cat shows a typed line at once and prints it a second time only after Enter,
// so the pane tells the two moments apart: once during the pause, twice after SendText returns.
func TestRealTmuxPausesBetweenTextAndEnter(t *testing.T) {
	tm, pane := realSession(t)
	const text = "pause-check-7f3a9c"
	done := make(chan error, 1)
	go func() { done <- tm.SendText(context.Background(), text) }()
	if n := strings.Count(pane(), text); n != 1 { // pane() waits 150 ms, which is inside the pause
		t.Fatalf("during the pause the text appears %d times, want 1 (typed, not yet entered)", n)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(pane(), text); n != 2 {
		t.Fatalf("after SendText the text appears %d times, want 2 (typed and echoed after Enter)", n)
	}
}

func TestRealTmuxDoesNotInterleaveConcurrentSenders(t *testing.T) {
	tm, pane := realSession(t)
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := tm.SendText(context.Background(), fmt.Sprintf("command-number-%02d-end", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got := lines(pane())
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("command-number-%02d-end", i)
		found := false
		for _, l := range got {
			if l == want {
				found = true
			}
			if strings.Count(l, "command-number-") > 1 {
				t.Fatalf("two commands were merged on one line: %q", l)
			}
		}
		if !found {
			t.Errorf("%q missing or damaged\npane:\n%s", want, strings.Join(got, "\n"))
		}
	}
}
