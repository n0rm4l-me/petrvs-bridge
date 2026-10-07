package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The PreToolUse hook gets what Claude has written so far to the chat before it acts (at most once in PB_PROGRESS_EVERY
// seconds), and before a command that may take the machine or the bridge down it also says what is coming and waits until the
// hub has written it to the chat. It never holds the tool back.

const noticeArgs = "send -type notice -"

// preRun runs the hook for a tool call and returns its standard output, its exit code and how long it took.
func (r *rig) preRun(tool, command string, env ...string) (out string, code int, took time.Duration) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": map[string]any{"command": command},
		"transcript_path": r.transcript,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("sh", "../../examples/claude-pretool-hook.sh")
	cmd.Env = append(os.Environ(), "PB_STATE_DIR="+filepath.Join(r.dir, "state"), "AGENT_BIN="+filepath.Join(r.dir, "agent"), "PB_FLUSH_PATTERN=",
		"PB_PROGRESS_EVERY=0") // the tests of the dangerous commands do not want the text of every call; those of the progress ask for it
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err = cmd.Run()
	took = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	return stdout.String(), code, took
}

// calls returns the arguments the stand-in agent was called with, in order.
func (r *rig) calls() []string {
	files, _ := filepath.Glob(filepath.Join(r.dir, "args.*"))
	sort.Strings(files)
	var out []string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out = append(out, strings.TrimSpace(string(b)))
	}
	return out
}

// spoolFile puts a message into the spool as the agent would find it.
func (r *rig) spoolFile(name string) string {
	r.t.Helper()
	dir := filepath.Join(r.dir, "state", "spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func TestACommandThatMayStopTheMachineOrTheBridgeIsAnnounced(t *testing.T) {
	for _, c := range []string{
		`osascript -e 'tell application "System Events" to shut down'; echo "exit: $?"`,
		`osascript -e 'tell app "System Events" to restart'`,
		`osascript -e 'tell app "System Events" to log out'`,
		`sudo shutdown -h now`,
		`sudo -u root /sbin/shutdown -r +1`,
		`cd /tmp && reboot`,
		`echo bye; poweroff`,
		`nohup sh -c 'sleep 1; halt' &` + "\n" + `halt`,
		`pmset sleepnow`,
		`systemctl poweroff`,
		`sudo systemctl -f reboot`,
		`loginctl suspend`,
		`tmux kill-session -t claude-dev`,
		`tmux -L work kill-server`,
		`pkill -f bin/agent`,
		`killall claude`,
		`launchctl bootout gui/501/com.example.bridge`,
	} {
		r := newRig(t)
		r.write(human)
		out, code, _ := r.preRun("Bash", c)
		if code != 0 || out != "" {
			t.Errorf("%q: exit %d, output %q: the hook must neither block nor answer the permission question", c, code, out)
		}
		if got := r.calls(); len(got) != 1 || got[0] != noticeArgs {
			t.Errorf("%q: the agent was called with %q, want one notice", c, got)
		}
	}
}

func TestOrdinaryCommandsAreLeftAlone(t *testing.T) {
	for _, c := range []string{
		`ls -la`,
		`grep -rn shutdown internal/`,
		`echo "do not reboot now"`,
		`git commit -m 'halt the loop on shutdown'`,
		`cat docs/reboot.md`,
		`osascript -e 'set volume output volume 0'`,
		`tmux ls`,
		`tmux send-keys -t claude-dev Enter`,
		`kill 12345`,
		`pkill -f chrome`,
		`launchctl list`,
		`systemctl status nginx`,
		`say -v Tingting hello`,
		// Words in a text that a command writes, not a command: the README of this very hook once raised the alarm.
		"python3 - <<'EOF'\ns = \"the hook knows `shutdown`, `reboot`, `pmset sleepnow`, `pkill agent`\\n`poweroff`, `osascript` shut down, restart or log out\"\nEOF",
		`echo 'use osascript to shut down, restart or log out'`,
		`git commit -m "the hook for pkill agent and tmux kill-session"`,
	} {
		r := newRig(t)
		r.write(human, assistantMsg("tool_use", "some words before"))
		out, code, took := r.preRun("Bash", c)
		if code != 0 || out != "" || len(r.calls()) != 0 {
			t.Errorf("%q: exit %d, output %q, agent calls %q: want nothing", c, code, out, r.calls())
		}
		if took > 3*time.Second {
			t.Errorf("%q: took %v", c, took)
		}
	}
}

func TestOtherToolsAreLeftAlone(t *testing.T) {
	r := newRig(t)
	r.write(human)
	if _, code, _ := r.preRun("Edit", "shutdown -h now"); code != 0 || len(r.calls()) != 0 {
		t.Fatalf("exit %d, agent calls %q", code, r.calls())
	}
}

func TestTheTextSoFarGoesFirstAndIsNotSentAgainAtTheEndOfTheTurn(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "Shutting the laptop down now."), toolUse)
	if _, code, _ := r.preRun("Bash", "sudo shutdown -h now"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := r.sent()
	if len(got) != 2 || got[0] != "Shutting the laptop down now." || !strings.Contains(got[1], "may stop this machine or the bridge") {
		t.Fatalf("%q", got)
	}
	if want := []string{"send -", noticeArgs}; strings.Join(r.calls(), "|") != strings.Join(want, "|") {
		t.Fatalf("the agent was called with %q, want %q: the text first, then the notice", r.calls(), want)
	}
	// The machine came back, or the command failed: the turn ends with its answer, and only the answer is new.
	r.write(toolReply, assistant("Done."))
	r.run()
	if got := r.sent(); len(got) != 3 || got[2] != "Done." {
		t.Fatalf("%q", got)
	}
}

func TestTheNoticeDoesNotShowTheCommand(t *testing.T) {
	r := newRig(t)
	r.write(human)
	r.preRun("Bash", "shutdown -h now # token=hunter2-s3cret")
	got := r.sent()
	if len(got) != 1 || strings.Contains(got[0], "hunter2") || strings.Contains(got[0], "shutdown -h") {
		t.Fatalf("%q: the command may hold secrets and must stay out of the chat", got)
	}
	if !strings.Contains(got[0], "power off, restart, log out or sleep") {
		t.Fatalf("%q: the notice should say what kind of command it is", got)
	}
}

func TestItWaitsUntilTheSpoolIsEmpty(t *testing.T) {
	r := newRig(t)
	r.write(human)
	queued := r.spoolFile("0001.json")
	go func() {
		time.Sleep(700 * time.Millisecond) // the hub confirmed: the agent removes the file
		os.Remove(queued)
	}()
	_, code, took := r.preRun("Bash", "reboot", "PB_FLUSH_WAIT=8")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if took < 600*time.Millisecond || took > 3*time.Second {
		t.Fatalf("took %v, want the 0.7s that the file stayed", took)
	}
}

func TestItGivesUpWaitingAfterPB_FLUSH_WAIT(t *testing.T) {
	r := newRig(t)
	r.write(human)
	r.spoolFile("0001.json") // the agent is not running: no one removes it
	_, code, took := r.preRun("Bash", "reboot", "PB_FLUSH_WAIT=1")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if took < 900*time.Millisecond || took > 3*time.Second {
		t.Fatalf("took %v, want about a second", took)
	}
	if got := r.calls(); len(got) != 1 {
		t.Fatalf("the notice was not queued: %q", got)
	}
}

func TestItDoesNotWaitWhenToldNotTo(t *testing.T) {
	r := newRig(t)
	r.write(human)
	r.spoolFile("0001.json")
	_, _, took := r.preRun("Bash", "reboot", "PB_FLUSH_WAIT=0")
	if took > time.Second {
		t.Fatalf("took %v", took)
	}
}

// A value that is not a number does not turn the wait off or make it endless: the default applies (10s; the file goes after
// a moment here, so the test does not have to sit through it).
func TestAnUnreadableWaitFallsBackToTheDefault(t *testing.T) {
	for _, v := range []string{"soon", "-3", ""} {
		r := newRig(t)
		r.write(human)
		queued := r.spoolFile("0001.json")
		go func() {
			time.Sleep(900 * time.Millisecond)
			os.Remove(queued)
		}()
		_, _, took := r.preRun("Bash", "reboot", "PB_FLUSH_WAIT="+v)
		if took < 800*time.Millisecond || took > 4*time.Second {
			t.Errorf("PB_FLUSH_WAIT=%q: took %v, want it to wait for the file (0.9s)", v, took)
		}
	}
}

func TestAFailingAgentNeverHoldsTheCommandBack(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "words"), toolUse)
	r.spoolFile("0001.json")
	out, code, took := r.preRun("Bash", "reboot", "AGENT_BIN="+filepath.Join(r.dir, "no-such-agent"), "PB_FLUSH_WAIT=8")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if took > 3*time.Second {
		t.Fatalf("took %v: with no way to queue the notice there is nothing to wait for", took)
	}
}

func TestACommandCanBeAddedWithAPattern(t *testing.T) {
	r := newRig(t)
	r.write(human)
	r.preRun("Bash", "./deploy-to-this-box.sh --replace", "PB_FLUSH_PATTERN=deploy-to-this-box")
	got := r.sent()
	if len(got) != 1 || !strings.Contains(got[0], "PB_FLUSH_PATTERN") {
		t.Fatalf("%q", got)
	}
	r2 := newRig(t)
	r2.write(human)
	r2.preRun("Bash", "./something-else.sh", "PB_FLUSH_PATTERN=deploy-to-this-box")
	if len(r2.calls()) != 0 {
		t.Fatalf("%q", r2.calls())
	}
}

// Progress: the words Claude writes between its actions reach the chat while it works, not in one lump when the turn is over.

func TestTheTextWrittenBeforeAnActionGoesToTheChatBeforeTheAction(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "I will read the code first."), toolUse)
	out, code, took := r.preRun("Read", "", "PB_PROGRESS_EVERY=20")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if got := r.sent(); len(got) != 1 || got[0] != "I will read the code first." {
		t.Fatalf("%q", got)
	}
	if got := r.calls(); len(got) != 1 || got[0] != "send -" {
		t.Fatalf("%q: an ordinary call queues the text and no notice", got)
	}
	if took > 3*time.Second {
		t.Fatalf("took %v", took)
	}
	// The end of the turn sends what was written after, and the first part once only.
	r.write(toolReply, assistant("All done."))
	r.run()
	if got := r.sent(); len(got) != 2 || got[1] != "All done." {
		t.Fatalf("%q", got)
	}
}

func TestProgressGoesOutOnceInTheInterval(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "First step."), toolUse)
	r.preRun("Bash", "ls", "PB_PROGRESS_EVERY=2")
	r.write(toolReply, assistantMsg("tool_use", "Second step."), toolUse)
	r.preRun("Bash", "ls", "PB_PROGRESS_EVERY=2") // too soon: the text waits for the next call
	if got := r.sent(); len(got) != 1 || got[0] != "First step." {
		t.Fatalf("%q", got)
	}
	time.Sleep(2100 * time.Millisecond)
	r.preRun("Bash", "ls", "PB_PROGRESS_EVERY=2")
	if got := r.sent(); len(got) != 2 || got[1] != "Second step." {
		t.Fatalf("%q: what was held back must go with the first call after the interval", got)
	}
}

func TestACallWithNothingNewQueuesNothingAndDoesNotStartTheInterval(t *testing.T) {
	r := newRig(t)
	r.write(human, toolUse) // no words yet
	r.preRun("Bash", "ls", "PB_PROGRESS_EVERY=30")
	if got := r.calls(); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	r.write(toolReply, assistantMsg("tool_use", "Words, right after."), toolUse)
	r.preRun("Bash", "ls", "PB_PROGRESS_EVERY=30")
	if got := r.sent(); len(got) != 1 || got[0] != "Words, right after." {
		t.Fatalf("%q: a call that found nothing to send must not make the next one wait", got)
	}
}

func TestProgressCanBeTurnedOff(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "Some words."), toolUse)
	r.preRun("Read", "", "PB_PROGRESS_EVERY=0")
	if got := r.calls(); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestProgressNeverWaitsForTheSpool(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "Some words."), toolUse)
	r.spoolFile("0001.json")
	_, _, took := r.preRun("Edit", "", "PB_PROGRESS_EVERY=20", "PB_FLUSH_WAIT=8")
	if took > 3*time.Second {
		t.Fatalf("took %v: only a command that may stop the machine waits for the chat", took)
	}
}

// Tool calls made together start their hooks together; the text must be queued once.
func TestToolCallsMadeTogetherQueueTheTextOnce(t *testing.T) {
	for round := 0; round < 3; round++ {
		r := newRig(t)
		r.write(human, assistantMsg("tool_use", "Reading three files."), toolUse)
		done := make(chan struct{})
		for i := 0; i < 4; i++ {
			go func() {
				r.preRun("Read", "", "PB_PROGRESS_EVERY=20")
				done <- struct{}{}
			}()
		}
		for i := 0; i < 4; i++ {
			<-done
		}
		if got := r.sent(); len(got) != 1 || got[0] != "Reading three files." {
			t.Fatalf("round %d: %q", round, got)
		}
	}
}

func TestADeadLockFromAKilledRunIsRemoved(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "Words."), toolUse)
	lock := filepath.Join(r.dir, "state", "progress.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	r.preRun("Read", "", "PB_PROGRESS_EVERY=20")
	if got := r.sent(); len(got) != 1 {
		t.Fatalf("%q: a lock from a run that was killed minutes ago must not keep the text back", got)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("the lock was left behind")
	}
}

func TestAFreshLockLeavesTheTextToTheRunThatHoldsIt(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "Words."), toolUse)
	if err := os.MkdirAll(filepath.Join(r.dir, "state", "progress.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.preRun("Read", "", "PB_PROGRESS_EVERY=20")
	if got := r.calls(); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}
