// Package hooks tests the example hooks for the assistant, which are shell scripts.
package hooks

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	human     = `{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"question"}}`
	toolReply = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`
)

func assistant(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, text)
}

// assistantMsg is a line as Claude Code writes it: every line of a message carries the message's stop_reason, "tool_use"
// while the assistant still has a tool to call and "end_turn" on the final message.
func assistantMsg(stop, text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","stop_reason":%q,"content":[{"type":"text","text":%q}]}}`, stop, text)
}

const toolUse = `{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","name":"ListAgents","input":{}}]}}`

// rig is a transcript, a state directory and a stand-in for the agent binary that records what `agent send -` is given.
type rig struct {
	t          *testing.T
	dir        string
	transcript string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "agent")
	script := "#!/bin/sh\nn=$(ls \"" + dir + "\"/sent.* 2>/dev/null | wc -l | tr -d ' ')\necho \"$*\" > \"" + dir + "/args.$n\"\ncat > \"" + dir + "/sent.$n\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, dir: dir, transcript: filepath.Join(dir, "session.jsonl")}
}

func (r *rig) write(lines ...string) {
	r.t.Helper()
	f, err := os.OpenFile(r.transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

func (r *rig) command() *exec.Cmd {
	cmd := exec.Command("sh", "../../examples/claude-stop-hook.sh")
	cmd.Env = append(os.Environ(), "PB_STATE_DIR="+filepath.Join(r.dir, "state"), "AGENT_BIN="+filepath.Join(r.dir, "agent"))
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`{"transcript_path":%q}`, r.transcript))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return cmd
}

func (r *rig) run() {
	r.t.Helper()
	cmd := r.command()
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("the hook failed: %v: %s", err, cmd.Stdout.(*bytes.Buffer).String())
	}
}

func (r *rig) sent() []string {
	files, _ := filepath.Glob(filepath.Join(r.dir, "sent.*"))
	sort.Strings(files)
	var out []string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out = append(out, string(b))
	}
	return out
}

func (r *rig) marks() int {
	files, _ := filepath.Glob(filepath.Join(r.dir, "state", "hook-*"))
	return len(files)
}

func TestTheAnswerIsQueuedOnceAndOnlyOnce(t *testing.T) {
	r := newRig(t)
	r.write(human, assistant("Answer"))
	r.run()
	r.run() // the next Stop of the same turn must not repeat it
	if got := r.sent(); len(got) != 1 || got[0] != "Answer" {
		t.Fatalf("%q", got)
	}
}

func TestAnAnswerWrittenAfterTheHookStartedIsStillSent(t *testing.T) {
	r := newRig(t)
	r.write(human) // the final answer is not in the transcript yet when the hook runs
	cmd := r.command()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond)
	r.write(toolReply, assistant("Late answer"))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %s", err, cmd.Stdout.(*bytes.Buffer).String())
	}
	if got := r.sent(); len(got) != 1 || got[0] != "Late answer" {
		t.Fatalf("%q", got)
	}
	if r.marks() != 1 {
		t.Fatal("the answer was sent but not marked")
	}
}

// Words before a tool call are a text block of the same turn. The hook once took them for the whole answer, queued only
// them and marked the turn as done, so the answer after the tool (a table of sessions) never reached the chat.
func TestTextBeforeAToolCallIsNotTakenForTheAnswer(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "Let me look."), toolUse) // the answer after the tool is not written yet
	cmd := r.command()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond)
	r.write(toolReply, assistantMsg("end_turn", "The list."))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %s", err, cmd.Stdout.(*bytes.Buffer).String())
	}
	if got := r.sent(); len(got) != 1 || got[0] != "Let me look.\n\nThe list." {
		t.Fatalf("%q", got)
	}
	r.run() // the turn is marked as done: a later Stop of the same turn adds nothing
	if got := r.sent(); len(got) != 1 {
		t.Fatalf("sent again: %q", got)
	}
}

// A turn that stops after the tool call (interrupted) never gets a final message: after the wait, what exists is sent.
func TestAnInterruptedTurnStillSendsItsText(t *testing.T) {
	r := newRig(t)
	r.write(human, assistantMsg("tool_use", "Let me look."), toolUse)
	start := time.Now()
	r.run()
	if time.Since(start) > 10*time.Second {
		t.Fatal("the hook waited too long")
	}
	if got := r.sent(); len(got) != 1 || got[0] != "Let me look." {
		t.Fatalf("%q", got)
	}
}

func TestNothingIsMarkedWhenThereIsNoAnswerYet(t *testing.T) {
	r := newRig(t)
	r.write(human)
	start := time.Now()
	r.run()
	if time.Since(start) > 10*time.Second {
		t.Fatal("the hook waited too long")
	}
	if len(r.sent()) != 0 || r.marks() != 0 {
		t.Fatalf("sent %q, marks %d", r.sent(), r.marks())
	}
	r.write(assistant("It came later")) // the next Stop finds it, because nothing was marked
	r.run()
	if got := r.sent(); len(got) != 1 || got[0] != "It came later" {
		t.Fatalf("%q", got)
	}
}

func TestATurnWithoutANewQuestionSendsOnlyWhatIsNew(t *testing.T) {
	r := newRig(t)
	r.write(human, assistant("First"))
	r.run()
	r.write(assistant("From a background task"))
	r.run()
	if got := r.sent(); len(got) != 2 || got[0] != "First" || got[1] != "From a background task" {
		t.Fatalf("%q", got)
	}
}

func TestATranscriptWithoutAHumanMessageSendsNothing(t *testing.T) {
	r := newRig(t)
	r.write(toolReply, assistant("Unprompted"))
	r.run()
	if len(r.sent()) != 0 {
		t.Fatalf("%q", r.sent())
	}
}

// notify runs the Notification hook with the given payload on its standard input.
func (r *rig) notify(payload string) {
	r.t.Helper()
	cmd := exec.Command("sh", "../../examples/claude-notify-hook.sh")
	cmd.Env = append(os.Environ(), "AGENT_BIN="+filepath.Join(r.dir, "agent"))
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("the hook failed: %v: %s", err, out.String())
	}
}

// Claude Code sends idle_prompt a minute after every finished turn, and auth_success when a login completes: neither
// needs an answer, and forwarding them filled the chat with "waiting for your input" that no prompt stood behind.
func TestNotificationsThatNeedNoAnswerAreNotForwarded(t *testing.T) {
	for _, typ := range []string{"idle_prompt", "auth_success"} {
		r := newRig(t)
		r.notify(fmt.Sprintf(`{"hook_event_name":"Notification","message":"Claude is waiting for your input","notification_type":%q}`, typ))
		if got := r.sent(); len(got) != 0 {
			t.Errorf("%s was forwarded: %q", typ, got)
		}
	}
}

func TestNotificationsThatNeedAnAnswerAreForwarded(t *testing.T) {
	const hint = ". Answer with /key 1–4 (the numbered choices), /enter or /esc."
	for name, payload := range map[string]string{
		"permission prompt":           `{"message":"Claude needs your permission","notification_type":"permission_prompt"}`,
		"elicitation dialog":          `{"message":"Claude needs your permission","notification_type":"elicitation_dialog"}`,
		"a type from a later version": `{"message":"Claude needs your permission","notification_type":"something_new"}`,
		"no type (older Claude Code)": `{"message":"Claude needs your permission"}`,
	} {
		r := newRig(t)
		r.notify(payload)
		got := r.sent()
		if len(got) != 1 || got[0] != "⏳ Claude needs your permission"+hint {
			t.Errorf("%s: %q", name, got)
		}
		if args, _ := os.ReadFile(filepath.Join(r.dir, "args.0")); strings.TrimSpace(string(args)) != "send -type notice -" {
			t.Errorf("%s: the agent was called with %q", name, args)
		}
	}
}

func TestANotificationWithoutAMessageIsNotForwarded(t *testing.T) {
	r := newRig(t)
	r.notify(`{"notification_type":"permission_prompt"}`)
	r.notify(`not json`)
	if got := r.sent(); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}
