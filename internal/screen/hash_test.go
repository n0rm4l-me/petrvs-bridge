package screen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func hashOfDialog(t *testing.T, s string) string {
	t.Helper()
	p, ok := ExtractPrompt(s)
	if !ok {
		t.Fatalf("no prompt in\n%s", s)
	}
	return p.Hash
}

// What is hashed is what the user approves. Two commands that differ in a space, in a blank line or in a line that looks
// like a tip are two commands, and the button of one must not answer the other.
func TestTheHashSeesIndentationBlankLinesAndTipLinesOfTheCommand(t *testing.T) {
	base := hashOfDialog(t, permissionDialog(1, " │ echo a", " │ echo b"))
	for name, frame := range map[string][]string{
		"an extra space of indentation": {" │ echo a", " │  echo b"},
		"a blank line between":          {" │ echo a", "", " │ echo b"},
		"a tip line added":              {" │ echo a", " │ Tip: echo b", " │ echo b"},
		"another command":               {" │ echo a", " │ echo c"},
		"a trailing word":               {" │ echo a", " │ echo b x"},
		"no bar on the second line":     {" │ echo a", "   echo b"},
		"indentation before the bar":    {" │ echo a", "  │ echo b"},
	} {
		if got := hashOfDialog(t, permissionDialog(1, frame...)); got == base {
			t.Errorf("%s: the hash did not change", name)
		}
	}
	// the indentation of a line that has no bar counts as well (the lines of a heredoc may be drawn without one)
	a := hashOfDialog(t, permissionDialog(1, " │ echo a", "   echo b"))
	if b := hashOfDialog(t, permissionDialog(1, " │ echo a", "     echo b")); a == b {
		t.Error("the indentation of a line without the bar did not change the hash")
	}
	// a space at the end of a line is not drawn
	if got := hashOfDialog(t, permissionDialog(1, " │ echo a   ", " │ echo b  ")); got != base {
		t.Error("white space at the end of a line changed the hash")
	}
}

func TestTheHashIgnoresOnlyTheHighlightTheIndentationOfChoicesAndTheTip(t *testing.T) {
	frame := []string{" │ echo a"}
	base := hashOfDialog(t, permissionDialog(1, frame...))
	for cursor := 2; cursor <= 3; cursor++ {
		if hashOfDialog(t, permissionDialog(cursor, frame...)) != base {
			t.Errorf("the highlight on %d changed the hash", cursor)
		}
	}
	// a tip in the dialog, outside the frame, changes from time to time
	withTip := strings.Replace(permissionDialog(1, frame...), " Does the thing", " Tip: one thing\n Does the thing", 1)
	otherTip := strings.Replace(withTip, "one thing", "another thing", 1)
	if a, b := hashOfDialog(t, withTip), hashOfDialog(t, otherTip); a != b || a != base {
		t.Errorf("the tip line moved the hash: %s %s %s", base, a, b)
	}
	// the choices may be indented differently (the terminal is a different width)
	moved := strings.Replace(permissionDialog(1, frame...), " ❯ 1. Yes", "  ❯  1. Yes", 1)
	if hashOfDialog(t, moved) != base {
		t.Error("the indentation of a choice changed the hash")
	}
	// trailing blank lines are the size of the window
	if hashOfDialog(t, permissionDialog(1, frame...)+"\n\n\n") != base {
		t.Error("blank lines at the end of the capture changed the hash")
	}
	// but a word in a choice is not nothing
	if hashOfDialog(t, strings.Replace(permissionDialog(1, frame...), "3. No", "3. Yes", 1)) == base {
		t.Error("another choice, the same hash")
	}
}

func TestAHashHasSixteenHexCharactersAndTheKeyChangesIt(t *testing.T) {
	t.Cleanup(func() { SetHashKey(nil) })
	s := permissionDialog(1, " │ echo a")
	plain := hashOfDialog(t, s)
	if len(plain) != 16 {
		t.Fatalf("%q", plain)
	}

	k1, k2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	SetHashKey(k1)
	a := hashOfDialog(t, s)
	if a == plain || len(a) != 16 || a != hashOfDialog(t, s) {
		t.Errorf("keyed %s, unkeyed %s", a, plain)
	}
	SetHashKey(k2)
	if b := hashOfDialog(t, s); b == a || b == plain {
		t.Errorf("another key gave the same hash %s", b)
	}
	SetHashKey(k1)
	k1[0] = 9 // the caller's slice is not the key
	if hashOfDialog(t, s) != a {
		t.Error("the key follows the caller's slice")
	}
	SetHashKey([]byte{})
	if hashOfDialog(t, s) != plain {
		t.Error("an empty key is not unkeyed")
	}
}

func TestLoadKeyCreatesAPrivateKeyAndReadsItBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "deeper")
	k, err := LoadKey(dir)
	if err != nil || len(k) != 32 || bytes.Equal(k, make([]byte, 32)) {
		t.Fatalf("%x %v", k, err)
	}
	info, err := os.Stat(filepath.Join(dir, "prompt-key"))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("%v %v", info, err)
	}
	again, err := LoadKey(dir)
	if err != nil || !bytes.Equal(again, k) {
		t.Fatalf("the key changed: %x %v", again, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".prompt-key-*")); len(left) != 0 {
		t.Errorf("temporary files are left: %v", left)
	}
	other, err := LoadKey(t.TempDir())
	if err != nil || bytes.Equal(other, k) {
		t.Errorf("two installs share a key")
	}
}

// The agent and a hook may start at the same moment and both find no key: they must end with the same one.
func TestLoadKeyGivesEveryCallerThatRacesTheSameKey(t *testing.T) {
	dir := t.TempDir()
	const n = 24
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errs[i] = LoadKey(dir)
		}()
	}
	close(start)
	wg.Wait()
	for i := range keys {
		if errs[i] != nil || !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("caller %d: %x %v (first: %x)", i, keys[i], errs[i], keys[0])
		}
	}
	if disk, _ := os.ReadFile(filepath.Join(dir, "prompt-key")); !bytes.Equal(disk, keys[0]) {
		t.Error("the key on disk is not the one everyone got")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".prompt-key-*")); len(left) != 0 {
		t.Errorf("temporary files are left: %v", left)
	}
}

// A key file that is not what was written is an error, and it is not replaced: a new key would make the buttons that are
// out in the chat fail, and would hide that something touched the file.
func TestLoadKeyRefusesAFileThatIsNotAKey(t *testing.T) {
	for name, setup := range map[string]func(path string) error{
		"too short": func(p string) error { return os.WriteFile(p, []byte("short"), 0o600) },
		"too long":  func(p string) error { return os.WriteFile(p, bytes.Repeat([]byte{1}, 33), 0o600) },
		"a link": func(p string) error {
			target := filepath.Join(filepath.Dir(p), "elsewhere")
			if err := os.WriteFile(target, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, p)
		},
		"a directory": func(p string) error { return os.Mkdir(p, 0o700) },
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "prompt-key")
		if err := setup(path); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Lstat(path)
		if k, err := LoadKey(dir); err == nil {
			t.Errorf("%s: accepted, key %x", name, k)
		}
		if after, err := os.Lstat(path); err != nil || !os.SameFile(before, after) {
			t.Errorf("%s: the file was replaced", name)
		}
	}
}

func TestLoadKeyFailsWhereItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := LoadKey(filepath.Join(blocker, "state")); err == nil {
		t.Errorf("accepted, key %x", k)
	}
}
