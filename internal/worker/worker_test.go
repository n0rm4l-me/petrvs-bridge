package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// fakeProc stands in for `claude -p`: the test writes the JSON lines the assistant would write and reads those the worker sends.
type fakeProc struct {
	inR    *io.PipeReader
	inW    *io.PipeWriter
	outR   *io.PipeReader
	outW   *io.PipeWriter
	sent   chan map[string]any
	killed chan struct{}
	exited chan struct{}
	once   sync.Once
	err    error
	stderr string
}

func newFakeProc() *fakeProc {
	f := &fakeProc{sent: make(chan map[string]any, 64), killed: make(chan struct{}), exited: make(chan struct{})}
	f.inR, f.inW = io.Pipe()
	f.outR, f.outW = io.Pipe()
	go func() {
		dec := json.NewDecoder(f.inR)
		for {
			var m map[string]any
			if dec.Decode(&m) != nil {
				return
			}
			f.sent <- m
		}
	}()
	return f
}

func (f *fakeProc) Stdin() io.WriteCloser { return f.inW }
func (f *fakeProc) Stdout() io.Reader     { return f.outR }
func (f *fakeProc) Wait() error           { <-f.exited; return f.err }
func (f *fakeProc) Stderr() string        { return f.stderr }
func (f *fakeProc) Kill()                 { f.end(errors.New("signal: killed"), true) }

func (f *fakeProc) end(err error, killed bool) {
	f.once.Do(func() {
		f.err = err
		if killed {
			close(f.killed)
		}
		f.inR.Close()
		f.outW.Close()
		close(f.exited)
	})
}

// emit writes one JSON line as the assistant would.
func (f *fakeProc) emit(v any) { _ = json.NewEncoder(f.outW).Encode(v) }

func (f *fakeProc) init(session string) {
	f.emit(map[string]any{"type": "system", "subtype": "init", "session_id": session})
}

func (f *fakeProc) result(text string, cost float64) {
	f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "total_cost_usd": cost})
}

func (f *fakeProc) toolRequest(id, tool string, input map[string]any, desc string) {
	f.emit(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": tool, "input": input, "description": desc,
	}})
}

type fakeOut struct {
	mu      sync.Mutex
	replies []string
	notices []notice
}

type notice struct {
	body    string
	buttons []wire.Button
}

func (o *fakeOut) Reply(body string) { o.mu.Lock(); o.replies = append(o.replies, body); o.mu.Unlock() }
func (o *fakeOut) Notice(body string, b []wire.Button) {
	o.mu.Lock()
	o.notices = append(o.notices, notice{body, b})
	o.mu.Unlock()
}
func (o *fakeOut) nReplies() int { o.mu.Lock(); defer o.mu.Unlock(); return len(o.replies) }
func (o *fakeOut) nNotices() int { o.mu.Lock(); defer o.mu.Unlock(); return len(o.notices) }
func (o *fakeOut) reply(i int) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.replies[i]
}
func (o *fakeOut) notice(i int) notice { o.mu.Lock(); defer o.mu.Unlock(); return o.notices[i] }

type rig struct {
	w      *Worker
	out    *fakeOut
	mu     sync.Mutex
	args   [][]string
	procs  []*fakeProc
	launch int
}

func newRig(t *testing.T, mutate func(*Config)) *rig {
	t.Helper()
	r := &rig{out: &fakeOut{}}
	cfg := Config{
		Name: "laptop", Workdir: t.TempDir(), Out: r.out,
		Launch: func(ctx context.Context, args []string, dir string) (Process, error) {
			f := newFakeProc()
			r.mu.Lock()
			r.args = append(r.args, args)
			r.procs = append(r.procs, f)
			r.mu.Unlock()
			return f, nil
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	r.w = w
	return r
}

func (r *rig) proc(t *testing.T, n int) *fakeProc {
	t.Helper()
	eventually(t, "the assistant to be started", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.procs) >= n })
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[n-1]
}

func (r *rig) argsOf(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.args[n-1]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nextSent(t *testing.T, f *fakeProc) map[string]any {
	t.Helper()
	select {
	case m := <-f.sent:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("the worker sent nothing to the assistant")
		return nil
	}
}

func noSent(t *testing.T, f *fakeProc, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sent:
		t.Fatalf("the worker sent something it should not have: %v", m)
	case <-time.After(d):
	}
}

func contains(args []string, a ...string) bool {
	for i := range args {
		if args[i] == a[0] && (len(a) == 1 || (i+1 < len(args) && args[i+1] == a[1])) {
			return true
		}
	}
	return false
}

func content(m map[string]any) string {
	msg, _ := m["message"].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

func decision(t *testing.T, m map[string]any) (id, behavior, message string, input map[string]any) {
	t.Helper()
	if m["type"] != "control_response" {
		t.Fatalf("not a control response: %v", m)
	}
	resp := m["response"].(map[string]any)
	body := resp["response"].(map[string]any)
	input, _ = body["updatedInput"].(map[string]any)
	message, _ = body["message"].(string)
	return resp["request_id"].(string), body["behavior"].(string), message, input
}

func capture(t *testing.T, w *Worker) string {
	t.Helper()
	s, err := w.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestATextIsGivenToTheAssistantAndTheResultIsTheReply(t *testing.T) {
	r := newRig(t, nil)
	if err := r.w.SendText(context.Background(), "  hello there \n"); err != nil {
		t.Fatal(err)
	}
	f := r.proc(t, 1)
	args := r.argsOf(1)
	for _, want := range [][]string{{"-p"}, {"--input-format", "stream-json"}, {"--output-format", "stream-json"}, {"--verbose"},
		{"--permission-prompt-tool", "stdio"}, {"--permission-prompts", "host"}, {"--setting-sources", "project,local"}, {"--max-budget-usd", "5"}} {
		if !contains(args, want...) {
			t.Errorf("the assistant was started without %v: %v", want, args)
		}
	}
	if contains(args, "--resume") {
		t.Errorf("the first start resumes nothing: %v", args)
	}
	m := nextSent(t, f)
	if m["type"] != "user" || content(m) != "hello there" {
		t.Fatalf("message to the assistant: %v", m)
	}
	f.init("sess-1")
	f.result("hi, I am here", 0.0123)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	if got := r.out.reply(0); got != "hi, I am here" {
		t.Fatalf("reply %q", got)
	}
	if scr := capture(t, r.w); !strings.Contains(scr, "idle") || !strings.Contains(scr, "$0.0123") || !strings.Contains(scr, "sess-1") {
		t.Fatalf("status screen:\n%s", scr)
	}
	if f := r.w.Facts(); f["worker"] != "idle" || f["worker_cost_usd"] != "0.0123" {
		t.Fatalf("facts %v", f)
	}
}

func TestARequestForPermissionIsPutToTheChatAndAnsweredByAKey(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if err := r.w.SendText(ctx, "write the file"); err != nil {
		t.Fatal(err)
	}
	f := r.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("req-1", "Bash", map[string]any{"command": "printf hi > x.txt"}, "Write hi to x.txt")
	eventually(t, "the dialog in the chat", func() bool { return r.out.nNotices() == 1 })

	n := r.out.notice(0)
	if !strings.Contains(n.body, "needs your permission to use Bash") || !strings.Contains(n.body, "printf hi > x.txt") {
		t.Fatalf("notice:\n%s", n.body)
	}
	scr := capture(t, r.w)
	p, ok := screen.ExtractPrompt(scr)
	if !ok || len(p.Options) != 2 || p.Options[0].Key != "1" || p.Options[1].Key != "2" {
		t.Fatalf("the virtual screen holds no prompt: %v %v\n%s", ok, p, scr)
	}
	// The buttons are the ones the agent checks: one per choice and Escape, each bound to the hash of the dialog on the screen.
	if len(n.buttons) != 3 {
		t.Fatalf("buttons %v", n.buttons)
	}
	for _, b := range n.buttons {
		if b.Prompt != p.Hash {
			t.Fatalf("button %+v is not bound to the dialog %s", b, p.Hash)
		}
	}
	if f := r.w.Facts(); f["worker"] != "waiting for permission" {
		t.Fatalf("facts %v", f)
	}
	// A message cannot be typed while the dialog waits.
	if err := r.w.SendText(ctx, "and also this"); err == nil {
		t.Fatal("text was taken while a request waits for permission")
	}
	noSent(t, f, 50*time.Millisecond)

	if err := r.w.SendKey(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	id, behavior, _, input := decision(t, nextSent(t, f))
	if id != "req-1" || behavior != "allow" || input["command"] != "printf hi > x.txt" {
		t.Fatalf("answer: %s %s %v", id, behavior, input)
	}
	if _, ok := screen.ExtractPrompt(capture(t, r.w)); ok {
		t.Fatal("the dialog is still on the screen after the answer")
	}
	// An old button pressed again answers nothing.
	if err := r.w.SendKey(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	noSent(t, f, 50*time.Millisecond)

	f.result("done", 0.01)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
}

func TestTheChatCanRefuseARequest(t *testing.T) {
	for _, key := range []string{"2", "Escape"} {
		r := newRig(t, nil)
		_ = r.w.SendText(context.Background(), "go")
		f := r.proc(t, 1)
		nextSent(t, f)
		f.toolRequest("req-9", "Bash", map[string]any{"command": "touch y"}, "")
		eventually(t, "the dialog", func() bool { return r.out.nNotices() == 1 })
		if err := r.w.SendKey(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		id, behavior, msg, _ := decision(t, nextSent(t, f))
		if id != "req-9" || behavior != "deny" || !strings.Contains(msg, "refused") {
			t.Fatalf("key %s: %s %s %q", key, id, behavior, msg)
		}
	}
}

func TestKeysThatMeanNothingWithoutATerminalAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	_ = r.w.SendText(context.Background(), "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("req-1", "Bash", map[string]any{"command": "touch y"}, "")
	eventually(t, "the dialog", func() bool { return r.out.nNotices() == 1 })
	for _, k := range []string{"Up", "Down", "Enter", "Tab", "3"} {
		if err := r.w.SendKey(context.Background(), k); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	noSent(t, f, 50*time.Millisecond)
	if _, ok := screen.ExtractPrompt(capture(t, r.w)); !ok {
		t.Fatal("a key that is not an answer took the dialog away")
	}
}

func TestThePolicyAllowsDeniesAndFlags(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	_ = r.w.SendText(ctx, "go")
	f := r.proc(t, 1)
	nextSent(t, f)

	f.toolRequest("a", "Read", map[string]any{"file_path": "/tmp/x"}, "")
	if id, b, _, in := decision(t, nextSent(t, f)); id != "a" || b != "allow" || in["file_path"] != "/tmp/x" {
		t.Fatalf("a read: %s %s %v", id, b, in)
	}
	if r.out.nNotices() != 0 {
		t.Fatal("a read was put to the chat")
	}

	f.toolRequest("b", "Bash", map[string]any{"command": "rm -rf /"}, "")
	if id, b, msg, _ := decision(t, nextSent(t, f)); id != "b" || b != "deny" || !strings.Contains(msg, "policy") {
		t.Fatalf("rm -rf /: %s %s %q", id, b, msg)
	}
	eventually(t, "the notice of the refusal", func() bool { return r.out.nNotices() == 1 })
	if !strings.Contains(r.out.notice(0).body, "refused") || len(r.out.notice(0).buttons) != 0 {
		t.Fatalf("notice %+v", r.out.notice(0))
	}

	f.toolRequest("c", "Bash", map[string]any{"command": "kubectl --context=prod -n x apply -f deploy.yaml"}, "")
	eventually(t, "the dialog of the risky command", func() bool { return r.out.nNotices() == 2 })
	if scr := capture(t, r.w); !strings.Contains(scr, "⚠ changes a Kubernetes cluster") {
		t.Fatalf("the dialog does not flag the command:\n%s", scr)
	}
	if !strings.Contains(r.out.notice(1).body, "changes a Kubernetes cluster") {
		t.Fatalf("the notice does not flag the command:\n%s", r.out.notice(1).body)
	}
}

func TestMessagesWaitForTheRunningRequestInOrder(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxQueued = 2 })
	ctx := context.Background()
	_ = r.w.SendText(ctx, "first")
	f := r.proc(t, 1)
	if content(nextSent(t, f)) != "first" {
		t.Fatal("first")
	}
	if err := r.w.SendText(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if err := r.w.SendText(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	if err := r.w.SendText(ctx, "fourth"); err == nil {
		t.Fatal("a queue over its limit took a message")
	}
	noSent(t, f, 50*time.Millisecond)
	if scr := capture(t, r.w); !strings.Contains(scr, "2 message(s) waiting") {
		t.Fatalf("status:\n%s", scr)
	}
	f.result("answer one", 0.01)
	if content(nextSent(t, f)) != "second" {
		t.Fatal("second did not follow the first")
	}
	f.result("answer two", 0.02)
	if content(nextSent(t, f)) != "third" {
		t.Fatal("third did not follow the second")
	}
	f.result("answer three", 0.03)
	eventually(t, "three replies", func() bool { return r.out.nReplies() == 3 })
	for i, want := range []string{"answer one", "answer two", "answer three"} {
		if got := r.out.reply(i); got != want {
			t.Errorf("reply %d = %q, want %q", i, got, want)
		}
	}
}

func TestAProcessThatEndsInTheMiddleIsReportedAndResumed(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	_ = r.w.SendText(ctx, "long job")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.init("sess-7")
	f.stderr = "boom: out of memory"
	f.emit(map[string]any{"type": "system", "subtype": "noise"}) // makes sure the init above was read
	f.end(errors.New("exit status 1"), false)
	eventually(t, "the notice of the end", func() bool { return r.out.nNotices() == 1 })
	body := r.out.notice(0).body
	if !strings.Contains(body, "ended in the middle of a request") || !strings.Contains(body, "exit status 1") || !strings.Contains(body, "boom") {
		t.Fatalf("notice:\n%s", body)
	}
	if err := r.w.SendText(ctx, "try again"); err != nil {
		t.Fatal(err)
	}
	f2 := r.proc(t, 2)
	if !contains(r.argsOf(2), "--resume", "sess-7") {
		t.Fatalf("the second start does not resume the conversation: %v", r.argsOf(2))
	}
	if content(nextSent(t, f2)) != "try again" {
		t.Fatal("the message did not reach the new process")
	}
}

func TestAProcessThatEndsBetweenRequestsIsStartedAgainQuietly(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	_ = r.w.SendText(ctx, "one")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.init("sess-2")
	f.result("ok", 0)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	f.end(nil, false)
	time.Sleep(50 * time.Millisecond)
	if r.out.nNotices() != 0 {
		t.Fatalf("a process that ended while idle was reported: %v", r.out.notice(0))
	}
	_ = r.w.SendText(ctx, "two")
	f2 := r.proc(t, 2)
	if !contains(r.argsOf(2), "--resume", "sess-2") || content(nextSent(t, f2)) != "two" {
		t.Fatalf("restart: %v", r.argsOf(2))
	}
}

func TestADialogNobodyAnswersIsRefused(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ApprovalTimeout = 40 * time.Millisecond })
	_ = r.w.SendText(context.Background(), "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("slow", "Bash", map[string]any{"command": "touch z"}, "")
	eventually(t, "the dialog", func() bool { return r.out.nNotices() == 1 })
	id, behavior, msg, _ := decision(t, nextSent(t, f))
	if id != "slow" || behavior != "deny" || !strings.Contains(msg, "Nobody answered") {
		t.Fatalf("%s %s %q", id, behavior, msg)
	}
	eventually(t, "the notice of the timeout", func() bool { return r.out.nNotices() == 2 })
	if !strings.Contains(r.out.notice(1).body, "got no answer") {
		t.Fatalf("notice %q", r.out.notice(1).body)
	}
	if _, ok := screen.ExtractPrompt(capture(t, r.w)); ok {
		t.Fatal("the dialog is still on the screen")
	}
}

func TestARequestThatRunsTooLongIsInterruptedThenKilled(t *testing.T) {
	r := newRig(t, func(c *Config) { c.TurnTimeout = 40 * time.Millisecond; c.KillGrace = 80 * time.Millisecond })
	_ = r.w.SendText(context.Background(), "forever")
	f := r.proc(t, 1)
	nextSent(t, f)
	m := nextSent(t, f)
	req, _ := m["request"].(map[string]any)
	if m["type"] != "control_request" || req["subtype"] != "interrupt" {
		t.Fatalf("expected an interrupt, got %v", m)
	}
	select {
	case <-f.killed:
	case <-time.After(3 * time.Second):
		t.Fatal("the assistant was not killed after the grace period")
	}
	eventually(t, "the notices", func() bool { return r.out.nNotices() >= 1 })
	if !strings.Contains(r.out.notice(0).body, "was interrupted") {
		t.Fatalf("notice %q", r.out.notice(0).body)
	}
}

func TestEscapeInterruptsARunningRequest(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	_ = r.w.SendText(ctx, "work")
	f := r.proc(t, 1)
	nextSent(t, f)
	if err := r.w.SendKey(ctx, "Escape"); err != nil {
		t.Fatal(err)
	}
	m := nextSent(t, f)
	if req, _ := m["request"].(map[string]any); m["type"] != "control_request" || req["subtype"] != "interrupt" {
		t.Fatalf("expected an interrupt, got %v", m)
	}
	// With nothing running, Escape does nothing.
	f.result("stopped", 0)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	if err := r.w.SendKey(ctx, "Escape"); err != nil {
		t.Fatal(err)
	}
	noSent(t, f, 50*time.Millisecond)
}

func TestAnErrorResultIsSaidInPlainWords(t *testing.T) {
	r := newRig(t, func(c *Config) { c.BudgetUSD = 2.5 })
	_ = r.w.SendText(context.Background(), "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.emit(map[string]any{"type": "result", "subtype": "error_max_budget_usd", "is_error": true, "total_cost_usd": 2.51})
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	if got := r.out.reply(0); !strings.Contains(got, "$2.5") || !strings.Contains(got, "/clear") {
		t.Fatalf("reply %q", got)
	}
}

func TestAnEmptyAnswerStillGetsAReply(t *testing.T) {
	r := newRig(t, nil)
	_ = r.w.SendText(context.Background(), "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.result("", 0)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	if !strings.Contains(r.out.reply(0), "without a text answer") {
		t.Fatalf("reply %q", r.out.reply(0))
	}
}

func TestTheInstructionsFileIsReadWhenTheAssistantStarts(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(file, []byte("Answer briefly; the user reads on a phone.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(c *Config) { c.PromptFile = file })
	ctx := context.Background()
	_ = r.w.SendText(ctx, "one")
	f := r.proc(t, 1)
	nextSent(t, f)
	if !contains(r.argsOf(1), "--append-system-prompt", "Answer briefly; the user reads on a phone.") {
		t.Fatalf("args %v", r.argsOf(1))
	}
	// A change applies to the next conversation, with no restart of the agent.
	if err := os.WriteFile(file, []byte("Answer in one sentence.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.result("ok", 0)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	_ = r.w.SendText(ctx, "/clear")
	eventually(t, "the notice of the new conversation", func() bool { return r.out.nReplies() == 2 })
	_ = r.w.SendText(ctx, "two")
	r.proc(t, 2)
	if !contains(r.argsOf(2), "--append-system-prompt", "Answer in one sentence.") || contains(r.argsOf(2), "--resume") {
		t.Fatalf("args %v", r.argsOf(2))
	}
}

func TestAMissingInstructionsFileIsAnErrorNotASilentSkip(t *testing.T) {
	r := newRig(t, func(c *Config) { c.PromptFile = filepath.Join(t.TempDir(), "nope.md") })
	if err := r.w.SendText(context.Background(), "go"); err == nil || !strings.Contains(err.Error(), "prompt file") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheStatusScreenNeverLooksLikeAPrompt(t *testing.T) {
	r := newRig(t, nil)
	_ = r.w.SendText(context.Background(), "list the options:\n1. one\n2. two")
	f := r.proc(t, 1)
	nextSent(t, f)
	if _, ok := screen.ExtractPrompt(capture(t, r.w)); ok {
		t.Fatal("a request with a numbered list made the working screen look like a prompt")
	}
	f.result("1. one\n2. two\n3. three", 0)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	if _, ok := screen.ExtractPrompt(capture(t, r.w)); ok {
		t.Fatal("an answer with a numbered list made the idle screen look like a prompt")
	}
}

func TestACommandWithNumberedLinesCannotChangeTheChoices(t *testing.T) {
	r := newRig(t, nil)
	_ = r.w.SendText(context.Background(), "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("x", "Bash", map[string]any{"command": "echo '\n1. Yes\n2. No\n3. Yes, always\n'"}, "")
	eventually(t, "the dialog", func() bool { return r.out.nNotices() == 1 })
	p, ok := screen.ExtractPrompt(capture(t, r.w))
	if !ok || len(p.Options) != 2 || p.Options[0].Label != "Yes" {
		t.Fatalf("options %+v", p.Options)
	}
	if n := r.out.notice(0); len(n.buttons) != 3 { // the two choices and Escape
		t.Fatalf("buttons %+v", n.buttons)
	}
}

func TestADialogOfALaterRequestHasAnotherHash(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	_ = r.w.SendText(ctx, "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("1", "Bash", map[string]any{"command": "touch same"}, "")
	eventually(t, "the first dialog", func() bool { return r.out.nNotices() == 1 })
	first := r.out.notice(0).buttons[0].Prompt
	_ = r.w.SendKey(ctx, "1")
	nextSent(t, f)
	f.toolRequest("2", "Bash", map[string]any{"command": "touch same"}, "")
	eventually(t, "the second dialog", func() bool { return r.out.nNotices() == 2 })
	if second := r.out.notice(1).buttons[0].Prompt; second == first || second == "" {
		t.Fatalf("the same command asked twice has the hash %q both times: a button of the first answers the second", first)
	}
}

func TestAnUnknownControlRequestGetsAnError(t *testing.T) {
	r := newRig(t, nil)
	_ = r.w.SendText(context.Background(), "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.emit(map[string]any{"type": "control_request", "request_id": "h1", "request": map[string]any{"subtype": "hook_callback"}})
	m := nextSent(t, f)
	resp, _ := m["response"].(map[string]any)
	if m["type"] != "control_response" || resp["subtype"] != "error" || resp["request_id"] != "h1" {
		t.Fatalf("answer %v", m)
	}
}

func TestClosingEndsTheAssistantAndRefusesText(t *testing.T) {
	r := newRig(t, nil)
	_ = r.w.SendText(context.Background(), "go")
	f := r.proc(t, 1)
	nextSent(t, f)
	r.w.Close()
	select {
	case <-f.killed:
	case <-time.After(3 * time.Second):
		t.Fatal("the assistant was not ended")
	}
	if err := r.w.SendText(context.Background(), "again"); err == nil {
		t.Fatal("a closed worker took text")
	}
	time.Sleep(50 * time.Millisecond)
	if r.out.nNotices() != 0 {
		t.Fatalf("closing was reported as a crash: %q", r.out.notice(0).body)
	}
}

func TestTheWorkerNeedsAWorkdirAndAnOutbox(t *testing.T) {
	if _, err := New(Config{Out: &fakeOut{}}); err == nil {
		t.Error("no workdir was accepted")
	}
	if _, err := New(Config{Workdir: t.TempDir()}); err == nil {
		t.Error("no outbox was accepted")
	}
}

func TestAScreenAndAPaneThatTheAgentCanUse(t *testing.T) {
	r := newRig(t, nil)
	if cmd, err := r.w.PaneCommand(context.Background()); err != nil || cmd != "claude" {
		t.Fatalf("pane command %q %v", cmd, err)
	}
	if err := r.w.Start(context.Background(), "/tmp", "claude"); err != nil {
		t.Fatal(err)
	}
	if scr := capture(t, r.w); !strings.Contains(scr, "idle, waiting for a message") {
		t.Fatalf("status:\n%s", scr)
	}
}

func screenPrompt(t *testing.T, w *Worker) (screen.Prompt, bool) {
	t.Helper()
	return screen.ExtractPrompt(capture(t, w))
}

func TestTheConversationIsResumedAfterTheAgentRestarts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "worker-session")
	r := newRig(t, func(c *Config) { c.SessionFile = file })
	ctx := context.Background()
	_ = r.w.SendText(ctx, "remember the number 42")
	f := r.proc(t, 1)
	nextSent(t, f)
	f.init("sess-keep")
	f.result("ok", 0)
	eventually(t, "the reply", func() bool { return r.out.nReplies() == 1 })
	eventually(t, "the id to be kept", func() bool {
		b, _ := os.ReadFile(file)
		return strings.TrimSpace(string(b)) == "sess-keep"
	})
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file of the id: %v %v", fi, err)
	}
	r.w.Close()

	// A new worker, as after a restart of the machine, resumes it.
	r2 := newRig(t, func(c *Config) { c.SessionFile = file })
	_ = r2.w.SendText(ctx, "what number?")
	r2.proc(t, 1)
	if !contains(r2.argsOf(1), "--resume", "sess-keep") {
		t.Fatalf("the conversation was not resumed: %v", r2.argsOf(1))
	}
	// /clear forgets it.
	f2 := r2.proc(t, 1)
	nextSent(t, f2)
	f2.result("42", 0)
	eventually(t, "the reply", func() bool { return r2.out.nReplies() == 1 })
	_ = r2.w.SendText(ctx, "/clear")
	eventually(t, "the file to go", func() bool { _, err := os.Stat(file); return err != nil })
}

func TestAnIdThatCannotBeAnIdIsNotUsed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "worker-session")
	if err := os.WriteFile(file, []byte("abc; rm -rf ~\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(c *Config) { c.SessionFile = file })
	_ = r.w.SendText(context.Background(), "hi")
	r.proc(t, 1)
	if contains(r.argsOf(1), "--resume") {
		t.Fatalf("an id from a file that is not an id was passed on: %v", r.argsOf(1))
	}
}
