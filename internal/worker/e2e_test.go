package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// These tests run the whole way: a message typed in the (fake) chat, the real hub, the real agent with a worker in place of
// tmux, and an assistant that is a script. What they show is that nothing in the agent or the hub had to change: the text,
// the dialog, the buttons bound to its hash, /screen and the refusal of an old button all work with a worker as they do with
// a terminal.

type scripted struct {
	mu    sync.Mutex
	procs []*fakeProc
}

func (s *scripted) launch(ctx context.Context, args []string, dir string) (Process, error) {
	f := newFakeProc()
	s.mu.Lock()
	s.procs = append(s.procs, f)
	s.mu.Unlock()
	return f, nil
}

func (s *scripted) proc(t *testing.T, n int) *fakeProc {
	t.Helper()
	eventually(t, "the assistant to be started", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.procs) >= n })
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[n-1]
}

func workerRig(t *testing.T, mutate func(*Config)) (*testrig.Rig, *Worker, *scripted) {
	t.Helper()
	sc := &scripted{}
	var w *Worker
	r := testrig.New(t, testrig.Options{Agent: func(name string, c *agentsvc.Config) {
		cfg := Config{Name: "Claude Code", Workdir: t.TempDir(), Out: SpoolOutbox{Spool: c.Spool}, Launch: sc.launch}
		if mutate != nil {
			mutate(&cfg)
		}
		var err error
		if w, err = New(cfg); err != nil {
			t.Fatal(err)
		}
		c.Exec, c.Screen = w, w
	}})
	t.Cleanup(w.Close)
	r.StartAll()
	r.WaitConnected("laptop")
	return r, w, sc
}

func buttonData(m telegram.Sent, label string) string {
	for _, row := range m.Buttons {
		for _, b := range row {
			if strings.HasPrefix(b.Text, label) {
				return b.Data
			}
		}
	}
	return ""
}

func TestTheChatTalksToAnAssistantWithoutATerminal(t *testing.T) {
	r, _, sc := workerRig(t, nil)

	r.Say("what is in this folder?")
	f := sc.proc(t, 1)
	if got := content(nextSent(t, f)); got != "what is in this folder?" {
		t.Fatalf("the assistant got %q", got)
	}
	f.init("sess-e2e")
	f.result("Two files: README.md and main.go.", 0.004)
	r.WaitSent("Two files: README.md and main.go.")
}

func TestARequestForPermissionComesToTheChatAsButtonsAndTheAnswerGoesBack(t *testing.T) {
	r, _, sc := workerRig(t, nil)

	r.Say("create hello.txt")
	f := sc.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("req-A", "Bash", map[string]any{"command": "printf hello > hello.txt"}, "Create hello.txt")

	dialog := r.WaitButtons("needs your permission to use Bash")
	if !strings.Contains(dialog.Text, "printf hello > hello.txt") {
		t.Fatalf("the command is not shown to the user: %q", dialog.Text)
	}
	yes := buttonData(dialog, "1. Yes")
	if !strings.HasPrefix(yes, "k:laptop:1:") {
		t.Fatalf("the button for Yes: %q (%+v)", yes, dialog.Buttons)
	}
	// While it waits, /screen shows the same dialog and a text is refused.
	r.Say("/screen")
	r.Eventually("the screen with the dialog", func() bool {
		n := 0
		for _, s := range r.SentWithButtons() {
			if strings.Contains(s.Text, "printf hello > hello.txt") {
				n++
			}
		}
		return n >= 2
	})

	r.PressOn(dialog.ID, yes)
	id, behavior, _, input := decision(t, nextSent(t, f))
	if id != "req-A" || behavior != "allow" || input["command"] != "printf hello > hello.txt" {
		t.Fatalf("what the assistant got: %s %s %v", id, behavior, input)
	}
	f.result("Created hello.txt.", 0.01)
	r.WaitSent("Created hello.txt.")
}

func TestTheChatCanRefuseWithTheEscButton(t *testing.T) {
	r, _, sc := workerRig(t, nil)
	r.Say("delete the build folder")
	f := sc.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("req-B", "Bash", map[string]any{"command": "rm -rf ./build"}, "")
	dialog := r.WaitButtons("needs your permission to use Bash")
	esc := buttonData(dialog, "✖ Esc")
	if esc == "" {
		t.Fatalf("no Esc button: %+v", dialog.Buttons)
	}
	r.PressOn(dialog.ID, esc)
	id, behavior, msg, _ := decision(t, nextSent(t, f))
	if id != "req-B" || behavior != "deny" || !strings.Contains(msg, "refused") {
		t.Fatalf("%s %s %q", id, behavior, msg)
	}
}

// A button of a dialog that is gone must not answer the dialog that came after it, even for the same command.
func TestAButtonOfAnEarlierDialogCannotAnswerALaterOne(t *testing.T) {
	r, w, sc := workerRig(t, func(c *Config) { c.ApprovalTimeout = 150 * time.Millisecond })
	r.Say("go")
	f := sc.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("old", "Bash", map[string]any{"command": "touch same"}, "")
	first := r.WaitButtons("needs your permission to use Bash")
	oldYes := buttonData(first, "1. Yes")

	// Nobody answers the first dialog in time: the request is refused, and the assistant asks again for the same command.
	if id, behavior, _, _ := decision(t, nextSent(t, f)); id != "old" || behavior != "deny" {
		t.Fatalf("the first request: %s %s", id, behavior)
	}
	w.setApprovalTimeout(time.Hour)
	f.toolRequest("new", "Bash", map[string]any{"command": "touch same"}, "")
	r.Eventually("the second dialog", func() bool { return len(r.SentWithButtons()) >= 2 })

	// The tap on the old Yes is refused by the agent, and the new request is still waiting.
	r.PressOn(first.ID, oldYes)
	r.Eventually("the refusal", func() bool {
		return strings.Contains(strings.Join(r.TG.Answered(), "\n")+strings.Join(r.SentTexts(), "\n"), "no longer on the screen")
	})
	noSent(t, f, 100*time.Millisecond)
	if _, ok := screenPrompt(t, w); !ok {
		t.Fatal("the later request lost its dialog")
	}
}

func TestAMessageWhileADialogWaitsIsRefusedInTheChat(t *testing.T) {
	r, _, sc := workerRig(t, nil)
	r.Say("go")
	f := sc.proc(t, 1)
	nextSent(t, f)
	f.toolRequest("x", "Bash", map[string]any{"command": "touch q"}, "")
	r.WaitButtons("needs your permission to use Bash")
	r.Say("and one more thing")
	r.WaitSent("a prompt is waiting for an answer")
	noSent(t, f, 100*time.Millisecond)
}

func TestTheScreenOfAnIdleWorkerIsAStatus(t *testing.T) {
	r, _, _ := workerRig(t, nil)
	r.Say("/screen")
	snap := r.WaitButtons("screen")
	if !strings.Contains(snap.Text, "worker without a terminal") || !strings.Contains(snap.Text, "idle") {
		t.Fatalf("%q", snap.Text)
	}
}

func TestTheStatusGoesToTheHubWithTheHeartbeat(t *testing.T) {
	_, w, _ := workerRig(t, nil)
	if f := w.Facts(); f["worker"] != "idle" {
		t.Fatalf("%v", f)
	}
}

// setApprovalTimeout changes how long the dialogs that come next wait; the ones already waiting keep their limit.
func (w *Worker) setApprovalTimeout(d time.Duration) {
	w.mu.Lock()
	w.cfg.ApprovalTimeout = d
	w.mu.Unlock()
}
