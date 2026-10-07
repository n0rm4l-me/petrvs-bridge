package agentsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
)

// What stays on the screen when Claude Code has exited inline and the shell has drawn its prompt below.
const (
	// the last answer, with a numbered list, above the prompt
	remnantAnswer = "⏺ Here is what to do:\n  1. install the deps\n  2. run the tests\n  3. deploy\n\nuser@host ~/project % "
	// the last frame of the input box, with its mode line, above the prompt
	remnantFrame = "⏺ Done.\n\n────────────────────────────────────────────────────────────────────────────────\n ❯ \n────────────────────────────────────────────────────────────────────────────────\n  ⏵⏵ auto mode on (shift+tab to cycle)\nuser@host ~/project % "
	// a dialog that was on the screen when it exited
	remnantDialog = machPromptScreen + "user@host ~/project % "
	// the same with a prompt that has something on its right
	remnantRight = remnantFrame + "       12:01:33"
)

func TestOnlyTheEndOfTheScreenMakesItClaudeCode(t *testing.T) {
	bash := screentest.Bash(2, "ls -la")
	for name, c := range map[string]struct {
		scr  string
		want bool
	}{
		"idle, auto mode":                  {machIdleScreen, true},
		"idle, hint for shortcuts":         {screentest.Lines("⏺ Done.", "", screentest.Rule, " ❯ ", screentest.Rule, "  ? for shortcuts"), true},
		"busy":                             {machBusyScreen, true},
		"busy, no mode line under the box": {screentest.Lines("✻ Thinking… (12s · esc to interrupt)", "", screentest.Rule, " ❯ ", screentest.Rule), true},
		"permission dialog":                {machPromptScreen, true},
		"permission dialog, drawn":         {bash, true},
		"question with choices":            {screentest.Question(1), true},
		"answer with a list, box below":    {screentest.ListInAnswer(), true},
		"a status line under the mode":     {machIdleScreen + "  ~/project · Opus · 12% of context\n", true},

		"the reviewer's screen: a list, then a prompt": {remnantAnswer, false},
		"the last frame, then a prompt":                {remnantFrame, false},
		"a dialog, then a prompt":                      {remnantDialog, false},
		"the last frame, a prompt with a right side":   {remnantRight, false},
		"a bare shell prompt":                          {machShellScreen, false},
		"a list and no prompt line, no rule":           {"⏺ Here is what to do:\n  1. install the deps\n  2. run the tests\n  3. deploy\n", false},
		"a picker without numbers":                     {screentest.Resume(), false},
		"nothing":                                      {"", false},
		"only blank lines":                             {"\n\n   \n", false},
		"frame then a bash prompt":                     {remnantFrameWith("bash-5.2$ "), false},
		"frame then a root prompt":                     {remnantFrameWith("host# "), false},
		"frame then a fish prompt":                     {remnantFrameWith("user@host ~/project> "), false},
		"frame then a themed prompt":                   {remnantFrameWith("~/project on  main ❯ "), false},
	} {
		if got := looksLikeClaudeCode(c.scr); got != c.want {
			t.Errorf("%s: looksLikeClaudeCode = %v, want %v", name, got, c.want)
		}
	}
}

func remnantFrameWith(prompt string) string {
	return strings.TrimSuffix(remnantFrame, "user@host ~/project % ") + prompt
}

// After Claude Code exits inline its last answer stays above the prompt. The guard took that for Claude Code and let the
// words of the chat into the shell, which runs them as a command.
func TestARemnantOfClaudeCodeAboveAShellPromptDoesNotLetTextIntoTheShell(t *testing.T) {
	for name, scr := range map[string]string{"list": remnantAnswer, "frame": remnantFrame, "dialog": remnantDialog} {
		r := newMachRig(t, Extras{})
		r.pane.set(func(f *machPane) { f.prog, f.screen = "zsh", scr })
		r.a.execute(context.Background(), cmd(1, "rm -rf /tmp/x"))
		if r.ex.got() != "" {
			t.Errorf("%s: typed into the shell: %s", name, r.ex.got())
		}
		if msg := userMsg(t, r.a.checkTextTarget(context.Background())); !strings.Contains(msg, "a shell, not Claude Code") {
			t.Errorf("%s: %q", name, msg)
		}
	}
}

// ... and auto-heal and /continue took it for a running Claude Code, so it was never started again.
func TestARemnantAboveAnIdleShellPromptIsHealedAndContinued(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.prog, f.screen = "zsh", remnantAnswer })
	r.heal(0)
	r.heal(21 * time.Second)
	if !strings.HasPrefix(r.ex.got(), "t:cd ") {
		t.Fatalf("auto-heal did not start Claude Code: %q", r.ex.got())
	}

	r = newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.prog, f.screen = "zsh", remnantAnswer })
	if err := r.a.continueClaude(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.ex.got(), "t:cd ") {
		t.Fatalf("/continue did not start Claude Code: %q, notices %q", r.ex.got(), r.notices(t))
	}
	for _, n := range r.notices(t) {
		if strings.Contains(n, "already running") {
			t.Fatalf("/continue said %q", n)
		}
	}
}

func TestAWrapperThatShowsClaudeCodeIsStillLeftAlone(t *testing.T) {
	for name, scr := range map[string]string{"idle": machIdleScreen, "busy": machBusyScreen, "dialog": machPromptScreen} {
		r := newMachRig(t, machHealExt())
		r.pane.set(func(f *machPane) { f.prog, f.screen = "sh", scr })
		for i := 0; i < 4; i++ {
			r.heal(time.Minute)
		}
		if r.ex.got() != "" {
			t.Errorf("%s: typed into Claude Code: %s", name, r.ex.got())
		}
		if err := r.a.continueClaude(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.ex.got() != "" {
			t.Errorf("%s: /continue typed into Claude Code: %s", name, r.ex.got())
		}
		if n := r.notices(t); len(n) != 1 || !strings.Contains(n[0], "already running") {
			t.Errorf("%s: %q", name, n)
		}
	}
}

const machTypedScreen = "user@host ~/project % rm -rf ~/old"

// Auto-heal typed its command after whatever stood on the input line: rm -rf ~/oldcd '/work' && exec claude --continue.
func TestAHalfTypedLineAtTheShellIsNeverGluedToTheCommand(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	r.pane.set(func(f *machPane) { f.screen = machTypedScreen })
	for i := 0; i < 10; i++ { // the owner has walked away from it
		r.heal(time.Minute)
	}
	if r.ex.got() != "" || len(r.notices(t)) != 0 {
		t.Fatalf("typed %q, notices %q", r.ex.got(), r.notices(t))
	}
	// The line is cleared: the grace period starts from then.
	r.pane.set(func(f *machPane) { f.screen = machShellScreen })
	r.heal(time.Second)
	r.heal(19 * time.Second)
	if r.ex.got() != "" {
		t.Fatalf("typed before a whole grace period of an idle prompt: %q", r.ex.got())
	}
	r.heal(2 * time.Second)
	if !strings.HasPrefix(r.ex.got(), "t:cd ") {
		t.Fatalf("not started once the prompt had been idle: %q", r.ex.got())
	}
}

func TestATextTypedDuringTheGraceStartsItAgain(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	r.heal(0)
	r.heal(10 * time.Second)
	r.pane.set(func(f *machPane) { f.screen = machTypedScreen }) // the owner comes back and begins a command
	r.heal(5 * time.Second)                                      // 15 s since the first look
	r.heal(10 * time.Second)                                     // 25 s: the old clock would have fired
	r.heal(10 * time.Second)
	if r.ex.got() != "" {
		t.Fatalf("typed into a line that the owner was writing: %q", r.ex.got())
	}
}

// The line can change between the look of the tick and the moment of typing (the capture in launch is the last look).
func TestALineThatAppearsBetweenTheDecisionAndTheTypingIsLeftAlone(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	r.heal(0)
	captures := 0
	r.pane.set(func(f *machPane) {
		f.onCap = func() {
			captures++
			if captures == 1 { // the capture of the tick that decides; the next one is launch's
				r.pane.set(func(f *machPane) { f.screen = machTypedScreen })
			}
		}
	})
	r.heal(21 * time.Second)
	if r.ex.got() != "" {
		t.Fatalf("typed after the line had changed: %q", r.ex.got())
	}
	if n := r.notices(t); len(n) != 0 {
		t.Fatalf("%q", n)
	}
	// Nothing was counted as a start.
	r.pane.set(func(f *machPane) { f.onCap = nil; f.screen = machShellScreen })
	r.heal(time.Second)
	r.heal(21 * time.Second)
	if !strings.HasPrefix(r.ex.got(), "t:cd ") {
		t.Fatalf("%q", r.ex.got())
	}
	r.a.mach.mu.Lock()
	starts := len(r.a.mach.heal.starts)
	r.a.mach.mu.Unlock()
	if starts != 1 {
		t.Fatalf("%d starts counted, want 1", starts)
	}
}

func TestAPromptThatIsNotRecognisedIsNotTypedInto(t *testing.T) {
	for name, scr := range map[string]string{"empty screen": "", "unfinished output": "compiling...\n", "login": "Password:"} {
		r := newMachRig(t, machHealExt())
		r.pane.shell("zsh")
		r.pane.set(func(f *machPane) { f.screen = scr })
		for i := 0; i < 5; i++ {
			r.heal(time.Minute)
		}
		if r.ex.got() != "" {
			t.Errorf("%s: typed %q", name, r.ex.got())
		}
	}
}

func TestEveryKindOfIdlePromptIsTypedInto(t *testing.T) {
	for _, scr := range []string{"user@host ~/project % ", "bash-5.2$ ", "root@host:/# ", "user@host ~/project> ", "~/project on  main ❯ ", "~/x\n╰─❯ ", "ls\nfile\n\nuser@host ~ % \n\n"} {
		r := newMachRig(t, machHealExt())
		r.pane.shell("zsh")
		r.pane.set(func(f *machPane) { f.screen = scr })
		r.heal(0)
		r.heal(21 * time.Second)
		if !strings.HasPrefix(r.ex.got(), "t:cd ") {
			t.Errorf("%q: not started", scr)
		}
	}
}

func TestAPaneThatIsScrolledBackIsLeftAlone(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	r.pane.set(func(f *machPane) { f.inMode = true })
	for i := 0; i < 5; i++ {
		r.heal(time.Minute)
	}
	if r.ex.got() != "" {
		t.Fatalf("typed into a pane in copy mode: %q", r.ex.got())
	}
	r.pane.set(func(f *machPane) { f.inMode = false })
	r.heal(time.Second)
	r.heal(19 * time.Second)
	if r.ex.got() != "" {
		t.Fatal("the grace did not begin again when the owner left copy mode")
	}
	r.heal(2 * time.Second)
	if !strings.HasPrefix(r.ex.got(), "t:cd ") {
		t.Fatalf("%q", r.ex.got())
	}
	// A pane whose mode cannot be read is not typed into either.
	r = newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	r.pane.set(func(f *machPane) { f.modeErr = errors.New("tmux hangs") })
	for i := 0; i < 4; i++ {
		r.heal(time.Minute)
	}
	if r.ex.got() != "" {
		t.Fatalf("typed although the mode could not be read: %q", r.ex.got())
	}
}

func TestContinueDoesNotGlueTheCommandToAHalfTypedLineEither(t *testing.T) {
	for name, set := range map[string]func(*machPane){
		"half-typed": func(f *machPane) { f.screen = machTypedScreen },
		"scrolled":   func(f *machPane) { f.inMode = true },
	} {
		r := newMachRig(t, machHealExt())
		r.pane.shell("zsh")
		r.pane.set(set)
		err := r.a.continueClaude(context.Background())
		if msg := userMsg(t, err); !strings.Contains(msg, "shell prompt") || !strings.Contains(msg, "/continue") {
			t.Errorf("%s: %q", name, msg)
		}
		if r.ex.got() != "" {
			t.Errorf("%s: typed %q", name, r.ex.got())
		}
		if n := r.notices(t); len(n) != 0 {
			t.Errorf("%s: %q", name, n)
		}
	}
	// Idle prompt: typed, and said.
	r := newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	if err := r.a.continueClaude(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.ex.got(), "t:cd ") || len(r.notices(t)) != 1 {
		t.Fatalf("%q %q", r.ex.got(), r.notices(t))
	}
}

// healStart used to trust the look of healTick. A /continue that ran in between has cleared the clock, and the pane still
// shows the shell for a moment; a second command was typed.
func TestHealStartChecksAgainUnderTheLockWhatTheTickDecidedOn(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		prep func(r *machRig, h *healClock)
		want bool // a start
	}{
		"as decided":            {func(r *machRig, h *healClock) {}, true},
		"the clock was cleared": {func(r *machRig, h *healClock) { h.since, h.reason, h.line = time.Time{}, "", "" }, false},
		"the grace is not over": {func(r *machRig, h *healClock) { h.since = r.clk.Now().Add(-10 * time.Second) }, false},
		"another state":         {func(r *machRig, h *healClock) { h.reason = goneSession }, false},
		"a start just made":     {func(r *machRig, h *healClock) { h.lastStart = r.clk.Now().Add(-5 * time.Second) }, false},
		"a start long ago":      {func(r *machRig, h *healClock) { h.lastStart = r.clk.Now().Add(-5 * time.Minute) }, true},
	} {
		r := newMachRig(t, machHealExt())
		r.pane.shell("zsh")
		r.a.mach.mu.Lock()
		h := &r.a.mach.heal
		h.since, h.reason, h.line = r.clk.Now().Add(-30*time.Second), goneShell, "user@host ~/project %"
		c.prep(r, h)
		r.a.mach.mu.Unlock()
		r.a.healStart(ctx, r.a.healSettings())
		if got := strings.HasPrefix(r.ex.got(), "t:cd "); got != c.want {
			t.Errorf("%s: started %v, want %v (typed %q)", name, got, c.want, r.ex.got())
		}
	}
}

// The race itself: the clock is cleared (by /continue) after the tick decided and before healStart looked.
func TestAContinueThatRunsBetweenTheTickAndTheStartIsNotFollowedByASecondCommand(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.shell("sh")
	r.heal(0)
	r.pane.set(func(f *machPane) {
		f.onPane = func(call int) {
			if call == 2 { // the look of the tick was the first; this is healStart's, with the start lock held
				r.a.mach.mu.Lock()
				r.a.mach.heal.since, r.a.mach.heal.reason = time.Time{}, ""
				r.a.mach.heal.lastStart = r.clk.Now()
				r.a.mach.mu.Unlock()
			}
		}
	})
	r.heal(21 * time.Second)
	if r.ex.got() != "" {
		t.Fatalf("a second command was typed: %q", r.ex.got())
	}
}

func TestAStartByContinueIsNotFollowedByAutoHealWhileTheGraceLasts(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	if err := r.a.continueClaude(context.Background()); err != nil {
		t.Fatal(err)
	}
	typed := r.ex.got()
	// The fake pane keeps showing the shell, as a real one does for a moment.
	r.heal(0)
	r.heal(19 * time.Second)
	if r.ex.got() != typed {
		t.Fatalf("auto-heal typed again inside the grace period: %q", r.ex.got())
	}
}
