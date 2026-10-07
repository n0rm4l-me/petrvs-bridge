package hooks

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The logic of the Notification hook (what is forwarded, the prompt from the screen, the buttons, hiding secrets) lives
// in `agent prompt` and is tested in internal/promptcmd and, through the built binary, in internal/smoke. What is left for
// the script is to hand Claude Code's JSON to that command, unchanged, and to need nothing else.

// notifyRun runs the Notification hook with the payload on its standard input and a stand-in for the agent that records
// its arguments and standard input. It returns the arguments, the input, the output and the exit code.
func notifyRun(t *testing.T, payload string, env []string, agentExit int) (args, input, out string, code int) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "agent")
	cat, err := exec.LookPath("cat") // the stand-in needs it even though the hook's PATH is empty
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" > \"" + dir + "/args\"\n" + cat + " > \"" + dir + "/stdin\"\nexit " + string(rune('0'+agentExit)) + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "../../examples/claude-notify-hook.sh")
	// Nothing but what the test gives: not jq, not tmux, not the agent on PATH.
	cmd.Env = append([]string{"PATH=" + dir + "/empty", "AGENT_BIN=" + fake}, env...)
	cmd.Stdin = strings.NewReader(payload)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "args"))
	in, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	return strings.TrimSpace(string(a)), string(in), buf.String(), code
}

func TestTheNotificationHookHandsTheJSONToAgentPromptUnchanged(t *testing.T) {
	for name, payload := range map[string]string{
		"a permission prompt": `{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash","notification_type":"permission_prompt","cwd":"/work"}` + "\n",
		"no final newline":    `{"message":"Claude needs your permission"}`,
		"non-ASCII and more":  "{\"message\":\"Claude braucht Erlaubnis für ⏳ ‘ls’\",\"extra\":\"" + strings.Repeat("x", 50000) + "\"}\n",
		"idle (dropped by the command, not by the script)": `{"message":"Claude is waiting for your input","notification_type":"idle_prompt"}` + "\n",
		"not JSON at all": "garbage\n",
	} {
		args, input, out, code := notifyRun(t, payload, []string{"TMUX_PANE=%1", "PB_STATE_DIR=/nowhere"}, 0)
		if args != "prompt" {
			t.Errorf("%s: the agent was called with %q", name, args)
		}
		if input != payload {
			t.Errorf("%s: the agent was given %d bytes, not the %d it was sent", name, len(input), len(payload))
		}
		if code != 0 || out != "" {
			t.Errorf("%s: exit %d, output %q", name, code, out)
		}
	}
}

// A hook that fails shows up in Claude Code; one that swallowed the agent's failure would lose a prompt without a trace.
func TestTheNotificationHookFailsWhenTheAgentDoes(t *testing.T) {
	if _, _, _, code := notifyRun(t, `{"message":"x"}`, nil, 1); code != 1 {
		t.Fatalf("exit %d", code)
	}
}
