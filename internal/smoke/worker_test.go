package smoke

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests run the real hub and the real agent, the agent in worker mode, on this machine: the bus is the fake server of the
// Pub/Sub library run as a program (pubsubfake, a module of its own: no emulator and no Java is needed), the chat is a fake Telegram HTTP server, and the
// assistant is either a script that speaks the protocol of `claude -p`, or, with PB_TEST_REAL_CLAUDE set, the real claude.
// Nothing leaves the machine: the child processes get a clean environment with no Google credentials, and the bus and the chat
// are on localhost.

type tgButton struct{ Text, Data string }

type tgSent struct {
	ID      int
	Text    string
	Buttons [][]tgButton
}

type fakeChat struct {
	mu          sync.Mutex
	updates     []map[string]any
	sent        []tgSent
	nextUpdate  int
	nextMessage int
	answered    []string
}

func (f *fakeChat) say(chat, user int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUpdate++
	f.nextMessage++
	f.updates = append(f.updates, map[string]any{"update_id": f.nextUpdate, "message": map[string]any{
		"message_id": 100000 + f.nextMessage, "date": time.Now().Unix(), "text": text,
		"from": map[string]any{"id": user}, "chat": map[string]any{"id": chat}}})
}

func (f *fakeChat) tap(chat, user int64, messageID int, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUpdate++
	f.updates = append(f.updates, map[string]any{"update_id": f.nextUpdate, "callback_query": map[string]any{
		"id": fmt.Sprintf("cb%d", f.nextUpdate), "data": data, "from": map[string]any{"id": user},
		"message": map[string]any{"message_id": messageID, "date": time.Now().Unix(), "chat": map[string]any{"id": chat}}}})
}

func (f *fakeChat) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []string
	for _, s := range f.sent {
		parts = append(parts, s.Text)
	}
	return strings.Join(parts, "\n")
}

// withButtons returns the first message that has buttons and holds sub.
func (f *fakeChat) withButtons(sub string) (tgSent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		if len(s.Buttons) > 0 && strings.Contains(s.Text, sub) {
			return s, true
		}
	}
	return tgSent{}, false
}

func (m tgSent) button(label string) string {
	for _, row := range m.Buttons {
		for _, b := range row {
			if strings.HasPrefix(b.Text, label) {
				return b.Data
			}
		}
	}
	return ""
}

func (f *fakeChat) handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/bot"+token+"/") {
		case "getUpdates":
			offset := 0
			if v, ok := body["offset"].(float64); ok {
				offset = int(v)
			}
			deadline := time.Now().Add(700 * time.Millisecond)
			for time.Now().Before(deadline) {
				f.mu.Lock()
				var out []map[string]any
				for _, u := range f.updates {
					if u["update_id"].(int) >= offset {
						out = append(out, u)
					}
				}
				f.mu.Unlock()
				if len(out) > 0 {
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
					return
				}
				time.Sleep(30 * time.Millisecond)
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
		case "sendMessage":
			text, _ := body["text"].(string)
			var rows [][]tgButton
			if rm, ok := body["reply_markup"].(map[string]any); ok {
				rs, _ := rm["inline_keyboard"].([]any)
				for _, row := range rs {
					var out []tgButton
					cells, _ := row.([]any)
					for _, c := range cells {
						cm, _ := c.(map[string]any)
						t, _ := cm["text"].(string)
						d, _ := cm["callback_data"].(string)
						out = append(out, tgButton{t, d})
					}
					rows = append(rows, out)
				}
			}
			f.mu.Lock()
			f.nextMessage++
			id := 200000 + f.nextMessage
			f.sent = append(f.sent, tgSent{ID: id, Text: text, Buttons: rows})
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": id, "date": time.Now().Unix(), "chat": map[string]any{"id": 1001}}})
		case "answerCallbackQuery":
			text, _ := body["text"].(string)
			f.mu.Lock()
			f.answered = append(f.answered, text)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		default: // setMyCommands, editMessageReplyMarkup, setMessageReaction, ...
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	})
}

// cleanEnv is the environment of the programs that are started: this process's, without anything that could point them at the
// real bridge, the real chat or the real cloud.
func cleanEnv(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "PB_"), strings.HasPrefix(name, "TELEGRAM_"), strings.HasPrefix(name, "SLACK_"),
			strings.HasPrefix(name, "GOOGLE_"), strings.HasPrefix(name, "CLOUDSDK_"), name == "PUBSUB_EMULATOR_HOST", name == "TMUX", name == "TMUX_PANE":
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

func startClean(t *testing.T, bin string, env []string) *proc {
	t.Helper()
	p := &proc{out: &bytes.Buffer{}}
	p.cmd = exec.Command(bin)
	p.cmd.Env = cleanEnv(env...)
	p.cmd.Stdout, p.cmd.Stderr = &lockedWriter{p}, &lockedWriter{p}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.cmd.Process.Signal(syscall.SIGTERM) // lets the agent end its assistant
		done := make(chan struct{})
		go func() { p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			p.cmd.Process.Kill()
			<-done
		}
	})
	return p
}

// startFakePubSub builds and starts the fake Pub/Sub server and returns its address. It skips the test when the server cannot be
// built (the module needs the network the first time).
func startFakePubSub(t *testing.T, bin string) string {
	t.Helper()
	exe := filepath.Join(bin, "pubsubfake")
	build := exec.Command("go", "build", "-o", exe, ".")
	build.Dir = "pubsubfake"
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build the fake Pub/Sub server (is there a network for its modules?): %v\n%s", err, out)
	}
	cmd := exec.Command(exe)
	cmd.Env = cleanEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Signal(syscall.SIGTERM); cmd.Wait() })
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case addr := <-line:
		if addr == "" {
			t.Fatal("the fake Pub/Sub server printed no address")
		}
		return addr
	case <-time.After(20 * time.Second):
		t.Fatal("the fake Pub/Sub server did not start")
		return ""
	}
}

type workerStack struct {
	chat    *fakeChat
	state   string
	workdir string
	hub     *proc
	agent   *proc
}

func startWorkerStack(t *testing.T, agentEnv ...string) *workerStack {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	busAddr := startFakePubSub(t, bin)
	for _, c := range []string{"hub", "agent", "bridgekeys"} {
		must(t, exec.Command("go", "build", "-o", filepath.Join(bin, c), "github.com/n0rm4l-me/petrvs-bridge/cmd/"+c))
	}
	keys := filepath.Join(dir, "keys")
	agents := filepath.Join(dir, "agents")
	os.MkdirAll(agents, 0o700)
	for _, n := range []string{"hub", "laptop"} {
		must(t, exec.Command(filepath.Join(bin, "bridgekeys"), "gen", "-name", n, "-out", keys))
	}
	must(t, exec.Command("cp", filepath.Join(keys, "laptop.pub.json"), agents))

	const token = "1:worker-smoke-token"
	chat := &fakeChat{}
	tg := httptest.NewServer(chat.handler(token))
	t.Cleanup(tg.Close)

	st := &workerStack{chat: chat, state: filepath.Join(dir, "state"), workdir: filepath.Join(dir, "work")}
	os.MkdirAll(st.workdir, 0o700)
	common := []string{"PUBSUB_EMULATOR_HOST=" + busAddr, "PB_PROJECT=pb-smoke", fmt.Sprintf("PB_PREFIX=w%d", time.Now().UnixNano()%1_000_000),
		"PB_CREATE_RESOURCES=true", "PB_LOG_LEVEL=debug"}
	st.hub = startClean(t, filepath.Join(bin, "hub"), append(append([]string{}, common...),
		"PB_IDENTITY="+filepath.Join(keys, "hub.identity.json"), "PB_AGENTS_DIR="+agents, "PB_CHAT_ID=1001", "TELEGRAM_BOT_TOKEN="+token,
		"PB_TELEGRAM_API_BASE="+tg.URL, "PB_POLL_TIMEOUT=1s", "PB_OFFLINE_AFTER=1m"))
	env := append(append([]string{}, common...), "PB_NAME=laptop", "PB_IDENTITY="+filepath.Join(keys, "laptop.identity.json"),
		"PB_HUB_PUB="+filepath.Join(keys, "hub.pub.json"), "PB_STATE_DIR="+st.state, "PB_ALARM=false", "PB_HEARTBEAT=300ms",
		"PB_HANDSHAKE_RETRY=500ms", "PB_SILENCE_LIMIT=5s", "PB_RECEIPT_TIMEOUT=1s", "PB_WORKER=true", "PB_WORKDIR="+st.workdir)
	st.agent = startClean(t, filepath.Join(bin, "agent"), append(env, agentEnv...))

	eventually(t, "the hub to report the agent connected\nhub:\n"+st.hub.output()+"\nagent:\n"+st.agent.output(), 40*time.Second,
		func() bool { return strings.Contains(chat.text(), "laptop is connected") })
	return st
}

// fakeClaude speaks enough of the protocol of `claude -p --input-format stream-json --output-format stream-json`: it says hello,
// answers each user message with the text it was given, and for a message that holds "needs-approval" asks for permission to
// use Bash and reports the decision it was given.
const fakeClaude = `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"fake-sess"}'
while IFS= read -r line; do
  [ "$(printf '%s' "$line" | jq -r .type)" = user ] || continue
  text=$(printf '%s' "$line" | jq -r .message.content)
  case "$text" in
    *needs-approval*)
      echo '{"type":"control_request","request_id":"fake-req","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"touch approved.txt"},"description":"Create approved.txt"}}'
      IFS= read -r answer
      behavior=$(printf '%s' "$answer" | jq -r .response.response.behavior)
      jq -cn --arg t "decision: $behavior for: $text" '{type:"result",subtype:"success",is_error:false,result:$t,total_cost_usd:0.001}'
      ;;
    *)
      jq -cn --arg t "echo: $text" '{type:"result",subtype:"success",is_error:false,result:$t,total_cost_usd:0.001}'
      ;;
  esac
done
`

func TestWorkerAgentAnswersApprovesAndRefusesThroughTheRealBinaries(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed (the scripted assistant needs it)")
	}
	script := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(script, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	st := startWorkerStack(t, "PB_WORKER_COMMAND="+script)
	dump := func() string {
		return "\nchat:\n" + st.chat.text() + "\nhub:\n" + st.hub.output() + "\nagent:\n" + st.agent.output()
	}

	// A message from the phone reaches the assistant, and its answer comes back.
	st.chat.say(1001, 1001, "hello worker")
	eventually(t, "the answer in the chat"+dump(), 20*time.Second, func() bool { return strings.Contains(st.chat.text(), "echo: hello worker") })

	// Someone else is ignored.
	st.chat.say(999, 999, "intruder text")
	st.chat.say(1001, 1001, "second message")
	eventually(t, "the second answer"+dump(), 20*time.Second, func() bool { return strings.Contains(st.chat.text(), "echo: second message") })
	if strings.Contains(st.chat.text(), "intruder") {
		t.Fatal("a message of an unauthorised user reached the assistant")
	}

	// The conversation id is kept in the state directory.
	eventually(t, "the session file", 10*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(st.state, "worker-session"))
		return err == nil && strings.TrimSpace(string(b)) == "fake-sess"
	})

	// A request for permission comes as buttons; Yes lets it through.
	st.chat.say(1001, 1001, "first needs-approval")
	var dialog tgSent
	eventually(t, "the dialog"+dump(), 20*time.Second, func() bool {
		var ok bool
		dialog, ok = st.chat.withButtons("needs your permission to use Bash")
		return ok
	})
	if !strings.Contains(dialog.Text, "touch approved.txt") {
		t.Fatalf("the dialog does not show the command: %q", dialog.Text)
	}
	yes := dialog.button("1. Yes")
	if !strings.HasPrefix(yes, "k:laptop:1:") {
		t.Fatalf("buttons: %+v", dialog.Buttons)
	}
	st.chat.tap(1001, 1001, dialog.ID, yes)
	eventually(t, "the decision to be allow"+dump(), 20*time.Second, func() bool { return strings.Contains(st.chat.text(), "decision: allow for: first needs-approval") })

	// The next request is refused with Esc.
	st.chat.say(1001, 1001, "second needs-approval")
	var dialog2 tgSent
	eventually(t, "the second dialog"+dump(), 20*time.Second, func() bool {
		st.chat.mu.Lock()
		defer st.chat.mu.Unlock()
		n := 0
		for _, s := range st.chat.sent {
			if len(s.Buttons) > 0 && strings.Contains(s.Text, "needs your permission to use Bash") {
				n++
				dialog2 = s
			}
		}
		return n == 2
	})
	st.chat.tap(1001, 1001, dialog2.ID, dialog2.button("✖ Esc"))
	eventually(t, "the decision to be deny"+dump(), 20*time.Second, func() bool { return strings.Contains(st.chat.text(), "decision: deny for: second needs-approval") })

	// The agent says that it runs as a worker, and the answers are not in the logs of the hub or the agent.
	if !strings.Contains(st.agent.output(), `"mode":"worker"`) {
		t.Fatalf("the agent did not start in worker mode:\n%s", st.agent.output())
	}
	for name, p := range map[string]*proc{"hub": st.hub, "agent": st.agent} {
		if strings.Contains(p.output(), "worker-smoke-token") {
			t.Errorf("the bot token is in the log of the %s", name)
		}
		if strings.Contains(p.output(), "hello worker") {
			t.Errorf("the text of a message is in the log of the %s", name)
		}
	}
}

func TestWorkerAgentWithTheRealClaudeThroughTheRealBinaries(t *testing.T) {
	if os.Getenv("PB_TEST_REAL_CLAUDE") == "" {
		t.Skip("set PB_TEST_REAL_CLAUDE=1 to run the test that starts the real claude")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	st := startWorkerStack(t, "PB_WORKER_MODEL=haiku", "PB_WORKER_BUDGET_USD=0.25", "PB_WORKER_TURN_TIMEOUT=2m", "PB_WORKER_APPROVAL_TIMEOUT=2m")
	dump := func() string { return "\nchat:\n" + st.chat.text() + "\nagent:\n" + st.agent.output() }

	st.chat.say(1001, 1001, "Reply with exactly the single word: pong")
	eventually(t, "pong"+dump(), 90*time.Second, func() bool { return strings.Contains(strings.ToLower(st.chat.text()), "pong") })

	st.chat.say(1001, 1001, "Run exactly this shell command and nothing else: printf hi > real.txt . Then reply with the single word finished.")
	var dialog tgSent
	eventually(t, "the dialog"+dump(), 90*time.Second, func() bool {
		var ok bool
		dialog, ok = st.chat.withButtons("needs your permission to use Bash")
		return ok
	})
	st.chat.tap(1001, 1001, dialog.ID, dialog.button("1. Yes"))
	eventually(t, "finished"+dump(), 90*time.Second, func() bool { return strings.Contains(strings.ToLower(st.chat.text()), "finished") })
	if b, err := os.ReadFile(filepath.Join(st.workdir, "real.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("the approved command did not write its file: %q %v", b, err)
	}
}
