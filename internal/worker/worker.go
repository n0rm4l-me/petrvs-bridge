// Package worker runs the assistant without a terminal. Where the tmux executor types into a session and reads the screen, the
// worker starts Claude Code as a child process in its headless mode (`claude -p`, JSON lines in and out) and speaks to it
// directly: a text from the chat becomes a user message, the final answer of a turn becomes a reply, and a request of the
// assistant for permission to use a tool becomes a dialog in the chat with buttons, answered by the key that a button sends.
//
// The Worker is an agentsvc.Executor (it takes the text and the keys) and an agentsvc.Screen (it shows a virtual screen: the
// dialog that waits for an answer, or a short status), so the agent, the hub and the chat need no change: the buttons, the
// hash that binds a button to its dialog, /screen and the stuck-request check all work as they do for tmux.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/promptcmd"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Process is the assistant's process as the worker sees it.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Wait returns when the process has ended; it is called after Stdout has been read to its end.
	Wait() error
	Kill()
	// Stderr is the last of what the process wrote to its error output, for a message to the chat.
	Stderr() string
}

// Launcher starts the assistant with the arguments, in dir.
type Launcher func(ctx context.Context, args []string, dir string) (Process, error)

// Outbox is where the worker puts what the chat should get.
type Outbox interface {
	// Reply is the answer of a finished turn.
	Reply(body string)
	// Notice is anything else: a dialog that waits for an answer (with its buttons), or news about the worker.
	Notice(body string, buttons []wire.Button)
}

// Config is what a Worker needs.
type Config struct {
	// Name is the agent's name, for the texts of the chat.
	Name string
	// Command is the assistant's program; default "claude".
	Command string
	// Workdir is where the assistant works. Required.
	Workdir string
	// Model is passed as --model; empty leaves the assistant's own choice.
	Model string
	// BudgetUSD is the most the assistant's process may spend (--max-budget-usd); default 5, negative for no limit.
	BudgetUSD float64
	// SessionFile, if set, keeps the id of the conversation across restarts of the agent, so that after a restart of the machine the
	// next message resumes the conversation. "/clear" removes it.
	SessionFile string
	// PromptFile holds extra instructions for the assistant (how to work through the chat); it is read each time the process
	// starts, so a change applies to the next conversation without a restart of the agent.
	PromptFile string
	// SettingSources says which settings of Claude Code the process loads; default "project,local". The settings of the user
	// are left out on purpose: their hooks (the Stop hook of the bridge, for one) would send each answer to the chat twice.
	SettingSources string
	// Policy decides which requests for a tool go to the chat; default DefaultPolicy.
	Policy *Policy
	// TurnTimeout is how long one request may run before it is interrupted; default 30 minutes, negative for no limit.
	TurnTimeout time.Duration
	// KillGrace is how long an interrupted turn has to end before the process is killed; default 30 seconds.
	KillGrace time.Duration
	// ApprovalTimeout is how long a dialog waits for the chat before the request is refused; default 30 minutes.
	ApprovalTimeout time.Duration
	// MaxQueued bounds the messages that wait for the turn that is running; default 16.
	MaxQueued int
	// Settings is JSON passed to the assistant as --settings, on top of the settings it loads. The default switches the hooks off: the
	// hooks of a project (the Stop hook of this bridge, for one) would send every answer to the chat a second time, since the worker
	// sends it itself. "none" passes nothing.
	Settings string
	// DisallowedTools are the tools taken away from the assistant (--disallowedTools). Nil means DefaultInteractiveTools, the ones
	// that need a person at a terminal; an empty list takes nothing away.
	DisallowedTools []string

	Out    Outbox
	Launch Launcher
	Log    *slog.Logger
	Now    func() time.Time
}

// Worker is the assistant without a terminal. It is safe for concurrent use.
type Worker struct {
	cfg  Config
	ctx  context.Context
	stop context.CancelFunc

	mu        sync.Mutex
	pr        *proc
	session   string
	running   bool
	turn      int
	turnText  string
	turnStart time.Time
	turnTimer *time.Timer
	killTimer *time.Timer
	pending   *approval
	queue     []string
	costBase  float64
	procCost  float64
	seq       int
	ctlSeq    int
	closed    bool
}

type approval struct {
	id    string // the request id of the assistant, which the answer must carry
	pr    *proc
	tool  string
	input json.RawMessage
	seq   int
	text  string // the dialog
	limit time.Duration
	timer *time.Timer
}

type proc struct {
	p     Process
	stdin io.WriteCloser
	wmu   sync.Mutex
}

func (pr *proc) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	pr.wmu.Lock()
	defer pr.wmu.Unlock()
	_, err = pr.stdin.Write(append(b, '\n'))
	return err
}

// New checks the settings and returns a Worker. The assistant is not started until the first message.
func New(cfg Config) (*Worker, error) {
	if cfg.Workdir == "" {
		return nil, errors.New("worker: the working directory is required")
	}
	if cfg.Out == nil {
		return nil, errors.New("worker: no outbox")
	}
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	if cfg.Name == "" {
		cfg.Name = "Claude Code"
	}
	if cfg.BudgetUSD == 0 {
		cfg.BudgetUSD = 5
	}
	if cfg.SettingSources == "" {
		cfg.SettingSources = "project,local"
	}
	if cfg.Policy == nil {
		cfg.Policy = DefaultPolicy()
	}
	if cfg.TurnTimeout == 0 {
		cfg.TurnTimeout = 30 * time.Minute
	}
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 30 * time.Second
	}
	if cfg.ApprovalTimeout == 0 {
		cfg.ApprovalTimeout = 30 * time.Minute
	}
	if cfg.MaxQueued == 0 {
		cfg.MaxQueued = 16
	}
	if cfg.DisallowedTools == nil {
		cfg.DisallowedTools = DefaultInteractiveTools
	}
	if cfg.Settings == "" {
		cfg.Settings = `{"disableAllHooks":true}`
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, stop := context.WithCancel(context.Background())
	if cfg.Launch == nil {
		cfg.Launch = ExecLauncher(cfg.Command)
	}
	w := &Worker{cfg: cfg, ctx: ctx, stop: stop}
	if cfg.SessionFile != "" {
		if b, err := os.ReadFile(cfg.SessionFile); err == nil {
			if id := strings.TrimSpace(string(b)); validSession(id) {
				w.session = id
			}
		}
	}
	return w, nil
}

// validSession says whether text is fit to be an id of a conversation: it ends up on a command line.
func validSession(id string) bool {
	if id == "" || len(id) > 80 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// rememberSessionLocked keeps the id of the conversation in memory and, if there is a file for it, on disk.
func (w *Worker) rememberSessionLocked(id string) {
	if id == w.session || !validSession(id) {
		return
	}
	w.session = id
	if w.cfg.SessionFile == "" {
		return
	}
	if err := os.WriteFile(w.cfg.SessionFile, []byte(id+"\n"), 0o600); err != nil {
		w.cfg.Log.Warn("cannot keep the id of the conversation", "err", err)
	}
}

// Close ends the assistant's process. The messages that wait are dropped.
func (w *Worker) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.stopTimersLocked()
	if w.pending != nil && w.pending.timer != nil {
		w.pending.timer.Stop()
	}
	w.pending, w.queue = nil, nil
	if w.pr != nil {
		w.pr.p.Kill()
	}
	w.stop()
}

// ---- Executor ----

// SendText gives the assistant a message from the chat. While a turn runs it waits in a queue; while a dialog waits for an
// answer it is refused, as the tmux executor's agent refuses to type into a prompt. "/clear" starts a new conversation.
func (w *Worker) SendText(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("worker: closed")
	}
	if w.pending != nil {
		return errors.New("worker: a request for permission waits for an answer")
	}
	if text == "/clear" {
		w.clearLocked()
		return nil
	}
	if w.running {
		if len(w.queue) >= w.cfg.MaxQueued {
			return fmt.Errorf("worker: %d messages already wait for the running request", len(w.queue))
		}
		w.queue = append(w.queue, text)
		return nil
	}
	return w.startTurnLocked(text)
}

// SendKey answers the dialog that waits (1 allows, 2 or Escape refuses); Escape with no dialog interrupts the running turn.
// Every other key does nothing: there is no terminal to move in.
func (w *Worker) SendKey(ctx context.Context, key string) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("worker: closed")
	}
	if w.pending != nil {
		a := w.pending
		switch key {
		case "1":
			w.pending = nil
			a.timer.Stop()
			w.mu.Unlock()
			return a.pr.respond(a.id, a.input, true, "")
		case "2", "Escape":
			w.pending = nil
			a.timer.Stop()
			w.mu.Unlock()
			return a.pr.respond(a.id, nil, false, "The user refused this request in the chat. Do not try another way to do the same thing; say what you could not do.")
		}
		w.mu.Unlock()
		return nil
	}
	if key == "Escape" && w.running && w.pr != nil {
		pr := w.pr
		w.mu.Unlock()
		return pr.interrupt(w.nextControlID())
	}
	w.mu.Unlock()
	return nil
}

func (w *Worker) nextControlID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ctlSeq++
	return "pb-" + strconv.Itoa(w.ctlSeq)
}

// ---- Screen ----

// Capture is the virtual screen: the dialog that waits for an answer, else a status. It never fails.
func (w *Worker) Capture(ctx context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending != nil {
		return w.pending.text, nil
	}
	state := "idle"
	if w.running {
		state = "working"
	}
	return idleScreen(w.cfg.Name, state, w.cfg.Now().Sub(w.turnStart), w.turnText, len(w.queue), w.costBase+w.procCost, w.session), nil
}

// PaneCommand says that the assistant is in front, as it is for the agent's checks that look for a shell.
func (w *Worker) PaneCommand(ctx context.Context) (string, error) { return "claude", nil }

// Start has nothing to do: the process starts with the first message, and again after it ends.
func (w *Worker) Start(ctx context.Context, workdir, command string) error { return nil }

// Facts are what the worker adds to the heartbeat.
func (w *Worker) Facts() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	state := "idle"
	switch {
	case w.pending != nil:
		state = "waiting for permission"
	case w.running:
		state = "working"
	}
	return map[string]string{"worker": state, "worker_cost_usd": fmt.Sprintf("%.4f", w.costBase+w.procCost), "worker_queued": strconv.Itoa(len(w.queue))}
}

// ---- turns ----

func (w *Worker) args() ([]string, error) {
	a := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "manual", "--permission-prompts", "host", "--permission-prompt-tool", "stdio",
		"--setting-sources", w.cfg.SettingSources}
	if w.cfg.Settings != "none" {
		a = append(a, "--settings", w.cfg.Settings)
	}
	if len(w.cfg.DisallowedTools) > 0 {
		a = append(a, "--disallowedTools", strings.Join(w.cfg.DisallowedTools, ","))
	}
	if w.cfg.Model != "" {
		a = append(a, "--model", w.cfg.Model)
	}
	if w.cfg.BudgetUSD > 0 {
		a = append(a, "--max-budget-usd", strconv.FormatFloat(w.cfg.BudgetUSD, 'f', -1, 64))
	}
	if w.cfg.PromptFile != "" {
		b, err := os.ReadFile(w.cfg.PromptFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read the worker prompt file: %w", err)
		}
		if p := strings.TrimSpace(string(b)); p != "" {
			a = append(a, "--append-system-prompt", p)
		}
	}
	if w.session != "" {
		a = append(a, "--resume", w.session)
	}
	return a, nil
}

func (w *Worker) ensureProcessLocked() error {
	if w.pr != nil {
		return nil
	}
	args, err := w.args()
	if err != nil {
		return err
	}
	p, err := w.cfg.Launch(w.ctx, args, w.cfg.Workdir)
	if err != nil {
		return fmt.Errorf("cannot start the assistant: %w", err)
	}
	pr := &proc{p: p, stdin: p.Stdin()}
	w.pr = pr
	w.cfg.Log.Info("assistant process started", "resume", w.session != "")
	go w.readLoop(pr, p.Stdout())
	return nil
}

func (w *Worker) startTurnLocked(text string) error {
	if err := w.ensureProcessLocked(); err != nil {
		return err
	}
	pr := w.pr
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
	if err := pr.send(msg); err != nil {
		w.dropProcessLocked(pr)
		return fmt.Errorf("cannot give the assistant the message: %w", err)
	}
	w.running = true
	w.turn++
	w.turnText = text
	w.turnStart = w.cfg.Now()
	w.stopTimersLocked()
	if w.cfg.TurnTimeout > 0 {
		id := w.turn
		w.turnTimer = time.AfterFunc(w.cfg.TurnTimeout, func() { w.turnExpired(id) })
	}
	return nil
}

func (w *Worker) stopTimersLocked() {
	if w.turnTimer != nil {
		w.turnTimer.Stop()
		w.turnTimer = nil
	}
	if w.killTimer != nil {
		w.killTimer.Stop()
		w.killTimer = nil
	}
}

// dropProcessLocked forgets a process that cannot be written to and ends it.
func (w *Worker) dropProcessLocked(pr *proc) {
	if w.pr == pr {
		w.pr = nil
	}
	pr.p.Kill()
}

func (w *Worker) clearLocked() {
	w.stopTimersLocked()
	w.queue = nil
	w.running = false
	w.session = ""
	if w.cfg.SessionFile != "" {
		_ = os.Remove(w.cfg.SessionFile)
	}
	if w.pr != nil {
		pr := w.pr
		w.pr = nil
		pr.p.Kill()
	}
	w.costBase += w.procCost
	w.procCost = 0
	go w.cfg.Out.Reply("🧹 A new conversation starts with your next message.")
}

func (w *Worker) turnExpired(id int) {
	w.mu.Lock()
	if w.closed || !w.running || w.turn != id || w.pr == nil {
		w.mu.Unlock()
		return
	}
	pr := w.pr
	ctl := w.nextControlIDLocked()
	grace := w.cfg.KillGrace
	w.killTimer = time.AfterFunc(grace, func() { w.turnKill(id) })
	limit := w.cfg.TurnTimeout
	w.mu.Unlock()
	w.cfg.Log.Warn("a turn ran too long; interrupting it", "limit", limit)
	_ = pr.interrupt(ctl)
	w.cfg.Out.Notice(fmt.Sprintf("⏱ The request ran for more than %s, so it was interrupted.", limit.Round(time.Second)), nil)
}

func (w *Worker) turnKill(id int) {
	w.mu.Lock()
	if w.closed || !w.running || w.turn != id || w.pr == nil {
		w.mu.Unlock()
		return
	}
	pr := w.pr
	w.mu.Unlock()
	w.cfg.Log.Warn("an interrupted turn did not end; killing the assistant")
	pr.p.Kill()
}

func (w *Worker) nextControlIDLocked() string {
	w.ctlSeq++
	return "pb-" + strconv.Itoa(w.ctlSeq)
}

// ---- the assistant's output ----

type message struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	Cost      float64         `json:"total_cost_usd"`
}

type toolRequest struct {
	Subtype     string          `json:"subtype"`
	ToolName    string          `json:"tool_name"`
	Input       json.RawMessage `json:"input"`
	Description string          `json:"description"`
}

func (w *Worker) readLoop(pr *proc, r io.Reader) {
	dec := json.NewDecoder(r)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			if !errors.Is(err, io.EOF) {
				w.cfg.Log.Warn("the assistant wrote something that is not JSON; ending it", "err", err)
				pr.p.Kill()
			}
			break
		}
		w.handle(pr, &m)
	}
	w.onExit(pr, pr.p.Wait())
}

func (w *Worker) handle(pr *proc, m *message) {
	switch m.Type {
	case "system":
		if m.Subtype == "init" && m.SessionID != "" {
			w.mu.Lock()
			if w.pr == pr {
				w.rememberSessionLocked(m.SessionID)
			}
			w.mu.Unlock()
		}
	case "control_request":
		var req toolRequest
		if err := json.Unmarshal(m.Request, &req); err != nil || req.Subtype != "can_use_tool" {
			_ = pr.send(controlError(m.RequestID, "the worker does not handle this request"))
			return
		}
		w.onToolRequest(pr, m.RequestID, &req)
	case "result":
		w.onResult(pr, m)
	}
}

func (w *Worker) onToolRequest(pr *proc, id string, req *toolRequest) {
	var input map[string]any
	_ = json.Unmarshal(req.Input, &input)
	d := w.cfg.Policy.Decide(req.ToolName, input)
	switch d.Action {
	case Allow:
		w.cfg.Log.Info("tool allowed by the policy", "tool", req.ToolName)
		_ = pr.respond(id, req.Input, true, "")
		return
	case Deny:
		w.cfg.Log.Warn("tool denied by the policy", "tool", req.ToolName, "reason", d.Reason)
		told := "Refused by the worker's policy: " + d.Reason + ". Do not try another way to do the same thing."
		if d.Advice != "" {
			told = "Refused: " + d.Reason + ". " + d.Advice
		} else {
			w.cfg.Out.Notice("🚫 **"+w.cfg.Name+"** was refused a "+req.ToolName+" request by the policy: "+d.Reason+".", nil)
		}
		_ = pr.respond(id, nil, false, told)
		return
	}

	w.mu.Lock()
	if w.closed || w.pr != pr {
		w.mu.Unlock()
		return
	}
	w.seq++
	seq := w.seq
	text := renderDialog(req.ToolName, seq, input, req.Description, d.Risk)
	prompt, ok := screen.ExtractPrompt(text)
	if !ok { // cannot happen with a dialog made here; refuse rather than approve what cannot be shown
		w.mu.Unlock()
		_ = pr.respond(id, nil, false, "The worker could not show this request to the user.")
		return
	}
	a := &approval{id: id, pr: pr, tool: req.ToolName, input: req.Input, seq: seq, text: text, limit: w.cfg.ApprovalTimeout}
	a.timer = time.AfterFunc(a.limit, func() { w.approvalExpired(seq) })
	w.pending = a
	w.mu.Unlock()

	n := promptcmd.ForPrompt(w.cfg.Name+" needs your permission to use "+req.ToolName, prompt)
	w.cfg.Out.Notice(n.Body, n.Buttons)
}

func (w *Worker) approvalExpired(seq int) {
	w.mu.Lock()
	a := w.pending
	if w.closed || a == nil || a.seq != seq {
		w.mu.Unlock()
		return
	}
	w.pending = nil
	w.mu.Unlock()
	_ = a.pr.respond(a.id, nil, false, "Nobody answered in the chat in time, so the request was refused.")
	w.cfg.Out.Notice("⌛ The request for permission to use "+a.tool+" got no answer in "+a.limit.Round(time.Millisecond).String()+" and was refused.", nil)
}

func (w *Worker) onResult(pr *proc, m *message) {
	w.mu.Lock()
	if w.pr != pr {
		w.mu.Unlock()
		return
	}
	w.running = false
	w.stopTimersLocked()
	if m.Cost > 0 {
		w.procCost = m.Cost
	}
	reply := w.replyText(m)
	var next string
	if len(w.queue) > 0 {
		next, w.queue = w.queue[0], w.queue[1:]
	}
	var startErr error
	if next != "" {
		startErr = w.startTurnLocked(next)
	}
	w.mu.Unlock()

	w.cfg.Out.Reply(reply)
	if startErr != nil {
		w.cfg.Out.Notice("⚠️ The next waiting message could not be given to the assistant: "+startErr.Error(), nil)
	}
}

// maxReply bounds an answer: the wire allows 256 KiB of a message once it is escaped for storage.
const maxReply = 100_000

func (w *Worker) replyText(m *message) string {
	text := strings.TrimSpace(m.Result)
	switch {
	case m.Subtype == "error_max_budget_usd":
		text = "⚠️ The budget of this conversation ($" + strconv.FormatFloat(w.cfg.BudgetUSD, 'f', -1, 64) + ") is spent, so the assistant stopped. Send /clear to start a new conversation, or raise PB_WORKER_BUDGET_USD." + suffix(text)
	case m.Subtype == "error_max_turns":
		text = "⚠️ The assistant reached its limit of steps for this request and stopped." + suffix(text)
	case m.IsError || strings.HasPrefix(m.Subtype, "error"):
		kind := m.Subtype
		if kind == "" {
			kind = "error"
		}
		text = "⚠️ The request ended with an error (" + kind + ")." + suffix(text)
	case text == "":
		text = "(the assistant finished without a text answer)"
	}
	if len(text) > maxReply {
		cut := maxReply
		for cut > 0 && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = text[:cut] + "\n\n(cut: the answer was longer than the chat takes)"
	}
	return text
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return "\n\n" + s
}

func (w *Worker) onExit(pr *proc, err error) {
	w.mu.Lock()
	if w.pr != pr {
		w.mu.Unlock()
		return
	}
	w.pr = nil
	w.stopTimersLocked()
	wasRunning, pending, queued, closed := w.running, w.pending, len(w.queue), w.closed
	if pending != nil && pending.timer != nil {
		pending.timer.Stop()
	}
	w.running, w.pending, w.queue = false, nil, nil
	w.costBase += w.procCost
	w.procCost = 0
	w.mu.Unlock()
	if closed {
		return
	}
	tail := strings.TrimSpace(pr.p.Stderr())
	w.cfg.Log.Warn("assistant process ended", "err", err, "stderr", tail, "mid_request", wasRunning)
	if !wasRunning && pending == nil {
		return // it ended between requests; the next message starts it again
	}
	msg := "⚠️ The assistant's process ended in the middle of a request"
	if err != nil {
		msg += " (" + err.Error() + ")"
	}
	msg += ". Send the message again: the conversation is resumed."
	if queued > 0 {
		msg += fmt.Sprintf(" %d message(s) that waited were dropped.", queued)
	}
	if tail != "" {
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		msg += "\n\n```\n" + tail + "\n```"
	}
	w.cfg.Out.Notice(msg, nil)
}

// ---- messages to the assistant ----

func (pr *proc) respond(id string, input json.RawMessage, allow bool, why string) error {
	var body any
	if allow {
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		body = map[string]any{"behavior": "allow", "updatedInput": input}
	} else {
		body = map[string]any{"behavior": "deny", "message": why}
	}
	return pr.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": body,
	}})
}

func (pr *proc) interrupt(id string) error {
	return pr.send(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "interrupt"}})
}

func controlError(id, why string) map[string]any {
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": why}}
}
