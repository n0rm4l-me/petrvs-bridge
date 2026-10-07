package agentsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/n0rm4l-me/petrvs-bridge/internal/tmuxctl"
)

// The inbox is the directory where files from the chat are saved. Only this code writes to it, and only new files whose
// names it made itself; nothing in it is executed or interpreted.

const (
	// The inbox is tidied at most once an hour, or after this many new files, so that a burst of files cannot fill it
	// between two passes.
	tidyEvery      = time.Hour
	tidyAfterSaves = 100
	inboxMaxAge    = 14 * 24 * time.Hour
	inboxMaxFiles  = 200
	// maxTidyEntries bounds how much of a huge directory one pass reads.
	maxTidyEntries = 10000
	// maxCaptionRunes is how much of the caption is typed into the session.
	maxCaptionRunes = 500
	// zeroWidthJoiner is the one invisible character the caption keeps: emoji sequences (a family, a flag) need it.
	zeroWidthJoiner = '\u200d'
)

var errInboxLink = &userError{"refused: the inbox directory is a link; set a real directory as the inbox"}

// inboxFile matches the names saveInbox makes (date, time, start of the transfer id). Tidying touches nothing else, so an
// inbox that is a directory of the user's own, or has files of their own in it, loses none of them.
var inboxFile = regexp.MustCompile(`^\d{8}-\d{6}-[0-9A-Za-z]{6}-`)

// inboxName makes the part of the file name that comes from the chat: letters, digits, dot, underscore and hyphen, so that
// the path can be typed into a line without quoting.
func inboxName(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
}

// saveInbox writes data to a new file in dir and returns its absolute path. It never overwrites and never follows a link:
// the file is created with O_EXCL (which also refuses a link in the place of the name), mode 0600. The directory itself
// may lie below a link (/tmp is one on some systems) but may not be one, since a link put there would send the files, and
// the tidying, somewhere else. A directory that this call creates gets a .gitignore (see keepOutOfGit).
func saveInbox(dir, name, transfer string, data []byte, now time.Time) (string, error) {
	_, err := os.Lstat(dir)
	fresh := errors.Is(err, os.ErrNotExist)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	switch {
	case err != nil:
		return "", err
	case fi.Mode()&os.ModeSymlink != 0:
		return "", errInboxLink
	case !fi.IsDir():
		return "", errors.New("the inbox is not a directory")
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if fresh {
		keepOutOfGit(real)
	}
	path := filepath.Join(real, now.Format("20060102-150405")+"-"+transfer[:6]+"-"+inboxName(name))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path) // ours: O_EXCL made it just now
		return "", err
	}
	return path, nil
}

// keepOutOfGit puts a .gitignore that ignores everything into a new inbox. The default inbox lies in the project
// (<workdir>/.petrvs-inbox), where the assistant's `git add -A` would commit the files that came from the chat. An existing
// .gitignore is left as it is, and so is an inbox that was there before: it may be a directory of the user's own. It is
// a convenience, so a failure to write it does not stop the file from being saved.
func keepOutOfGit(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, ".gitignore"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
	if err != nil {
		return
	}
	f.WriteString("*\n")
	f.Close()
}

// tidyInbox removes what is old or beyond the newest inboxMaxFiles, among the files this code made. It reads at most
// maxTidyEntries entries.
func tidyInbox(dir string, now time.Time) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() { // a link is not followed: see saveInbox
		return errInboxLink
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(maxTidyEntries)
	d.Close()
	if err != nil && len(entries) == 0 {
		return err
	}
	type kept struct {
		name string
		mod  time.Time
	}
	var files []kept
	for _, e := range entries {
		if !e.Type().IsRegular() || !inboxFile.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, kept{e.Name(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for i, f := range files {
		if now.Sub(f.mod) > inboxMaxAge || len(files)-i > inboxMaxFiles {
			os.Remove(filepath.Join(dir, f.name))
		}
	}
	return nil
}

func kindWord(photo bool) string {
	if photo {
		return "photo"
	}
	return "file"
}

// fileLine is the line typed into the session to tell it where a file from the chat is. The path has only safe
// characters (unless the inbox the user chose has odd ones); the caption is the user's, cleaned (see cleanCaption).
func fileLine(photo bool, path, name string, size int, caption string) string {
	shown := path
	if strings.ContainsFunc(path, unicode.IsSpace) {
		shown = `"` + path + `"`
	}
	line := fmt.Sprintf("[%s from the chat, saved at %s (%s, %s)]", kindWord(photo), shown, extLabel(name), humanSize(size))
	if c := cleanCaption(caption); c != "" {
		line += " " + c
	}
	if tmuxctl.ValidateText(line) != nil && caption != "" {
		return fileLine(photo, path, name, size, "")
	}
	return line
}

// extLabel is the kind of file by its extension, as far as the name tells.
func extLabel(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" {
		return "no extension"
	}
	if len(ext) > 10 || strings.ContainsFunc(ext, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) {
		return "unknown type"
	}
	return ext
}

func humanSize(n int) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return sizeIn(float64(n)/(1<<10), "KB")
	default:
		return sizeIn(float64(n)/(1<<20), "MB")
	}
}

func sizeIn(v float64, unit string) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f %s", v, unit)
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

// cleanCaption makes the caption fit one line that is typed into the session: white space and control characters,
// including line breaks, become single spaces, characters that reorder or hide text (bidirectional marks, zero-width
// spaces) are dropped, invalid UTF-8 is replaced, and at most maxCaptionRunes characters stay.
func cleanCaption(s string) string {
	var b strings.Builder
	n, pendingSpace := 0, false
	for _, r := range strings.ToValidUTF8(s, " ") {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r): // IsSpace covers the line and paragraph separators too
			pendingSpace = b.Len() > 0
			continue
		case unicode.Is(unicode.Cf, r) && r != zeroWidthJoiner:
			continue
		}
		need := 1
		if pendingSpace {
			need = 2
		}
		if n+need > maxCaptionRunes {
			break
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
		n += need
	}
	return b.String()
}
