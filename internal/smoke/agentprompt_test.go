package smoke

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
)

// promptFakeTmux answers capture-pane with a canned screen and records how it was asked.
const promptFakeTmux = `#!/bin/sh
echo "$*" > "$FAKE_TMUX_ARGS"
[ "$1" = capture-pane ] || exit 1
cat "$FAKE_TMUX_SCREEN"
`

func queuedItems(t *testing.T, dir string) []spool.Item {
	t.Helper()
	var out []spool.Item
	for _, raw := range spooled(t, dir) {
		var it spool.Item
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			t.Fatal(err)
		}
		out = append(out, it)
	}
	return out
}

// `agent prompt` runs in Claude Code's Notification hook, like `agent send`: it needs the state directory and tmux on
// PATH, and nothing of the settings that only `agent run` needs.
func TestPromptNeedsNoMoreThanTheStateDirectoryAndTmux(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent")
	must(t, exec.Command("go", "build", "-o", bin, "github.com/n0rm4l-me/petrvs-bridge/cmd/agent"))

	tmuxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmuxDir, "tmux"), []byte(promptFakeTmux), 0o755); err != nil {
		t.Fatal(err)
	}
	screenFile := filepath.Join(t.TempDir(), "screen.txt")
	if err := os.WriteFile(screenFile, []byte(screentest.Bash(1, "ls -la /tmp")), 0o644); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "tmux.args")
	withTmux := []string{"PATH=" + tmuxDir + ":/usr/bin:/bin", "TMUX_PANE=%3", "FAKE_TMUX_SCREEN=" + screenFile, "FAKE_TMUX_ARGS=" + argsFile}
	const permission = `{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`

	t.Run("the prompt on the screen with a button for each choice", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state")
		_, errs, code := runEmpty(t, bin, permission, append([]string{"PB_STATE_DIR=" + state}, withTmux...), "prompt")
		got := queuedItems(t, filepath.Join(state, "spool"))
		if code != 0 || len(got) != 1 {
			t.Fatalf("exit %d, spooled %+v, stderr %q", code, got, errs)
		}
		// the hash is keyed with the key the hook made in the state directory
		key, err := screen.LoadKey(state)
		if err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(filepath.Join(state, "prompt-key")); err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
			t.Errorf("the key file: %v %v", info, err)
		}
		pr, _ := screen.ExtractPrompt(screentest.Bash(1, "ls -la /tmp"))
		screen.SetHashKey(key)
		t.Cleanup(func() { screen.SetHashKey(nil) })
		keyed, _ := screen.ExtractPrompt(screentest.Bash(1, "ls -la /tmp"))
		if keyed.Hash == pr.Hash {
			t.Fatal("the hash does not depend on the key")
		}
		pr = keyed
		it := got[0]
		if it.Type != "notice" || !strings.HasPrefix(it.Body, "⏳ **Claude needs your permission to use Bash**\n```\n Bash command\n") || !strings.Contains(it.Body, "│ ls -la /tmp") {
			t.Errorf("%+v", it)
		}
		if len(it.Buttons) != 4 || it.Buttons[0].Key != "1" || it.Buttons[3].Key != "Escape" || it.Buttons[0].Prompt != pr.Hash {
			t.Errorf("buttons %+v, want the hash %s", it.Buttons, pr.Hash)
		}
		if args, _ := os.ReadFile(argsFile); strings.TrimSpace(string(args)) != "capture-pane -p -t %3" {
			t.Errorf("tmux was asked %q", args)
		}
	})

	t.Run("the default state directory and no settings", func(t *testing.T) {
		home := t.TempDir()
		_, errs, code := runEmpty(t, bin, permission, append([]string{"HOME=" + home}, withTmux...), "prompt")
		got := queuedItems(t, filepath.Join(home, ".local", "state", "petrvs-bridge", "spool"))
		if code != 0 || len(got) != 1 || len(got[0].Buttons) != 4 {
			t.Fatalf("exit %d, spooled %+v, stderr %q", code, got, errs)
		}
	})

	t.Run("no pane and no tmux give the plain notice", func(t *testing.T) {
		state := t.TempDir()
		_, errs, code := runEmpty(t, bin, permission, []string{"PB_STATE_DIR=" + state, "PATH=/usr/bin:/bin"}, "prompt")
		got := queuedItems(t, filepath.Join(state, "spool"))
		want := "⏳ **Claude needs your permission to use Bash**\n_Answer with /key N (the number of your choice), /enter or /esc._"
		if code != 0 || len(got) != 1 || got[0].Body != want || len(got[0].Buttons) != 0 {
			t.Fatalf("exit %d, spooled %+v, stderr %q", code, got, errs)
		}
	})

	// Without the key the hash would not match the agent's, so the buttons would be refused there: the hook sends the plain
	// notice instead, and says why on stderr (which Claude Code shows in its log).
	t.Run("a key that cannot be read gives the plain notice", func(t *testing.T) {
		state := t.TempDir()
		if err := os.WriteFile(filepath.Join(state, "prompt-key"), []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(argsFile) // the earlier cases left what tmux was asked
		_, errs, code := runEmpty(t, bin, permission, append([]string{"PB_STATE_DIR=" + state}, withTmux...), "prompt")
		got := queuedItems(t, filepath.Join(state, "spool"))
		want := "⏳ **Claude needs your permission to use Bash**\n_Answer with /key N (the number of your choice), /enter or /esc._"
		if code != 0 || len(got) != 1 || got[0].Body != want || len(got[0].Buttons) != 0 || !strings.Contains(errs, "prompt key") {
			t.Fatalf("exit %d, spooled %+v, stderr %q", code, got, errs)
		}
		if args, err := os.ReadFile(argsFile); err == nil {
			t.Errorf("tmux was asked %q", args)
		}
	})

	t.Run("two hooks and the agent use one key", func(t *testing.T) {
		state := t.TempDir()
		for range 2 {
			if _, errs, code := runEmpty(t, bin, permission, append([]string{"PB_STATE_DIR=" + state}, withTmux...), "prompt"); code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
		}
		got := queuedItems(t, filepath.Join(state, "spool"))
		if len(got) != 2 || len(got[0].Buttons) != 4 || got[0].Buttons[0].Prompt != got[1].Buttons[0].Prompt {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("a notification that needs no answer queues nothing", func(t *testing.T) {
		state := t.TempDir()
		idle := `{"message":"Claude is waiting for your input","notification_type":"idle_prompt"}`
		if _, errs, code := runEmpty(t, bin, idle, append([]string{"PB_STATE_DIR=" + state}, withTmux...), "prompt"); code != 0 {
			t.Fatalf("exit %d: %s", code, errs)
		}
		if got := spooled(t, filepath.Join(state, "spool")); len(got) != 0 {
			t.Fatalf("%q", got)
		}
	})

	t.Run("help is not a configuration problem", func(t *testing.T) {
		if _, errs, code := runEmpty(t, bin, "", nil, "prompt", "-h"); code != 0 || strings.Contains(errs, "configuration problems") {
			t.Errorf("exit %d, stderr %q", code, errs)
		}
	})
}

// `agent run` makes the key that `agent prompt` and the agent's own check of a button both hash with. The bus is a port
// nobody listens on: the agent gets as far as the key, which is all this looks at.
func TestTheDaemonMakesThePromptKeyTheHookShares(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, c := range []string{"agent", "bridgekeys"} {
		must(t, exec.Command("go", "build", "-o", filepath.Join(bin, c), "github.com/n0rm4l-me/petrvs-bridge/cmd/"+c))
	}
	keys := filepath.Join(dir, "keys")
	for _, n := range []string{"hub", "laptop"} {
		must(t, exec.Command(filepath.Join(bin, "bridgekeys"), "gen", "-name", n, "-out", keys))
	}
	daemon := func(t *testing.T, state string) *proc {
		env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "PB_NAME=laptop", "PB_PROJECT=pb-key", "PB_STATE_DIR=" + state, "PB_ALARM=false",
			"PB_IDENTITY=" + filepath.Join(keys, "laptop.identity.json"), "PB_HUB_PUB=" + filepath.Join(keys, "hub.pub.json"),
			"PB_TMUX_SESSION=zzfixa" + strconv.Itoa(os.Getpid()), "PUBSUB_EMULATOR_HOST=127.0.0.1:1"}
		p := &proc{out: &bytes.Buffer{}}
		p.cmd = exec.Command("env", append(append([]string{"-i"}, env...), filepath.Join(bin, "agent"))...)
		p.cmd.Stdout, p.cmd.Stderr = &lockedWriter{p}, &lockedWriter{p}
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.cmd.Process.Kill(); p.cmd.Wait() })
		return p
	}

	t.Run("a new key", func(t *testing.T) {
		state := filepath.Join(dir, "state1")
		p := daemon(t, state)
		path := filepath.Join(state, "prompt-key")
		eventually(t, "the key file\n"+p.output(), 20*time.Second, func() bool {
			info, err := os.Stat(path)
			return err == nil && info.Size() == 32
		})
		disk, _ := os.ReadFile(path)
		if hook, err := screen.LoadKey(state); err != nil || !bytes.Equal(hook, disk) {
			t.Errorf("the hook reads another key: %x %v", hook, err)
		}
		if strings.Contains(p.output(), "prompt key") {
			t.Errorf("the agent complained:\n%s", p.output())
		}
	})

	t.Run("a key file that is damaged is a warning, not the end of the agent", func(t *testing.T) {
		state := filepath.Join(dir, "state2")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "prompt-key"), []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := daemon(t, state)
		eventually(t, "the warning\n"+p.output(), 20*time.Second, func() bool { return strings.Contains(p.output(), "cannot read or create the prompt key") })
		eventually(t, "the agent to go on\n"+p.output(), 20*time.Second, func() bool { return strings.Contains(p.output(), "agent running") })
		if disk, _ := os.ReadFile(filepath.Join(state, "prompt-key")); string(disk) != "short" {
			t.Errorf("the damaged key was replaced: %q", disk)
		}
	})
}
