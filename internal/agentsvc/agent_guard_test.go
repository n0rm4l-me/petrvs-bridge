package agentsvc

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Enter after the typed text picks the highlighted choice of a dialog, usually "1. Yes", so text is not typed while a dialog
// waits: not /sessions from the menu, not /clear, not a sentence of the chat.
func TestNoTextIsTypedWhileAPromptIsOnTheScreen(t *testing.T) {
	r := newMachRig(t, Extras{})
	r.pane.set(func(f *machPane) { f.screen = machPromptScreen })
	ctx := context.Background()
	texts := []string{"List all your sessions\n(use ListAgents) in a short table.", "/sessions", "/clear", "/compact", "/resume", "yes, go on", "1"}
	for i, text := range texts {
		msg := userMsg(t, r.a.checkTextTarget(ctx))
		if !strings.Contains(msg, "a prompt is waiting for an answer, so the text was not typed") || !strings.Contains(msg, "/key N") || !strings.Contains(msg, "then send the text again") {
			t.Fatalf("%q", msg)
		}
		r.a.execute(ctx, cmd(i+1, text))
	}
	if r.ex.got() != "" { // neither text nor a key
		t.Fatalf("typed although a prompt waited: %s", r.ex.got())
	}
	if n := r.a.refused.Load(); n != uint64(len(texts)) {
		t.Fatalf("%d refused, want %d", n, len(texts))
	}
	// The buttons still work: a key answers the prompt.
	key := cmd(10, "1")
	key.Type = wire.TypeKey
	r.a.execute(ctx, key)
	if r.ex.got() != "k:1" {
		t.Fatalf("%q", r.ex.got())
	}
	// Once the dialog is gone the same text goes through.
	r.pane.set(func(f *machPane) { f.screen = machIdleScreen })
	r.a.execute(ctx, cmd(11, "/sessions"))
	if r.ex.got() != "k:1,t:/sessions" {
		t.Fatalf("%q", r.ex.got())
	}
}

func TestTextGoesThroughWhenNoPromptIsOnTheScreenOrTheScreenCannotBeRead(t *testing.T) {
	for name, set := range map[string]func(*machPane){
		"idle":           func(f *machPane) {},
		"busy":           func(f *machPane) { f.screen = machBusyScreen },
		"a list only":    func(f *machPane) { f.screen = "⏺ Steps:\n  1. first\n  2. second\n\n" + machIdleScreen },
		"capture failed": func(f *machPane) { f.capErr = os.ErrDeadlineExceeded },
		"session gone":   func(f *machPane) { f.gone = true },
	} {
		r := newMachRig(t, Extras{})
		r.pane.set(set)
		r.a.execute(context.Background(), cmd(1, "hello"))
		if r.ex.got() != "t:hello" {
			t.Errorf("%s: %q", name, r.ex.got())
		}
	}
	// Without a screen nothing is known and nothing is refused.
	a, ex, _ := withSession(t, nil)
	a.execute(context.Background(), cmd(1, "hello"))
	if ex.got() != "t:hello" {
		t.Fatalf("%q", ex.got())
	}
}

// A shell-named pane that shows a dialog of Claude Code (a wrapper) gets no text either.
func TestNoTextIsTypedIntoADialogThatRunsUnderAShellName(t *testing.T) {
	r := newMachRig(t, Extras{})
	r.pane.set(func(f *machPane) { f.prog, f.screen = "sh", machPromptScreen })
	r.a.execute(context.Background(), cmd(1, "hello"))
	if r.ex.got() != "" {
		t.Fatalf("%q", r.ex.got())
	}
	if msg := userMsg(t, r.a.checkTextTarget(context.Background())); !strings.Contains(msg, "a prompt is waiting") {
		t.Fatalf("%q", msg)
	}
}

// The line that tells the session about a file is typed text as well. The file stays saved; the message says that the
// session was not told.
func TestTheLineAboutAFileIsNotTypedIntoAPromptOrAShellButTheFileIsSaved(t *testing.T) {
	pane := newMachPane()
	r := newDownRig(t, func(c *Config) { c.Screen = pane })
	ctx := context.Background()
	saved := func() int { return len(savedFiles(r.inbox)) }

	pane.set(func(f *machPane) { f.screen = machPromptScreen })
	err := r.a.executeFile(ctx, r.upload("aabbccddeeff", "report.pdf", "please read", payload(1000)))
	msg := userMsg(t, err)
	if !strings.Contains(msg, "saved in the inbox") || !strings.Contains(msg, "the session was not told") || !strings.Contains(msg, "a prompt is waiting") {
		t.Fatalf("%q", msg)
	}
	if len(r.ex.typed()) != 0 || saved() != 1 {
		t.Fatalf("typed %q, %d files saved", r.ex.typed(), saved())
	}

	pane.shell("zsh") // Claude Code has exited; the caption would be run as a command
	err = r.a.executeFile(ctx, r.upload("112233445566", "notes.txt", "x; rm -rf ~", payload(500)))
	msg = userMsg(t, err)
	if !strings.Contains(msg, "saved in the inbox") || !strings.Contains(msg, "shell") || !strings.Contains(msg, "not told") {
		t.Fatalf("%q", msg)
	}
	if len(r.ex.typed()) != 0 || saved() != 2 {
		t.Fatalf("typed %q, %d files saved", r.ex.typed(), saved())
	}

	pane.claude()
	if err := r.a.executeFile(ctx, r.upload("abcdef123456", "ok.txt", "", payload(100))); err != nil {
		t.Fatal(err)
	}
	if len(r.ex.typed()) != 1 || saved() != 3 {
		t.Fatalf("typed %q, %d files saved", r.ex.typed(), saved())
	}
}
