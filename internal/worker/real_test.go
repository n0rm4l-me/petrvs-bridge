package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// These tests start the real `claude`. They cost a few cents and need a login, so they run only when PB_TEST_REAL_CLAUDE is set:
//
//	PB_TEST_REAL_CLAUDE=1 go test -run TestReal -v -count=1 ./internal/worker/
//
// Each one uses the smallest model, a budget of 0.25 USD, an empty working directory, and settings of the project only: the
// hooks of the user's own settings would send what the test says to the chat.

func realWorker(t *testing.T, mutate func(*Config)) (*Worker, *fakeOut, string) {
	t.Helper()
	if os.Getenv("PB_TEST_REAL_CLAUDE") == "" {
		t.Skip("set PB_TEST_REAL_CLAUDE=1 to run the tests that start the real claude")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	dir := t.TempDir()
	out := &fakeOut{}
	cfg := Config{Name: "Claude Code", Workdir: dir, Model: "haiku", BudgetUSD: 0.25, SettingSources: "project", Out: out,
		TurnTimeout: 2 * time.Minute, ApprovalTimeout: 2 * time.Minute}
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w, out, dir
}

func waitReal(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRealAnAnswerComesBack(t *testing.T) {
	w, out, _ := realWorker(t, nil)
	if err := w.SendText(context.Background(), "Reply with exactly the single word: pong"); err != nil {
		t.Fatal(err)
	}
	waitReal(t, "the answer", func() bool { return out.nReplies() == 1 })
	if got := strings.ToLower(out.reply(0)); !strings.Contains(got, "pong") {
		t.Fatalf("answer %q", out.reply(0))
	}
	if f := w.Facts(); f["worker"] != "idle" || f["worker_cost_usd"] == "0.0000" {
		t.Fatalf("facts %v", f)
	}
}

func TestRealAnApprovedCommandRunsAndARefusedOneDoesNot(t *testing.T) {
	for _, c := range []struct {
		key     string
		file    string
		created bool
	}{{"1", "yes.txt", true}, {"2", "no.txt", false}} {
		w, out, dir := realWorker(t, nil)
		ctx := context.Background()
		if err := w.SendText(ctx, "Run exactly this shell command and nothing else: printf hi > "+c.file+" . Then reply with the single word done."); err != nil {
			t.Fatal(err)
		}
		waitReal(t, "the dialog", func() bool { return out.nNotices() >= 1 })
		n := out.notice(0)
		if !strings.Contains(n.body, "needs your permission to use Bash") || !strings.Contains(n.body, "printf hi > "+c.file) || len(n.buttons) != 3 {
			t.Fatalf("notice %+v", n)
		}
		if err := w.SendKey(ctx, c.key); err != nil {
			t.Fatal(err)
		}
		waitReal(t, "the answer", func() bool { return out.nReplies() == 1 })
		_, err := os.Stat(filepath.Join(dir, c.file))
		if (err == nil) != c.created {
			t.Fatalf("key %s: file exists = %v, want %v (answer %q)", c.key, err == nil, c.created, out.reply(0))
		}
		if !c.created && strings.TrimSpace(out.reply(0)) == "" {
			t.Fatal("a refused request left the chat with no answer")
		}
	}
}

func TestRealThePolicyRefusesWithoutAskingTheChat(t *testing.T) {
	w, out, dir := realWorker(t, func(c *Config) {
		p, err := newPolicy(PolicyFile{DenyBash: []string{`(^|[;&|]\s*)printf\s`}})
		if err != nil {
			t.Fatal(err)
		}
		c.Policy = p
	})
	if err := w.SendText(context.Background(), "Run exactly this shell command and nothing else: printf hi > denied.txt . Then say in one sentence what happened."); err != nil {
		t.Fatal(err)
	}
	waitReal(t, "the answer", func() bool { return out.nReplies() == 1 })
	if _, err := os.Stat(filepath.Join(dir, "denied.txt")); err == nil {
		t.Fatal("the policy refused the command and it ran")
	}
	for i := 0; i < out.nNotices(); i++ {
		if len(out.notice(i).buttons) > 0 {
			t.Fatalf("a refused command was put to the chat: %q", out.notice(i).body)
		}
	}
}

func TestRealTheInstructionsFileChangesHowItAnswers(t *testing.T) {
	file := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(file, []byte("You work through a chat. End every answer with the exact word BANANA on its own line.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, out, _ := realWorker(t, func(c *Config) { c.PromptFile = file })
	if err := w.SendText(context.Background(), "Say hello in one short sentence."); err != nil {
		t.Fatal(err)
	}
	waitReal(t, "the answer", func() bool { return out.nReplies() == 1 })
	if !strings.Contains(out.reply(0), "BANANA") {
		t.Fatalf("the instructions were not followed: %q", out.reply(0))
	}
}

func TestRealAConversationSurvivesTheProcess(t *testing.T) {
	w, out, _ := realWorker(t, nil)
	ctx := context.Background()
	if err := w.SendText(ctx, "Remember this secret word: kiwi. Reply with the single word ok."); err != nil {
		t.Fatal(err)
	}
	waitReal(t, "the first answer", func() bool { return out.nReplies() == 1 })

	// The process ends, as it would after a crash or a restart of the machine; the next message resumes the session.
	w.mu.Lock()
	pr := w.pr
	w.mu.Unlock()
	if pr == nil {
		t.Fatal("no process")
	}
	pr.p.Kill()
	waitReal(t, "the process to be gone", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.pr == nil })

	if err := w.SendText(ctx, "What was the secret word? Reply with just that word."); err != nil {
		t.Fatal(err)
	}
	waitReal(t, "the second answer", func() bool { return out.nReplies() == 2 })
	if got := strings.ToLower(out.reply(1)); !strings.Contains(got, "kiwi") {
		t.Fatalf("the conversation was not resumed: %q", out.reply(1))
	}
}

// The whole way with the real assistant: a message in the (fake) chat, the real hub and agent, a worker that starts the real
// claude, a dialog with buttons in the chat, a tap, and the file the approved command wrote.
func TestRealTheChatDrivesTheRealAssistantThroughTheHub(t *testing.T) {
	if os.Getenv("PB_TEST_REAL_CLAUDE") == "" {
		t.Skip("set PB_TEST_REAL_CLAUDE=1 to run the tests that start the real claude")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	dir := t.TempDir()
	var w *Worker
	r := testrig.New(t, testrig.Options{Slow: true, Agent: func(name string, c *agentsvc.Config) {
		var err error
		w, err = New(Config{Name: "Claude Code", Workdir: dir, Model: "haiku", BudgetUSD: 0.25, SettingSources: "project",
			TurnTimeout: 2 * time.Minute, ApprovalTimeout: 2 * time.Minute, Out: SpoolOutbox{Spool: c.Spool}})
		if err != nil {
			t.Fatal(err)
		}
		c.Exec, c.Screen = w, w
	}})
	t.Cleanup(w.Close)
	r.StartAll()
	r.WaitConnected("laptop")

	r.Say("Reply with exactly the single word: pong")
	r.Eventually("the answer in the chat", func() bool { return strings.Contains(strings.ToLower(strings.Join(r.SentTexts(), "\n")), "pong") })

	r.Say("Run exactly this shell command and nothing else: printf hi > chat.txt . Then reply with the single word finished.")
	dialog := r.WaitButtons("needs your permission to use Bash")
	yes := buttonData(dialog, "1. Yes")
	if !strings.HasPrefix(yes, "k:laptop:1:") {
		t.Fatalf("buttons %+v", dialog.Buttons)
	}
	r.PressOn(dialog.ID, yes)
	r.Eventually("the final answer in the chat", func() bool { return strings.Contains(strings.ToLower(strings.Join(r.SentTexts(), "\n")), "finished") })
	if b, err := os.ReadFile(filepath.Join(dir, "chat.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("the approved command did not write the file: %q %v", b, err)
	}
}

// The hooks of a project must not fire in the worker: with the bridge's own hooks in the working directory, every answer would go
// to the chat twice.
func TestRealTheHooksOfTheProjectDoNotFire(t *testing.T) {
	w, out, dir := realWorker(t, func(c *Config) { c.SettingSources = "project,local" })
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "hook-fired")
	hook := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "touch ` + marker + `"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), []byte(hook), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.SendText(context.Background(), "Reply with the single word: ok"); err != nil {
		t.Fatal(err)
	}
	waitReal(t, "the answer", func() bool { return out.nReplies() == 1 })
	time.Sleep(500 * time.Millisecond) // a Stop hook runs as the turn ends
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook of the project fired in the worker")
	}
}
