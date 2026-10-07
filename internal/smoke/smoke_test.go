// Package smoke runs the real hub and agent binaries against the Pub/Sub emulator, a real tmux
// session and a fake Telegram HTTP server. It is skipped when the emulator or tmux is missing.
package smoke

import (
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

type fakeTelegram struct {
	mu      sync.Mutex
	updates []map[string]any
	sent    []string
	nextID  int
}

func (f *fakeTelegram) push(chat, user int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.updates = append(f.updates, map[string]any{"update_id": f.nextID, "message": map[string]any{
		"message_id": f.nextID, "text": text, "from": map[string]any{"id": user}, "chat": map[string]any{"id": chat}}})
}

func (f *fakeTelegram) sentText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.sent, "\n")
}

func (f *fakeTelegram) handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/bot"+token+"/") {
		case "getUpdates":
			offset := int(body["offset"].(float64))
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
			f.mu.Lock()
			f.sent = append(f.sent, body["text"].(string))
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	})
}

func must(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", cmd.Args, err, out)
	}
	return string(out)
}

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

type proc struct {
	cmd *exec.Cmd
	out *bytes.Buffer
	mu  sync.Mutex
}

func start(t *testing.T, bin string, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{out: &bytes.Buffer{}}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.Stdout, p.cmd.Stderr = &lockedWriter{p}, &lockedWriter{p}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.cmd.Process.Kill(); p.cmd.Wait() })
	return p
}

type lockedWriter struct{ p *proc }

func (w *lockedWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.out.Write(b)
}
func (p *proc) output() string { p.mu.Lock(); defer p.mu.Unlock(); return p.out.String() }

func TestRealBinariesEndToEnd(t *testing.T) {
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("PUBSUB_EMULATOR_HOST not set")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, c := range []string{"hub", "agent", "bridgekeys"} {
		must(t, exec.Command("go", "build", "-o", filepath.Join(bin, c), "github.com/n0rm4l-me/petrvs-bridge/cmd/"+c))
	}
	keys := filepath.Join(dir, "keys")
	agents := filepath.Join(dir, "agents")
	os.MkdirAll(agents, 0o700)
	for _, n := range []string{"hub", "laptop"} {
		out := must(t, exec.Command(filepath.Join(bin, "bridgekeys"), "gen", "-name", n, "-out", keys))
		if !strings.Contains(out, "fingerprint") {
			t.Fatal(out)
		}
	}
	must(t, exec.Command("cp", filepath.Join(keys, "laptop.pub.json"), agents))

	const token = "1:smoke-token"
	tg := &fakeTelegram{}
	srv := httptest.NewServer(tg.handler(token))
	t.Cleanup(srv.Close)

	session := fmt.Sprintf("cbsmoke%d", os.Getpid())
	must(t, exec.Command("tmux", "new-session", "-d", "-s", session, "-x", "200", "-y", "50", "cat"))
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })
	pane := func() string {
		out, _ := exec.Command("tmux", "capture-pane", "-p", "-t", session).Output()
		return string(out)
	}

	prefix := fmt.Sprintf("smoke%d", time.Now().UnixNano()%1_000_000)
	state := filepath.Join(dir, "state")
	common := []string{"PB_PROJECT=pb-smoke", "PB_PREFIX=" + prefix, "PB_CREATE_RESOURCES=true", "PB_LOG_LEVEL=debug"}
	hub := start(t, filepath.Join(bin, "hub"), append(common,
		"PB_IDENTITY="+filepath.Join(keys, "hub.identity.json"), "PB_AGENTS_DIR="+agents, "PB_CHAT_ID=1001",
		"TELEGRAM_BOT_TOKEN="+token, "PB_TELEGRAM_API_BASE="+srv.URL, "PB_POLL_TIMEOUT=1s", "PB_OFFLINE_AFTER=1m"))
	agentEnv := append(common, "PB_NAME=laptop", "PB_IDENTITY="+filepath.Join(keys, "laptop.identity.json"), "PB_HUB_PUB="+filepath.Join(keys, "hub.pub.json"),
		"PB_TMUX_SESSION="+session, "PB_STATE_DIR="+state, "PB_ALARM=false", "PB_HEARTBEAT=300ms", "PB_HANDSHAKE_RETRY=500ms",
		"PB_SILENCE_LIMIT=5s", "PB_RECEIPT_TIMEOUT=1s")
	agent := start(t, filepath.Join(bin, "agent"), agentEnv)

	eventually(t, "the hub to report the agent connected\nhub:\n"+hub.output()+"\nagent:\n"+agent.output(), 30*time.Second, func() bool { return strings.Contains(tg.sentText(), "laptop is connected") })

	// phone to tmux
	tg.push(1001, 1001, "hello from the phone")
	eventually(t, "the text in the tmux pane\n"+pane(), 15*time.Second, func() bool { return strings.Contains(pane(), "hello from the phone") })

	// someone else is ignored
	tg.push(999, 999, "intruder text")
	tg.push(1001, 1001, "second line")
	eventually(t, "the second line", 15*time.Second, func() bool { return strings.Contains(pane(), "second line") })
	if strings.Contains(pane(), "intruder") {
		t.Fatal("an unauthorised message reached the terminal")
	}

	// a hook on the laptop queues a reply, the hub shows it
	must(t, withEnv(exec.Command(filepath.Join(bin, "agent"), "send", "the answer from claude"), "PB_STATE_DIR="+state))
	eventually(t, "the reply in the chat", 15*time.Second, func() bool { return strings.Contains(tg.sentText(), "the answer from claude") })

	// a reply fed on standard input
	c := withEnv(exec.Command(filepath.Join(bin, "agent"), "send", "-type", "notice", "-"), "PB_STATE_DIR="+state)
	c.Stdin = strings.NewReader("multi\nline\nnotice\n")
	must(t, c)
	eventually(t, "the notice in the chat", 15*time.Second, func() bool { return strings.Contains(tg.sentText(), "multi\nline\nnotice") })

	// lockdown from the phone, local unlock
	tg.push(1001, 1001, "/lockdown")
	eventually(t, "the lock file", 15*time.Second, func() bool { _, err := os.Stat(filepath.Join(state, "locked")); return err == nil })
	tg.push(1001, 1001, "/unlock")
	tg.push(1001, 1001, "blocked while locked")
	eventually(t, "the refusal", 15*time.Second, func() bool { return strings.Contains(tg.sentText(), "this machine is locked") })
	if strings.Contains(pane(), "blocked while locked") {
		t.Fatal("a locked agent typed text")
	}
	out := must(t, withEnv(exec.Command(filepath.Join(bin, "agent"), "unlock"), "PB_STATE_DIR="+state))
	if !strings.Contains(out, "unlocked") {
		t.Fatal(out)
	}
	tg.push(1001, 1001, "typed after the local unlock")
	eventually(t, "text after unlock", 15*time.Second, func() bool { return strings.Contains(pane(), "typed after the local unlock") })

	// the token and message bodies must not appear in the logs
	for name, p := range map[string]*proc{"hub": hub, "agent": agent} {
		if o := p.output(); strings.Contains(o, "smoke-token") || strings.Contains(o, "hello from the phone") || strings.Contains(o, "the answer from claude") {
			t.Fatalf("%s log leaks a secret or a message body:\n%s", name, o)
		}
	}

	// clean shutdown
	for name, p := range map[string]*proc{"agent": agent, "hub": hub} {
		p.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s did not stop cleanly: %v\n%s", name, err, p.output())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not stop after SIGTERM", name)
		}
	}
}

func withEnv(c *exec.Cmd, env ...string) *exec.Cmd { c.Env = append(os.Environ(), env...); return c }
