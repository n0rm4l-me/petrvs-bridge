package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
)

func mustLink(t *testing.T, old, new string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(new), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(old, new); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}
}

func wantRefused(t *testing.T, what, path, state string, env map[string]string, inError string) {
	t.Helper()
	env["PB_STATE_DIR"] = state
	data, _, err := sfRead(path, state, 1<<20, envOf(env))
	if err == nil {
		t.Errorf("%s: %d bytes were read", what, len(data))
		return
	}
	if !strings.Contains(err.Error(), inError) {
		t.Errorf("%s: refused with %q, want %q in it", what, err, inError)
	}
}

// The name of a file says little: the alarm secret, the identity and the files in the state directory may have a second
// name anywhere (a hard link), and nothing in that name tells what it is. The file itself is compared.
func TestAHardLinkToAFileOfTheBridgeIsRefused(t *testing.T) {
	root := realTemp(t)
	state := filepath.Join(root, "state")
	secret := mustWrite(t, filepath.Join(root, "keys", "alarm.b32"), "JBSWY3DPEHPK3PXP\n")
	id := mustWrite(t, filepath.Join(root, "keys", "laptop.json"), "not an identity, as far as the markers can tell")
	alarmState := mustWrite(t, filepath.Join(state, "alarm-code-state"), "17")
	queued := mustWrite(t, filepath.Join(state, "spool", "00000000000000000001-1-00000001.json"), `{"type":"reply","body":"x"}`)
	other := mustWrite(t, filepath.Join(root, "other", "notes.txt"), "an ordinary file")
	pics := filepath.Join(root, "pics")
	env := map[string]string{"PB_IDENTITY": id, "PB_ALARM_SECRET_FILE": secret}
	for i, c := range []struct{ name, target string }{{"alarm secret", secret}, {"identity", id}, {"alarm state", alarmState}, {"queued message", queued}} {
		name, target := c.name, c.target
		link := filepath.Join(pics, fmt.Sprintf("picture-%d.png", i)) // a name that says nothing
		mustLink(t, target, link)
		wantRefused(t, name, link, state, env, "belongs to the bridge")
	}
	ok := filepath.Join(pics, "fine.png")
	mustLink(t, other, ok)
	if _, _, err := sfRead(ok, state, 1<<20, envOf(map[string]string{"PB_STATE_DIR": state, "PB_IDENTITY": id, "PB_ALARM_SECRET_FILE": secret})); err != nil {
		t.Errorf("a hard link to an ordinary file was refused: %v", err)
	}
	// the hook's shell does not have the identity and the alarm secret in its environment; the state directory is always known
	link := filepath.Join(pics, "alarm-state-again.png")
	mustLink(t, alarmState, link)
	wantRefused(t, "alarm state without the other settings", link, state, map[string]string{}, "belongs to the bridge")
}

// A directory is the same directory under every name the file system gives it. The places are compared with the
// directories above the file, not as text: a link to the state directory, or a name that differs only in how the file
// system folds it, is the state directory.
func TestOwnDirectoriesAreRecognisedByIdentityNotByText(t *testing.T) {
	root := realTemp(t)
	state := filepath.Join(root, "state")
	file := mustWrite(t, filepath.Join(state, "deep", "er", "x.png"), "x")
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "another-name")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	// the place is named by the link; the file is where the descriptor says, below the real directory
	o := &sfOwn{dirs: []sfPlace{{link, sfWhyBridge}}}
	if err := o.identity(file, fi); err == nil || !strings.Contains(err.Error(), "belongs to the bridge") {
		t.Errorf("the state directory under another name: %v", err)
	}
	o = &sfOwn{dirs: []sfPlace{{filepath.Join(root, "elsewhere"), sfWhyBridge}}}
	if err := o.identity(file, fi); err != nil {
		t.Errorf("a directory that does not exist refused a file: %v", err)
	}
	o = &sfOwn{dirs: []sfPlace{{filepath.Join(state, "deep", "er", "x.png"), sfWhyBridge}}}
	if err := o.identity(file, fi); err != nil {
		t.Errorf("a file named as a directory refused the file: %v", err)
	}

	// The same on a file system that folds names: STATE is state, and composed and decomposed letters are one name. The
	// probes skip this where the file system tells the spellings apart.
	for name, c := range map[string]struct{ made, asked string }{
		"case":          {"state", "STATE"},
		"normalisation": {"caf\u00e9", "cafe\u0301"},
	} {
		dir := filepath.Join(root, c.made)
		f := mustWrite(t, filepath.Join(dir, "x.png"), "x")
		alt := filepath.Join(root, c.asked)
		if _, err := os.Stat(alt); err != nil {
			t.Logf("%s: this file system tells %q and %q apart; not tested here", name, c.made, c.asked)
			continue
		}
		st, _ := os.Stat(f)
		if err := (&sfOwn{dirs: []sfPlace{{alt, sfWhyBridge}}}).identity(f, st); err == nil {
			t.Errorf("%s: the directory under a spelling the file system folds was not recognised", name)
		}
	}
}

func TestFilesInTheInboxAreNotSentBack(t *testing.T) {
	root := realTemp(t)
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	const file = "20260101-120000-abcdef-photo.jpg"
	for name, c := range map[string]struct {
		env   map[string]string
		inbox string
		rel   string
	}{
		"in the working directory": {map[string]string{"PB_WORKDIR": work}, filepath.Join(work, ".petrvs-inbox"), file},
		"a chosen inbox":           {map[string]string{"PB_WORKDIR": work, "PB_INBOX_DIR": filepath.Join(root, "chosen")}, filepath.Join(root, "chosen"), file},
		"the default in the state": {map[string]string{}, filepath.Join(state, "inbox"), file},
		"a chosen one, no workdir": {map[string]string{"PB_INBOX_DIR": filepath.Join(root, "elsewhere", "in")}, filepath.Join(root, "elsewhere", "in"), file},
		"deeper in the inbox":      {map[string]string{"PB_INBOX_DIR": filepath.Join(root, "in2")}, filepath.Join(root, "in2"), "sub/deeper/p.jpg"},
		"a blank workdir":          {map[string]string{"PB_WORKDIR": "  "}, filepath.Join(state, "inbox"), file},
	} {
		photo := mustWrite(t, filepath.Join(c.inbox, filepath.FromSlash(c.rel)), "a photo")
		env := map[string]string{}
		for k, v := range c.env {
			env[k] = v
		}
		wantRefused(t, name, photo, state, env, "inbox")
		// the command refuses it as well, and queues nothing
		env["PB_STATE_DIR"] = state
		if code := sfSendFile([]string{photo}, envOf(env)); code != 1 {
			t.Errorf("%s: send-file exit %d", name, code)
		}
	}
	if sp, err := spool.Open(filepath.Join(state, "spool")); err != nil || sp.Len() != 0 {
		t.Fatalf("something was queued: %v", err)
	}

	// a file next to the inbox, or in a directory that only has a similar name, is sent as before
	for _, p := range []string{
		mustWrite(t, filepath.Join(work, "screenshot.png"), "x"), mustWrite(t, filepath.Join(work, ".petrvs-inbox-old", "a.png"), "x"),
		mustWrite(t, filepath.Join(work, "petrvs-inbox", "a.png"), "x"),
	} {
		env := map[string]string{"PB_STATE_DIR": state, "PB_WORKDIR": work}
		if _, _, err := sfRead(p, state, 1<<20, envOf(env)); err != nil {
			t.Errorf("%s was refused: %v", p, err)
		}
	}
}

// The shell of the assistant usually has no PB_WORKDIR, so the inbox cannot be worked out from it; the name of the
// default directory still gives it away, whatever its case.
func TestTheDefaultInboxIsRefusedByNameWhenTheWorkingDirectoryIsNotKnown(t *testing.T) {
	root := realTemp(t)
	state := filepath.Join(root, "state")
	for _, dir := range []string{".petrvs-inbox", ".PETRVS-Inbox"} {
		photo := mustWrite(t, filepath.Join(root, "project", dir, "sub", "20260101-120000-abcdef-photo.png"), "a photo")
		wantRefused(t, dir, photo, state, map[string]string{}, "inbox")
	}
}

// The rule that finds the inbox is written down twice, in the configuration and here; they must agree, or the agent
// saves the photos in one place and refuses to send them back from another.
func TestTheInboxIsWhereTheConfigurationPutsIt(t *testing.T) {
	root := realTemp(t)
	for name, env := range map[string]map[string]string{
		"nothing set":   {},
		"a workdir":     {"PB_WORKDIR": filepath.Join(root, "work")},
		"an inbox":      {"PB_INBOX_DIR": filepath.Join(root, "in")},
		"both":          {"PB_WORKDIR": filepath.Join(root, "work"), "PB_INBOX_DIR": filepath.Join(root, "in")},
		"blank values":  {"PB_WORKDIR": " ", "PB_INBOX_DIR": "\t"},
		"padded values": {"PB_WORKDIR": " " + filepath.Join(root, "work") + " "},
	} {
		env["PB_STATE_DIR"] = filepath.Join(root, "state")
		get := envOf(env)
		cfg, code := parse(nil, localEnv(get))
		if cfg == nil {
			t.Fatalf("%s: the configuration was not accepted (exit %d)", name, code)
		}
		if got := sfInboxDir(cfg.StateDir, get); got != cfg.InboxDir {
			t.Errorf("%s: send-file thinks the inbox is %s, the configuration says %s", name, got, cfg.InboxDir)
		}
	}
	if sfInboxName != ".petrvs-inbox" {
		t.Error("the inbox name changed; so must the configuration")
	}
}

// fdPath is what the whole check rests on: it must say where the file is, not where the path said.
func TestFdPathSaysWhereTheOpenFileReallyIs(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the system cannot be asked")
	}
	root := realTemp(t)
	file := mustWrite(t, filepath.Join(root, "real", "dir", "x.txt"), "x")
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real", "dir"), link); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(link, "x.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := fdPath(f, "ignored")
	if err != nil || got != file {
		t.Fatalf("fdPath = %q, %v; want %q", got, err, file)
	}
	// Linux reports a removed file with " (deleted)" after the name, which would hide the name from the checks: refused.
	if runtime.GOOS == "linux" {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if got, err := fdPath(f, "ignored"); err == nil {
			t.Fatalf("a removed file is at %q", got)
		}
	}
}
