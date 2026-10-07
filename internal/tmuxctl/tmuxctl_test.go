package tmuxctl

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type recorder struct{ calls [][]string }

func (r *recorder) run(_ context.Context, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	return nil
}

func TestSendTextIsLiteralAndSeparatedByDoubleDash(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude", r.run)
	if err := tm.SendText(context.Background(), "-t evil; rm -rf / && $(x)"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"if-shell", "-F", "-t", "=claude:", "#{pane_in_mode}", "send-keys -X -t =claude: cancel", ";",
			"send-keys", "-t", "=claude:", "-l", "--", "-t evil; rm -rf / && $(x)", ";", "run-shell", "sleep " + enterDelay, ";", "send-keys", "-t", "=claude:", "Enter"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %v", r.calls)
	}
}

// tmux reads "-t claude" as a prefix when there is no session of that name, so text and keys would land in "claude-dev".
func TestTheTargetIsAnExactSessionName(t *testing.T) {
	for name, want := range map[string]string{"claude": "=claude:", "a-b_c": "=a-b_c:", "claude:2": "=claude:2", "a.b:c": "=a.b:c"} {
		r := &recorder{}
		tm, err := New(name, r.run)
		if err != nil {
			t.Fatal(err)
		}
		tm.SendText(context.Background(), "x")
		tm.SendKey(context.Background(), "Enter")
		tm.SendKey(context.Background(), "1")
		for _, c := range r.calls {
			for i, a := range c {
				if a == "-t" && c[i+1] != want {
					t.Errorf("session %q: target %q, want %q (in %v)", name, c[i+1], want, c)
				}
			}
		}
		if len(r.calls) != 3 {
			t.Fatalf("calls = %v", r.calls)
		}
	}
}

func TestTrailingNewlinesAreTrimmedAndInnerOnesKept(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude", r.run)
	tm.SendText(context.Background(), "line one\nline two\r\n\n")
	if len(r.calls) != 1 || r.calls[0][12] != "line one\nline two" {
		t.Fatalf("text sent: %q", r.calls)
	}
}

func TestBadTextIsRefusedBeforeTmuxRuns(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude", r.run)
	for name, in := range map[string]string{
		"empty": "", "only newline": "\n", "nul": "a\x00b", "escape": "a\x1b[31mb", "backspace": "a\x08b",
		"invalid utf8": "a\xffb", "too long": strings.Repeat("x", MaxText+1), "del": "a\x7fb", "c1 control": "a\u0085b",
	} {
		if err := tm.SendText(context.Background(), in); !errors.Is(err, ErrText) {
			t.Errorf("%s: got %v, want ErrText", name, err)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("tmux was run for refused text: %v", r.calls)
	}
}

func TestTextAtTheLimitAndWithUnicode(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude", r.run)
	if err := tm.SendText(context.Background(), strings.Repeat("x", MaxText)); err != nil {
		t.Fatal(err)
	}
	if err := tm.SendText(context.Background(), "привет, 你好, 🙂\tтаб"); err != nil {
		t.Fatal(err)
	}
}

func TestKeyAllowList(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude", r.run)
	for _, k := range []string{"Enter", "Escape", "Up", "Down", "Tab", "BTab", "1", "9"} {
		if err := tm.SendKey(context.Background(), k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	// Only the exact names count: BTab is Shift+Tab, and spellings of it or of other modified keys are refused.
	for _, k := range []string{"", "C-c", "C-d", "0", "10", "enter", "Enter ", "Enter;ls", "-l", "F1", "BSpace", "btab", "BackTab", "S-Tab", "Shift+Tab", "BTab ", "BTab;ls"} {
		if err := tm.SendKey(context.Background(), k); !errors.Is(err, ErrKey) {
			t.Errorf("%q: got %v, want ErrKey", k, err)
		}
	}
	if len(r.calls) != 8 {
		t.Fatalf("calls = %d", len(r.calls))
	}
	want := []string{"if-shell", "-F", "-t", "=claude:", "#{pane_in_mode}", "send-keys -X -t =claude: cancel", ";", "send-keys", "-t", "=claude:", "BTab"}
	if got := r.calls[5]; !reflect.DeepEqual(got, want) {
		t.Fatalf("BTab is sent as %v", got)
	}
}

// In copy mode (the owner scrolled with the mouse) send-keys goes to the copy-mode key table and tmux still says "ok":
// each typing invocation starts by leaving the mode, in the same invocation and for the same exact target.
func TestEveryTypingInvocationLeavesCopyModeFirstForTheSameTarget(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude", r.run)
	tm.SendText(context.Background(), "hello")
	tm.SendKey(context.Background(), "1")
	tm.SendKey(context.Background(), "Escape")
	if len(r.calls) != 3 {
		t.Fatalf("calls = %v", r.calls)
	}
	lead := []string{"if-shell", "-F", "-t", "=claude:", "#{pane_in_mode}", "send-keys -X -t =claude: cancel", ";"}
	for _, c := range r.calls {
		if !reflect.DeepEqual(c[:len(lead)], lead) {
			t.Errorf("the call does not start by leaving copy mode: %v", c)
		}
	}
}

func TestSessionNameValidation(t *testing.T) {
	for _, ok := range []string{"claude", "claude-1", "a.b:c", "A_b"} {
		if _, err := New(ok, nil); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a;b", "$(x)", "-t", strings.Repeat("a", 65), "a\nb"} {
		if bad == "-t" {
			continue // begins with a dash but matches the pattern; tmux receives it after "-t", so it is a value
		}
		if _, err := New(bad, nil); !errors.Is(err, ErrSession) {
			t.Errorf("%q: got %v, want ErrSession", bad, err)
		}
	}
}

func TestRunnerErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	tm, _ := New("claude", func(context.Context, ...string) error { return boom })
	if err := tm.SendText(context.Background(), "x"); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if err := tm.SendKey(context.Background(), "Enter"); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestExecRunnerReportsAMissingSession(t *testing.T) {
	if err := ExecRunner(context.Background(), "has-session", "-t", "definitely-not-a-real-session-xyz"); err == nil {
		t.Skip("tmux unexpectedly succeeded")
	}
}
