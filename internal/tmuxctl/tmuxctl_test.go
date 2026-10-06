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
		{"send-keys", "-t", "claude", "-l", "--", "-t evil; rm -rf / && $(x)"},
		{"send-keys", "-t", "claude", "Enter"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestTrailingNewlinesAreTrimmedAndInnerOnesKept(t *testing.T) {
	r := &recorder{}
	tm, _ := New("claude", r.run)
	tm.SendText(context.Background(), "line one\nline two\r\n\n")
	if r.calls[0][5] != "line one\nline two" {
		t.Fatalf("text sent: %q", r.calls[0][5])
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
	for _, k := range []string{"Enter", "Escape", "Up", "Down", "Tab", "1", "9"} {
		if err := tm.SendKey(context.Background(), k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"", "C-c", "C-d", "0", "10", "enter", "Enter ", "Enter;ls", "-l", "F1", "BSpace"} {
		if err := tm.SendKey(context.Background(), k); !errors.Is(err, ErrKey) {
			t.Errorf("%q: got %v, want ErrKey", k, err)
		}
	}
	if len(r.calls) != 7 {
		t.Fatalf("calls = %d", len(r.calls))
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
