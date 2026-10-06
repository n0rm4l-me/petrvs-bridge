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
