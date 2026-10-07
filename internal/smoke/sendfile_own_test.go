package smoke

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The chat photos saved in the inbox are read by Claude, and the Stop hook forwards every image Claude reads: without a
// refusal the bot would send the user's own photo back. The hook's environment often lacks PB_WORKDIR, so the default
// inbox is also recognised by its name.
func TestSendFileDoesNotSendTheInboxBack(t *testing.T) {
	bin := sfBuildAgent(t)
	state, work, other := t.TempDir(), t.TempDir(), t.TempDir()
	photo := sfWriteFile(t, filepath.Join(work, ".petrvs-inbox", "20261007-120000-aabbcc-photo.jpg"), []byte("jpeg"))
	chosen := sfWriteFile(t, filepath.Join(other, "chat", "20261007-120000-aabbcc-shot.png"), []byte("png"))
	fine := sfWriteFile(t, filepath.Join(work, "screenshot.png"), []byte("png"))

	for name, c := range map[string]struct {
		env  []string
		path string
	}{
		"the settings name the working directory": {[]string{"PB_STATE_DIR=" + state, "PB_WORKDIR=" + work}, photo},
		"the settings name the inbox":             {[]string{"PB_STATE_DIR=" + state, "PB_INBOX_DIR=" + filepath.Dir(chosen)}, chosen},
		"the hook has only the state directory":   {[]string{"PB_STATE_DIR=" + state}, photo},
	} {
		out, errs, code := sfRun(t, bin, c.env, "send-file", c.path)
		if code != 1 || out != "" || !strings.Contains(errs, "inbox") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
	if n := len(sfQueuedItems(t, state)); n != 0 {
		t.Fatalf("%d items of files from the inbox were queued", n)
	}
	if _, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state, "PB_WORKDIR=" + work}, "send-file", fine); code != 0 {
		t.Fatalf("a file next to the inbox was refused: %s", errs)
	}
}

// Names that a file system folds are the same names: the long s (U+017F) is the letter s, so .awſ is the .aws directory on
// APFS and NTFS; and the state directory is the same under any case.
func TestSendFileRefusesSecretsUnderFoldedNames(t *testing.T) {
	bin := sfBuildAgent(t)
	root, dir := t.TempDir(), t.TempDir()
	state := filepath.Join(root, "state")
	env := []string{"PB_STATE_DIR=" + state}
	long := sfWriteFile(t, filepath.Join(dir, ".awſ", "cli", "cache", "x.json"), []byte("{}"))
	upper := sfWriteFile(t, filepath.Join(dir, ".SSH", "known.png"), []byte("x"))
	key := sfWriteFile(t, filepath.Join(dir, "DEPLOY.PEM"), []byte("x"))
	own := sfWriteFile(t, filepath.Join(root, "STATE", "x.png"), []byte("x"))
	for name, p := range map[string]string{"a long s": long, "upper case": upper, "a key in upper case": key} {
		if _, errs, code := sfRun(t, bin, env, "send-file", p); code != 1 {
			t.Errorf("%s: exit %d, %q", name, code, errs)
		}
	}
	// the state directory in another case is refused by name whether or not the file system folds it
	if _, errs, code := sfRun(t, bin, env, "send-file", own); code != 1 || !strings.Contains(errs, "belongs to the bridge") {
		t.Errorf("the state directory in upper case: exit %d, %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(state, "spool")); err == nil {
		if n := len(sfQueuedItems(t, state)); n != 0 {
			t.Fatalf("%d items of refused files were queued", n)
		}
	}
}
