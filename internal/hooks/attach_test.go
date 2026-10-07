package hooks

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// tmux-attach-alert.sh: a client that attaches to the session of Claude Code is reported to the chat at once, once, and can
// be disconnected. These tests run a real tmux, on a server of their own: the environment they give it has no TMUX (inside
// a tmux pane that variable names the running server and wins over TMUX_TMPDIR, which once ended a live session), and the
// socket is checked before anything else is done.

const attachScript = "../../examples/tmux-attach-alert.sh"

type tmuxRig struct {
	t     *testing.T
	dir   string // short: the path of a unix socket is limited to about 100 bytes
	env   []string
	state string
	agent string
}

func newTmuxRig(t *testing.T) *tmuxRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "pbt")
	if err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real // on macOS /tmp is a link to /private/tmp, and tmux reports the real path
	}
	r := &tmuxRig{t: t, dir: dir, state: filepath.Join(dir, "state"), agent: filepath.Join(dir, "agent")}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_PANE=") && !strings.HasPrefix(kv, "TMUX_TMPDIR=") {
			r.env = append(r.env, kv)
		}
	}
	sock := filepath.Join(dir, "sock")
	if err := os.MkdirAll(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	r.env = append(r.env, "TMUX_TMPDIR="+sock)
	script := "#!/bin/sh\nn=$(ls \"" + dir + "\"/sent.* 2>/dev/null | wc -l | tr -d ' ')\necho \"$*\" > \"" + dir + "/args.$n\"\ncat > \"" + dir + "/sent.$n\"\n"
	if err := os.WriteFile(r.agent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		r.tmux("kill-server").Run() // r.env names the test server's directory and has no TMUX: it cannot reach another server
		os.RemoveAll(dir)
	}
	if out, err := r.tmux("new-session", "-d", "-s", "claude-dev", "-x", "80", "-y", "24", "cat").CombinedOutput(); err != nil {
		stop()
		t.Skipf("cannot start tmux here: %v %s", err, out)
	}
	// The test server is the one in dir, or the test stops before it touches anything else.
	out, err := r.tmux("display-message", "-p", "-t", "=claude-dev", "#{socket_path}").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), sock) {
		stop()
		t.Fatalf("this is not the test server (socket %q, error %v): refusing to go on", out, err)
	}
	t.Cleanup(stop)
	if err := r.tmux("new-session", "-d", "-s", "other", "cat").Run(); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *tmuxRig) tmux(args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", args...)
	cmd.Env = r.env
	return cmd
}

// script runs the script with the settings of the rig and returns its output and exit code.
func (r *tmuxRig) script(extraEnv []string, args ...string) (string, int) {
	r.t.Helper()
	cmd := exec.Command("sh", append([]string{attachScript}, args...)...)
	cmd.Env = append(append([]string{}, r.env...), "PB_STATE_DIR="+r.state, "AGENT_BIN="+r.agent, "PB_TMUX_SESSION=claude-dev")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	return string(out), code
}

func (r *tmuxRig) install(extraEnv ...string) {
	r.t.Helper()
	if out, code := r.script(extraEnv, "install"); code != 0 {
		r.t.Fatalf("install failed (%d): %s", code, out)
	}
}

type tmuxClient struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when the client process has ended
}

// attach starts a control-mode client, which needs no terminal and counts as a client of the session all the same.
func (r *tmuxRig) attach(session string) *tmuxClient {
	r.t.Helper()
	cmd := r.tmux("-C", "attach", "-t", "="+session)
	in, err := cmd.StdinPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	c := &tmuxClient{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(c.done) }()
	r.t.Cleanup(func() { in.Close(); cmd.Process.Kill() })
	return c
}

func (c *tmuxClient) ended(within time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(within):
		return false
	}
}

// alerts returns what the stand-in agent was given, as "arguments: text", in order.
func (r *tmuxRig) alerts() []string {
	files, _ := filepath.Glob(filepath.Join(r.dir, "sent.*"))
	sort.Strings(files)
	var out []string
	for _, f := range files {
		text, _ := os.ReadFile(f)
		if len(text) == 0 {
			continue // the stand-in creates the file before it writes the text: not an alert yet
		}
		args, _ := os.ReadFile(strings.Replace(f, "sent.", "args.", 1))
		out = append(out, strings.TrimSpace(string(args))+": "+string(text))
	}
	return out
}

func (r *tmuxRig) waitAlerts(n int) []string {
	r.t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.alerts(); len(got) >= n {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatalf("%d alerts after 4s, want %d: %q", len(r.alerts()), n, r.alerts())
	return nil
}

func (r *tmuxRig) hooks() string {
	out, _ := r.tmux("show-hooks", "-t", "=claude-dev:").Output()
	return string(out)
}

func TestAClientThatAttachesIsReportedOnceAtOnce(t *testing.T) {
	r := newTmuxRig(t)
	r.install()
	c := r.attach("claude-dev")
	got := r.waitAlerts(1)
	if !strings.HasPrefix(got[0], "send -type alert -: ") {
		t.Fatalf("%q: want an alert", got[0])
	}
	for _, want := range []string{"**Someone attached to the tmux session** `claude-dev`", fmt.Sprintf("pid %d", c.cmd.Process.Pid), "read-write, control mode", "It is still attached."} {
		if !strings.Contains(got[0], want) {
			t.Errorf("%q does not say %q", got[0], want)
		}
	}
	// One attach starts two hooks (client-attached and client-session-changed); the client is reported once.
	time.Sleep(800 * time.Millisecond)
	if got := r.alerts(); len(got) != 1 {
		t.Fatalf("%d alerts for one attach: %q", len(got), got)
	}
	if c.ended(300 * time.Millisecond) {
		t.Fatal("the client was disconnected although nothing asked for it")
	}
}

func TestEachNewClientIsReportedAndTheOldOnesAreNot(t *testing.T) {
	r := newTmuxRig(t)
	owner := r.attach("claude-dev") // there before the hooks
	time.Sleep(300 * time.Millisecond)
	r.install()
	second := r.attach("claude-dev")
	got := r.waitAlerts(1)
	time.Sleep(600 * time.Millisecond)
	got = r.alerts()
	if len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("pid %d", second.cmd.Process.Pid)) || strings.Contains(got[0], fmt.Sprintf("pid %d,", owner.cmd.Process.Pid)) {
		t.Fatalf("%q: want one alert, for the second client only", got)
	}
	r.attach("claude-dev")
	if got := r.waitAlerts(2); len(got) != 2 {
		t.Fatalf("%q", got)
	}
}

func TestAnotherSessionIsNotWatched(t *testing.T) {
	r := newTmuxRig(t)
	r.install()
	r.attach("other")
	time.Sleep(1200 * time.Millisecond)
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("%q: the hooks are on the one session", got)
	}
}

func TestAClientCanBeDisconnected(t *testing.T) {
	r := newTmuxRig(t)
	r.install("PB_ATTACH_KICK=1")
	c := r.attach("claude-dev")
	if !c.ended(4 * time.Second) {
		t.Fatal("the client was not disconnected")
	}
	got := r.waitAlerts(1)
	if !strings.Contains(got[0], "It was disconnected.") {
		t.Fatalf("%q", got[0])
	}
}

func TestAnAllowedWindowKeepsTheClientButStillReportsIt(t *testing.T) {
	r := newTmuxRig(t)
	if err := os.MkdirAll(r.state, 0o700); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(10 * time.Minute).Unix()
	if err := os.WriteFile(filepath.Join(r.state, "attach-allow-until"), []byte(fmt.Sprint(until)), 0o600); err != nil {
		t.Fatal(err)
	}
	r.install("PB_ATTACH_KICK=1")
	c := r.attach("claude-dev")
	got := r.waitAlerts(1)
	if !strings.Contains(got[0], "Not disconnected: attaching is allowed until") {
		t.Fatalf("%q", got[0])
	}
	if c.ended(1200 * time.Millisecond) {
		t.Fatal("a client was disconnected inside the allowed window")
	}
}

func TestAWindowThatHasPassedAllowsNothing(t *testing.T) {
	r := newTmuxRig(t)
	if err := os.MkdirAll(r.state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.state, "attach-allow-until"), []byte(fmt.Sprint(time.Now().Add(-time.Minute).Unix())), 0o600); err != nil {
		t.Fatal(err)
	}
	r.install("PB_ATTACH_KICK=1")
	if c := r.attach("claude-dev"); !c.ended(4 * time.Second) {
		t.Fatal("the client was not disconnected")
	}
}

func TestUninstallTakesTheHooksOff(t *testing.T) {
	r := newTmuxRig(t)
	r.install()
	if !strings.Contains(r.hooks(), "client-attached[0]") {
		t.Fatalf("not installed: %s", r.hooks())
	}
	if out, code := r.script(nil, "uninstall"); code != 0 {
		t.Fatalf("%d: %s", code, out)
	}
	if strings.Contains(r.hooks(), "client-attached[0]") || strings.Contains(r.hooks(), "client-session-changed[0]") {
		t.Fatalf("still installed: %s", r.hooks())
	}
	r.attach("claude-dev")
	time.Sleep(1200 * time.Millisecond)
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestInstallingTwiceKeepsOneHook(t *testing.T) {
	r := newTmuxRig(t)
	r.install()
	r.install()
	if strings.Contains(r.hooks(), "client-attached[1]") {
		t.Fatalf("a second copy of the hook: %s", r.hooks())
	}
	r.attach("claude-dev")
	r.waitAlerts(1)
	time.Sleep(600 * time.Millisecond)
	if got := r.alerts(); len(got) != 1 {
		t.Fatalf("%q", got)
	}
}

// guard puts the hooks on and then becomes the command, so it goes in front of the command that starts Claude Code and the
// hooks are back after every restart of the session.
func TestGuardInstallsTheHooksAndRunsTheCommand(t *testing.T) {
	r := newTmuxRig(t)
	marker := filepath.Join(r.dir, "ran")
	if out, code := r.script(nil, "guard", "sh", "-c", "echo $0 > "+marker, "started"); code != 0 {
		t.Fatalf("%d: %s", code, out)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != "started" {
		t.Fatalf("the command did not run, or not with its arguments: %q", b)
	}
	if !strings.Contains(r.hooks(), "client-attached[0]") {
		t.Fatalf("guard did not install: %s", r.hooks())
	}
}

func TestGuardStillRunsTheCommandWhenTheHooksCannotBeInstalled(t *testing.T) {
	r := newTmuxRig(t)
	marker := filepath.Join(r.dir, "ran")
	if _, code := r.script([]string{"PB_STATE_DIR=/tmp/it's here"}, "guard", "sh", "-c", "echo ok > "+marker); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != "ok" {
		t.Fatal("Claude Code would not have started: a failed install must not stand in its way")
	}
}

func TestSettingsThatTmuxOrShWouldReadAsSyntaxAreRefused(t *testing.T) {
	for _, env := range []string{"PB_STATE_DIR=/tmp/a'b", "PB_STATE_DIR=/tmp/$(id)", "PB_STATE_DIR=/tmp/a#{x}", "PB_STATE_DIR=/tmp/a%H", "AGENT_BIN=/x;touch /tmp/pwned", "PB_TMUX_SESSION=a\"b"} {
		r := newTmuxRig(t)
		out, code := r.script([]string{env}, "install")
		if code == 0 || !strings.Contains(out, "not installing") {
			t.Errorf("%s: exit %d, %q: want a refusal", env, code, out)
		}
		if strings.Contains(r.hooks(), "client-attached[0]") {
			t.Errorf("%s: a hook was installed", env)
		}
	}
}

func TestAMissingSessionIsAnErrorNotASilentSuccess(t *testing.T) {
	r := newTmuxRig(t)
	if _, code := r.script([]string{"PB_TMUX_SESSION=nothere"}, "install"); code == 0 {
		t.Fatal("install said it was done for a session that does not exist")
	}
}
