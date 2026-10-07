package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
)

// sfInboxName is the directory that holds the files from the chat when it is left to the working directory of Claude Code
// (config.ParseAgent builds the same path). It is also refused by name: the shell of the assistant often lacks PB_WORKDIR,
// and then the path of the inbox cannot be worked out, but the name still gives it away.
const sfInboxName = ".petrvs-inbox"

// sfScanLimit bounds how many entries of a directory are looked at when files are compared by identity.
const sfScanLimit = 2000

const (
	sfWhyBridge = "belongs to the bridge (its state, keys or secrets)"
	sfWhyInbox  = "is in the inbox for files from the chat, and those are not sent back"
)

// sfPlace is a directory or a file that must not be sent, and what to say about it.
type sfPlace struct {
	path, why string
}

// sfOwn is what belongs to the bridge. The paths go to filetx.Denied in every spelling they can take (it sees names only);
// the places themselves are compared with the file by identity (os.SameFile), which neither a difference in case or
// Unicode, nor a link, nor a second name for the same file can get around.
type sfOwn struct {
	text      []string // spellings of the bridge's places; everything below a directory is refused
	inboxText []string // spellings of the inbox, which has its own message
	dirs      []sfPlace
	files     []sfPlace
	state     string // the state directory: every file in it is one of the bridge's
}

// sfInboxDir is where the files from the chat are saved, worked out as config.ParseAgent does: PB_INBOX_DIR, else
// .petrvs-inbox in PB_WORKDIR, else inbox in the state directory.
func sfInboxDir(stateDir string, getenv func(string) string) string {
	if d := strings.TrimSpace(getenv("PB_INBOX_DIR")); d != "" {
		return d
	}
	if w := strings.TrimSpace(getenv("PB_WORKDIR")); w != "" {
		return filepath.Join(w, sfInboxName)
	}
	return filepath.Join(stateDir, "inbox")
}

// sfSpellings is a path as given, absolute and with links resolved.
func sfSpellings(p string) []string {
	out := []string{p}
	if abs, err := filepath.Abs(p); err == nil {
		out = append(out, abs)
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// sfOwnPlaces lists what belongs to the bridge. The state directory and the inbox are always known; the identity and the
// alarm secret only when the environment names them (a hook does not always have those settings), and then what is left
// to go by is the content and the name, as in sfSecretMarks and filetx.Denied. No path is made up.
func sfOwnPlaces(stateDir string, getenv func(string) string) *sfOwn {
	o := &sfOwn{state: stateDir}
	dir := func(p, why string) {
		if p != "" {
			o.text = append(o.text, sfSpellings(p)...)
			o.dirs = append(o.dirs, sfPlace{p, why})
		}
	}
	file := func(p, why string) {
		if p != "" {
			o.text = append(o.text, sfSpellings(p)...)
			o.files = append(o.files, sfPlace{p, why})
		}
	}
	dir(stateDir, sfWhyBridge)
	if id := getenv("PB_IDENTITY"); id != "" {
		file(id, sfWhyBridge)
		dir(filepath.Dir(id), sfWhyBridge)
	}
	file(getenv("PB_ALARM_SECRET_FILE"), sfWhyBridge)
	// The inbox is often below the state directory; it is looked at first, so that it is the one that is named.
	inbox := sfInboxDir(stateDir, getenv)
	o.inboxText = sfSpellings(inbox)
	o.dirs = append([]sfPlace{{inbox, sfWhyInbox}}, o.dirs...)
	return o
}

// named refuses a path by its name: the inbox, the bridge's own places and the names of secrets, each in whatever case the
// path is spelled.
func (o *sfOwn) named(p string) error {
	for _, d := range o.inboxText {
		if filetx.Under(p, d) {
			return fmt.Errorf("%s %s", filepath.Base(p), sfWhyInbox)
		}
	}
	if err := filetx.Denied(p, o.text...); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "filetx: "))
	}
	return nil
}

// identity refuses the open file when it is one of the bridge's own files, under whatever name it is reached, or when real
// (where the open descriptor says it is) lies inside one of the bridge's directories. Directories are found by walking up
// from the file and asking whether any of them is the same directory as an own one, so a spelling that only a file system
// which folds names understands (STATE for state) is still that directory.
func (o *sfOwn) identity(real string, fi os.FileInfo) error {
	base := filepath.Base(real)
	for _, f := range o.files {
		if st, err := os.Stat(f.path); err == nil && os.SameFile(st, fi) {
			return fmt.Errorf("%s %s", base, f.why)
		}
	}
	if o.state != "" {
		// a hard link to a file in the state directory is that file (the alarm state, a queued message)
		for _, d := range []string{o.state, filepath.Join(o.state, "spool")} {
			if sameAsEntryOf(d, fi) {
				return fmt.Errorf("%s %s", base, sfWhyBridge)
			}
		}
	}
	type own struct {
		st  os.FileInfo
		why string
	}
	var dirs []own
	for _, d := range o.dirs {
		if st, err := os.Stat(d.path); err == nil && st.IsDir() {
			dirs = append(dirs, own{st, d.why})
		}
	}
	for dir := filepath.Dir(real); ; {
		if filetx.Fold(filepath.Base(dir)) == filetx.Fold(sfInboxName) {
			return fmt.Errorf("%s %s", base, sfWhyInbox)
		}
		if st, err := os.Stat(dir); err == nil {
			for _, d := range dirs {
				if os.SameFile(st, d.st) {
					return fmt.Errorf("%s %s", base, d.why)
				}
			}
		}
		up := filepath.Dir(dir)
		if up == dir {
			return nil
		}
		dir = up
	}
}

// sameAsEntryOf says whether fi is one of the regular files in dir (not below it).
func sameAsEntryOf(dir string, fi os.FileInfo) bool {
	d, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer d.Close()
	entries, err := d.ReadDir(sfScanLimit)
	if err != nil && len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if st, err := e.Info(); err == nil && os.SameFile(st, fi) {
			return true
		}
	}
	return false
}
