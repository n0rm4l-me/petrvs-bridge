package agentsvc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func TestAMissingSessionIsStartedOnlyAfterTheGracePeriod(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.gone = true })
	r.heal(0)
	r.heal(19 * time.Second)
	if len(r.pane.started()) != 0 {
		t.Fatalf("started inside the grace period: %v", r.pane.started())
	}
	r.heal(2 * time.Second)
	want := r.dir + "|claude --continue"
	if got := r.pane.started(); len(got) != 1 || got[0] != want {
		t.Fatalf("%v, want [%s]", got, want)
	}
	n := r.notices(t)
	if len(n) != 1 || n[0] != "♻️ Claude Code had stopped (session gone); started with claude --continue" {
		t.Fatalf("%q", n)
	}
	if r.ex.got() != "" {
		t.Fatalf("something was typed: %s", r.ex.got())
	}
	// It runs again now: no second start however long we look.
	for i := 0; i < 10; i++ {
		r.heal(time.Minute)
	}
	if len(r.pane.started()) != 1 {
		t.Fatalf("%v", r.pane.started())
	}
}

func TestAShellInThePaneGetsTheCommandTypedWithTheDirectoryQuoted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ext := machHealExt()
	ext.Workdir = dir
	r := newMachRig(t, ext)
	r.pane.shell("zsh")
	r.heal(0)
	r.heal(21 * time.Second)
	want := "t:cd '" + strings.ReplaceAll(dir, "'", `'\''`) + "' && exec claude --continue"
	if r.ex.got() != want {
		t.Fatalf("typed %q, want %q", r.ex.got(), want)
	}
	if len(r.pane.started()) != 0 {
		t.Fatal("a session was created although the pane was there")
	}
	if n := r.notices(t); len(n) != 1 || n[0] != "♻️ Claude Code had stopped (a shell is in the foreground); started with claude --continue" {
		t.Fatalf("%q", n)
	}
}

func TestEveryShellNameCountsAndLoginShellsAndPathsToo(t *testing.T) {
	for _, prog := range []string{"zsh", "bash", "sh", "fish", "dash", "ksh", "tcsh", "login", "-zsh", "-bash", "/bin/zsh", "ZSH"} {
		r := newMachRig(t, machHealExt())
		r.pane.shell(prog)
		r.heal(0)
		r.heal(21 * time.Second)
		if !strings.HasPrefix(r.ex.got(), "t:cd ") {
			t.Errorf("%q was not taken for a shell", prog)
		}
	}
}

func TestClaudeCodeUnderAnyOtherNameIsLeftAlone(t *testing.T) {
	// How it shows depends on the installation: claude, node, or its version number.
	for _, prog := range []string{"claude", "node", "2.1.92", "python3", "vim", "claude-code", "bun"} {
		r := newMachRig(t, machHealExt())
		r.pane.set(func(f *machPane) { f.prog = prog })
		for i := 0; i < 5; i++ {
			r.heal(time.Hour)
		}
		if len(r.pane.started()) != 0 || r.ex.got() != "" || len(r.notices(t)) != 0 {
			t.Errorf("%q: it was started again: %v %q %q", prog, r.pane.started(), r.ex.got(), r.notices(t))
		}
	}
}

func TestRestartingByHandDuringTheGraceResetsIt(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.shell("zsh")
	r.heal(0)
	r.heal(15 * time.Second)
	r.pane.claude() // the owner types claude
	r.heal(5 * time.Second)
	r.pane.shell("zsh")     // and quits it again
	r.heal(5 * time.Second) // 25 s after the first look, 0 s into the new absence
	r.heal(19 * time.Second)
	if r.ex.got() != "" {
		t.Fatalf("acted on a clock that should have been cleared: %s", r.ex.got())
	}
	r.heal(2 * time.Second)
	if !strings.HasPrefix(r.ex.got(), "t:cd ") {
		t.Fatalf("not started after a whole grace period: %q", r.ex.got())
	}
}

func TestAPaneThatChangesFromGoneToAShellStartsTheGraceAgain(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.gone = true })
	r.heal(0)
	r.heal(15 * time.Second)
	// The owner creates the session by hand: it shows a shell for a while.
	r.pane.shell("zsh")
	r.heal(10 * time.Second) // 25 s since the first look
	if len(r.pane.started()) != 0 || r.ex.got() != "" {
		t.Fatal("acted while the owner was setting the session up")
	}
	r.heal(19 * time.Second)
	if r.ex.got() != "" {
		t.Fatal("acted before a grace period of the new state")
	}
	r.heal(2 * time.Second)
	if r.ex.got() == "" {
		t.Fatal("never acted")
	}
}

func TestALockedMachineIsNotHealedAndTheGraceStartsAfterTheUnlock(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.gone = true })
	r.lock()
	for i := 0; i < 10; i++ {
		r.heal(time.Minute)
	}
	if len(r.pane.started()) != 0 || len(r.notices(t)) != 0 {
		t.Fatalf("a locked machine was healed: %v", r.pane.started())
	}
	r.unlock()
	r.heal(0)
	if len(r.pane.started()) != 0 {
		t.Fatal("started at once after the unlock, with no grace period")
	}
	r.heal(21 * time.Second)
	if len(r.pane.started()) != 1 {
		t.Fatal("not started after the unlock and the grace period")
	}
}

func TestALockThatArrivesJustBeforeTheStartStopsIt(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.gone = true })
	r.heal(0)
	r.clk.add(30 * time.Second)
	// healTick decides, then the lock comes; healStart looks again.
	r.lock()
	r.a.healStart(context.Background(), r.a.healSettings())
	if len(r.pane.started()) != 0 {
		t.Fatal("started although the machine was locked")
	}
}

func TestARestartLoopIsStoppedAfterTheTriesAndSaidOnceAndStaysQuietHoursLater(t *testing.T) {
	ext := machHealExt()
	ext.Heal.MaxTries = 2
	r := newMachRig(t, ext)
	die := func() { r.pane.set(func(f *machPane) { f.gone = true }) } // it dies at once, every time
	die()
	for i := 0; i < 2; i++ {
		r.heal(0)
		r.heal(21 * time.Second)
		die()
	}
	if len(r.pane.started()) != 2 {
		t.Fatalf("%v", r.pane.started())
	}
	for i := 0; i < 100; i++ { // half an hour of looking, three times the window
		r.heal(21 * time.Second)
	}
	if len(r.pane.started()) != 2 {
		t.Fatalf("started beyond the tries: %v", r.pane.started())
	}
	gave := 0
	for _, n := range r.notices(t) {
		if n == "⚠️ Claude Code is down and could not be kept running (2 starts in 10 min); use /continue when you want to try again" {
			gave++
		}
	}
	if gave != 1 {
		t.Fatalf("the give-up notice was queued %d times: %q", gave, r.notices(t))
	}
	if len(r.notices(t)) != 3 { // two starts and the give-up
		t.Fatalf("%q", r.notices(t))
	}
}

func TestAfterGivingUpItSpeaksAgainOnlyOnceClaudeCodeHasRunAndStoppedAgain(t *testing.T) {
	ext := machHealExt()
	ext.Heal.MaxTries = 2
	r := newMachRig(t, ext)
	die := func() { r.pane.set(func(f *machPane) { f.gone = true }) }
	die()
	for i := 0; i < 3; i++ {
		r.heal(0)
		r.heal(21 * time.Second)
		die()
	}
	if len(r.pane.started()) != 2 || len(r.notices(t)) != 3 {
		t.Fatalf("%v %q", r.pane.started(), r.notices(t))
	}
	// The owner starts it by hand. It dies again inside the window, which still holds two starts: one more notice, no start.
	r.pane.claude()
	r.heal(time.Second)
	die()
	r.heal(0)
	r.heal(21 * time.Second)
	if len(r.pane.started()) != 2 || len(r.notices(t)) != 4 {
		t.Fatalf("%v %q", r.pane.started(), r.notices(t))
	}
	// When the starts have aged out of the window, it is tried again.
	r.pane.claude()
	r.heal(time.Second)
	r.clk.add(11 * time.Minute)
	die()
	r.heal(0)
	r.heal(21 * time.Second)
	if len(r.pane.started()) != 3 {
		t.Fatalf("not started after the window had passed: %v", r.pane.started())
	}
}

func TestAStartThatFailsStillCountsAsATry(t *testing.T) {
	ext := machHealExt()
	ext.Heal.MaxTries = 2
	r := newMachRig(t, ext)
	r.pane.set(func(f *machPane) { f.gone, f.startErr = true, errors.New("tmux: duplicate session") })
	for i := 0; i < 8; i++ {
		r.heal(21 * time.Second)
	}
	if n := len(r.pane.started()); n != 2 {
		t.Fatalf("%d starts were tried with a limit of 2", n)
	}
	n := r.notices(t)
	if len(n) != 1 || !strings.HasPrefix(n[0], "⚠️ Claude Code is down and could not be kept running (2 starts in 10 min)") {
		t.Fatalf("%q", n)
	}
}

func TestHealingWaitsForTmuxToAnswerAndActsOnNothingItCannotSee(t *testing.T) {
	old := paneTimeout
	paneTimeout = 50 * time.Millisecond
	t.Cleanup(func() { paneTimeout = old })
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.hang = true })
	begin := time.Now()
	for i := 0; i < 4; i++ {
		r.heal(time.Hour)
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("a tmux that hangs held the look for %v", took)
	}
	r.pane.set(func(f *machPane) { f.hang, f.paneErr = false, errors.New("tmux: unexpected failure") })
	r.heal(time.Hour)
	r.heal(time.Hour)
	if len(r.pane.started()) != 0 || r.ex.got() != "" || len(r.notices(t)) != 0 {
		t.Fatal("acted on a tmux that gave no answer")
	}
	// The clock did not start either: the first clear sight of a missing session starts the grace period.
	r.pane.set(func(f *machPane) { f.paneErr, f.gone = nil, true })
	r.heal(0)
	if len(r.pane.started()) != 0 {
		t.Fatal("started with no grace period after an unreadable stretch")
	}
}

func TestAnEmptyAnswerIsNotProofOfAShell(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.prog = "  " })
	for i := 0; i < 4; i++ {
		r.heal(time.Hour)
	}
	if r.ex.got() != "" || len(r.pane.started()) != 0 {
		t.Fatal("acted on an empty program name")
	}
}

func TestAShellPaneThatShowsClaudeCodeIsNotTypedInto(t *testing.T) {
	// A wrapper script makes the pane's program a shell while Claude Code runs under it. Typing a command into Claude
	// Code's input box would send it to the assistant.
	for name, scr := range map[string]string{"idle": machIdleScreen, "busy": machBusyScreen, "prompt": machPromptScreen} {
		r := newMachRig(t, machHealExt())
		r.pane.set(func(f *machPane) { f.prog, f.screen = "bash", scr })
		for i := 0; i < 4; i++ {
			r.heal(time.Minute)
		}
		if r.ex.got() != "" {
			t.Errorf("%s: typed into Claude Code: %q", name, r.ex.got())
		}
		if len(r.notices(t)) != 0 {
			t.Errorf("%s: %q", name, r.notices(t))
		}
		// Nothing was started, so no try was used up: the same pane turning into a real shell is healed.
		r.pane.set(func(f *machPane) { f.screen = "user@host ~ % " })
		r.heal(time.Minute)
		r.heal(time.Minute)
		if !strings.HasPrefix(r.ex.got(), "t:cd ") {
			t.Errorf("%s: a plain shell was not healed afterwards: %q", name, r.ex.got())
		}
	}
}

func TestAWorkingDirectoryThatIsGoneIsATryThatFailsAndSaysNothingUntilGivingUp(t *testing.T) {
	ext := machHealExt()
	ext.Heal.MaxTries = 2
	ext.Workdir = filepath.Join(t.TempDir(), "deleted")
	r := newMachRig(t, ext)
	r.pane.set(func(f *machPane) { f.gone = true })
	for i := 0; i < 6; i++ {
		r.heal(21 * time.Second)
	}
	if len(r.pane.started()) != 0 || r.ex.got() != "" {
		t.Fatalf("started in a directory that does not exist: %v", r.pane.started())
	}
	if n := r.notices(t); len(n) != 1 || !strings.Contains(n[0], "could not be kept running (2 starts") {
		t.Fatalf("%q", n)
	}
}

func TestTheCommandInTheNoticeHasItsSecretsHidden(t *testing.T) {
	ext := machHealExt()
	ext.Heal.Command = "env API_KEY=sk-abc123 claude --continue"
	r := newMachRig(t, ext)
	r.pane.set(func(f *machPane) { f.gone = true })
	r.heal(0)
	r.heal(21 * time.Second)
	n := r.notices(t)
	if len(n) != 1 || strings.Contains(n[0], "sk-abc123") || !strings.Contains(n[0], "<hidden>") {
		t.Fatalf("%q", n)
	}
	if got := r.pane.started(); len(got) != 1 || !strings.Contains(got[0], "sk-abc123") {
		t.Fatalf("the command itself must be run as it is: %v", got)
	}
}

func TestDefaultsFillWhatTheSettingsLeaveOut(t *testing.T) {
	r := newMachRig(t, Extras{Heal: Heal{Enabled: true}})
	s := r.a.healSettings()
	if s.command != "claude --continue" || s.tries != 3 || s.window != 10*time.Minute || s.grace != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestThePollIsNeverLongerThanAQuarterOfTheGrace(t *testing.T) {
	for _, c := range []struct{ max, span, want time.Duration }{
		{5 * time.Second, 20 * time.Second, 5 * time.Second},
		{5 * time.Second, 4 * time.Second, time.Second},
		{5 * time.Second, 0, 5 * time.Second},
		{5 * time.Second, time.Millisecond, minPoll},
		{30 * time.Second, 10 * time.Minute, 30 * time.Second},
		{30 * time.Second, time.Minute, 15 * time.Second},
	} {
		if got := pollEvery(c.max, c.span); got != c.want {
			t.Errorf("pollEvery(%v, %v) = %v, want %v", c.max, c.span, got, c.want)
		}
	}
}

func TestExtrasStartOnlyWhatIsConfigured(t *testing.T) {
	// Heal on, but no screen or no working directory: it stays off and the agent still runs.
	for name, mutate := range map[string]func(*machRig){
		"no screen":  func(r *machRig) { r.a.cfg.Screen = nil },
		"no workdir": func(r *machRig) { r.a.cfg.Ext.Workdir = "" },
	} {
		r := newMachRig(t, machHealExt())
		mutate(r)
		r.pane.set(func(f *machPane) { f.gone = true })
		ctx, cancel := context.WithCancel(context.Background())
		r.a.mach.healEvery = 5 * time.Millisecond
		r.a.startExtras(ctx)
		time.Sleep(60 * time.Millisecond)
		cancel()
		r.a.mach.wg.Wait()
		if r.pane.polls != 0 {
			t.Errorf("%s: it looked at the pane", name)
		}
	}
}

func TestTheLoopHealsInTheBackground(t *testing.T) {
	ext := machHealExt()
	ext.Heal.Grace = 40 * time.Millisecond // real time: no clock function is set in this test
	r := newMachRig(t, ext)
	r.a.cfg.Now = time.Now
	r.pane.set(func(f *machPane) { f.gone = true })
	r.a.mach.healEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	r.a.startExtras(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for len(r.pane.started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(r.pane.started()) != 1 {
		t.Fatalf("%v", r.pane.started())
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	r.a.mach.wg.Wait()
	if len(r.pane.started()) != 1 {
		t.Fatalf("%v", r.pane.started())
	}
}

// ---- /continue ----

func machStartCmd() *wire.Payload { return &wire.Payload{ID: "s1", Type: wire.TypeStart} }

func TestContinueStartsAMissingSessionAndASessionAtAShellWithoutUsingTheTries(t *testing.T) {
	ext := machHealExt()
	ext.Heal.MaxTries = 1
	r := newMachRig(t, ext)
	// One try of auto-heal is spent already.
	r.pane.set(func(f *machPane) { f.gone = true })
	r.heal(0)
	r.heal(21 * time.Second)
	r.pane.set(func(f *machPane) { f.gone = true })
	if err := r.a.executeStart(context.Background(), machStartCmd()); err != nil {
		t.Fatal(err)
	}
	if got := r.pane.started(); len(got) != 2 {
		t.Fatalf("/continue was held to the tries: %v", got)
	}
	n := r.notices(t)
	if last := n[len(n)-1]; last != "▶️ Claude Code was started (session gone) with claude --continue" {
		t.Fatalf("%q", n)
	}
	// At a shell the command is typed.
	r.pane.shell("zsh")
	if err := r.a.executeStart(context.Background(), machStartCmd()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.ex.got(), "t:cd ") || !strings.HasSuffix(r.ex.got(), " && exec claude --continue") {
		t.Fatalf("%q", r.ex.got())
	}
}

func TestContinueOnARunningClaudeCodeOnlySaysSo(t *testing.T) {
	r := newMachRig(t, machHealExt())
	if err := r.a.executeStart(context.Background(), machStartCmd()); err != nil {
		t.Fatal(err)
	}
	if len(r.pane.started()) != 0 || r.ex.got() != "" {
		t.Fatal("something was started although Claude Code runs")
	}
	if n := r.notices(t); len(n) != 1 || n[0] != "Claude Code is already running" {
		t.Fatalf("%q", n)
	}
	// Under a name that is not its own, the notice names the program.
	r.pane.set(func(f *machPane) { f.prog = "node" })
	r.a.executeStart(context.Background(), machStartCmd())
	if n := r.notices(t); len(n) != 2 || n[1] != "Claude Code is already running (in the foreground: node)" {
		t.Fatalf("%q", n)
	}
}

func TestContinueRefusesWithoutAScreenOrAWorkingDirectoryOrWhenTmuxDoesNotAnswer(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.a.cfg.Screen = nil
	err := r.a.executeStart(context.Background(), machStartCmd())
	var ue *userError
	if !errors.As(err, &ue) || !strings.Contains(ue.msg, "cannot see its tmux session") {
		t.Fatalf("%v", err)
	}
	r = newMachRig(t, machHealExt())
	r.a.cfg.Ext.Workdir = ""
	if err := r.a.executeStart(context.Background(), machStartCmd()); !errors.As(err, &ue) || !strings.Contains(ue.msg, "PB_WORKDIR") {
		t.Fatalf("%v", err)
	}
	r = newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.paneErr = errors.New("tmux failed") })
	if err := r.a.executeStart(context.Background(), machStartCmd()); !errors.As(err, &ue) || !strings.Contains(ue.msg, "could not look") {
		t.Fatalf("%v", err)
	}
	// A failing tmux is a user error with a fixed text: what tmux printed stays in the log.
	r = newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.gone, f.startErr = true, errors.New("tmux new-session: boom with /secret/path") })
	err = r.a.executeStart(context.Background(), machStartCmd())
	if !errors.As(err, &ue) || strings.Contains(ue.msg, "boom") || !strings.Contains(ue.msg, "could not start") {
		t.Fatalf("%v", err)
	}
}

func TestContinueWorksWhenAutoHealIsOff(t *testing.T) {
	ext := machHealExt()
	ext.Heal.Enabled = false
	r := newMachRig(t, ext)
	r.pane.set(func(f *machPane) { f.gone = true })
	if err := r.a.executeStart(context.Background(), machStartCmd()); err != nil || len(r.pane.started()) != 1 {
		t.Fatalf("%v %v", err, r.pane.started())
	}
}

func TestContinueDoesNotTypeIntoClaudeCodeThatRunsUnderAShellName(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.prog, f.screen = "bash", machIdleScreen })
	if err := r.a.executeStart(context.Background(), machStartCmd()); err != nil {
		t.Fatal(err)
	}
	if r.ex.got() != "" {
		t.Fatalf("typed into Claude Code: %q", r.ex.got())
	}
	if n := r.notices(t); len(n) != 1 || !strings.HasPrefix(n[0], "Claude Code is already running") {
		t.Fatalf("%q", n)
	}
}

func TestContinueAndAutoHealNeverStartTwiceAtTheSameTime(t *testing.T) {
	r := newMachRig(t, machHealExt())
	r.pane.set(func(f *machPane) { f.gone, f.startFor = true, 80*time.Millisecond })
	r.heal(0)
	r.clk.add(30 * time.Second)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.a.healStart(context.Background(), r.a.healSettings()) }()
	go func() { defer wg.Done(); r.a.executeStart(context.Background(), machStartCmd()) }()
	wg.Wait()
	if n := len(r.pane.started()); n != 1 {
		t.Fatalf("%d starts: %v", n, r.pane.started())
	}
}

func TestAStartedClaudeCodeForgetsTheRequestThatWasWaiting(t *testing.T) {
	ext := machHealExt()
	ext.StuckAfter = time.Minute
	r := newMachRig(t, ext)
	r.ask("do the thing")
	if r.a.mach.req == nil {
		t.Fatal("no request was pending before the restart")
	}
	r.pane.set(func(f *machPane) { f.gone = true })
	r.heal(0)
	r.heal(21 * time.Second)
	r.stuck(10 * time.Minute)
	for _, n := range r.notices(t) {
		if strings.Contains(n, "No reply") {
			t.Fatalf("an alert for a request that the restart lost: %q", n)
		}
	}
}
