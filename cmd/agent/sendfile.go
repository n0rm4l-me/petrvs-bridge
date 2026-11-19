package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/config"
	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

const (
	// sfUsage is printed when the arguments do not fit.
	sfUsage = "usage: agent send-file [-caption TEXT] [-voice] PATH"
	// sfCaptionBytes is how much of the caption is sent; Telegram shows at most 1024 characters, with the agent's tag.
	sfCaptionBytes = 1000
	// A looping assistant must not flood the chat: this many files per window for one state directory.
	sfRateFiles  = 10
	sfRateWindow = time.Minute
	// sfSniffBytes is how much of the start of a file is looked at for a private key.
	sfSniffBytes = 64 << 10
)

// sfSendFile queues a file for the hub to show in the chat: the parts go into the spool, then the item that closes the
// transfer. Like `send` it needs only the state directory, so a Claude Code hook (or the assistant itself, through its
// shell) can run it. Exit codes: 0 queued, 1 refused, 2 usage.
func sfSendFile(args []string, getenv func(string) string) int {
	fs := flag.NewFlagSet("send-file", flag.ContinueOnError)
	caption := fs.String("caption", "", "text shown with the file (at most 1000 bytes)")
	voice := fs.Bool("voice", false, "send an audio file (ogg with Opus, mp3 or m4a) as a voice message")
	// Flags may also follow the path.
	var path string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(os.Stderr, sfUsage)
				return 0
			}
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		if path != "" {
			fmt.Fprintln(os.Stderr, sfUsage)
			return 2
		}
		path, rest = rest[0], rest[1:]
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, sfUsage)
		return 2
	}

	maxBytes := int64(config.DefaultMaxFileBytes)
	if v := getenv("PB_MAX_FILE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > config.MaxFileLimit {
			fmt.Fprintf(os.Stderr, "error: PB_MAX_FILE_BYTES must be a number from 0 (the default, %d) to %d\n", int64(config.DefaultMaxFileBytes), int64(config.MaxFileLimit))
			return 2
		}
		if n > 0 {
			maxBytes = n
		}
	}
	dir := getenv("PB_STATE_DIR")
	if dir == "" {
		cfg, code := parse(nil, localEnv(getenv))
		if cfg == nil {
			return code
		}
		dir = cfg.StateDir
	}

	data, name, err := sfRead(path, dir, maxBytes, getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *voice && !filetx.IsVoice(name) {
		fmt.Fprintf(os.Stderr, "error: %s is not a format that Telegram plays as a voice message (ogg with Opus, mp3 or m4a)\n", name)
		return 1
	}
	sp, err := spool.Open(filepath.Join(dir, "spool"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	chunks := filetx.Chunks(data)
	if sp.Len()+len(chunks)+1 > spool.MaxFiles-spool.FileReserve {
		fmt.Fprintf(os.Stderr, "error: the queue is full (%d messages wait for the hub)\n", sp.Len())
		return 1
	}
	if err := sfTakeSlot(dir, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := sfQueue(sp, filepath.Join(dir, "spool"), data, chunks, name, sfCaption(*caption), *voice); err != nil {
		if errors.Is(err, spool.ErrFull) {
			fmt.Fprintln(os.Stderr, "error: the queue is full")
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		return 1
	}
	fmt.Printf("queued %s: %d bytes in %d parts\n", name, len(data), len(chunks))
	return 0
}

// sfRead checks that the file may go to the chat and reads it. The checks look at what the path really is: a
// symbolic link named like a photo that points at a key is the key.
//
// The path is checked twice. Before anything is opened it is checked as written and with links resolved, which gives
// the clear refusals early. But a path can change between the check and the open: O_NOFOLLOW covers only the last
// component, and a parent directory swapped between a real directory and a link to ~/.aws is read through on the open.
// So the file is opened first and the checks are repeated on the open descriptor: on the path the system reports for it
// and on its own identity. What is read is that descriptor, so what was judged is what is sent. (The directories above
// the file are compared by asking for them by name a moment after the path was read, which someone would have to
// rename them in that moment to fool; the guard is against accidents, and this is not where they happen.)
func sfRead(path, stateDir string, maxBytes int64, getenv func(string) string) ([]byte, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read %s", filepath.Base(abs))
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read %s", filepath.Base(abs))
	}
	if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file (a directory, a socket, a device?)", filepath.Base(abs))
	}
	// The agent's own directories and files are refused under every spelling of their names.
	own := sfOwnPlaces(stateDir, getenv)
	for _, p := range []string{abs, resolved} {
		if err := own.named(p); err != nil {
			return nil, "", err
		}
	}
	// Opened without following a link or waiting on a pipe.
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read %s", filepath.Base(abs))
	}
	defer f.Close()
	if fi, err = f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", filepath.Base(abs))
	}
	// Where the descriptor really points, whatever the path looked like a moment ago. A file that is not refused here is
	// the file that is read below.
	real, err := fdPath(f, resolved)
	if err != nil {
		// Nothing can be judged without it, so nothing is sent (on Linux this needs /proc).
		return nil, "", fmt.Errorf("cannot read %s: the system does not say where the open file is (%v)", filepath.Base(abs), err)
	}
	if err := own.named(real); err != nil {
		return nil, "", err
	}
	if err := own.identity(real, fi); err != nil {
		return nil, "", err
	}
	if fi.Size() == 0 {
		return nil, "", fmt.Errorf("%s is empty", filepath.Base(abs))
	}
	if fi.Size() > maxBytes {
		return nil, "", fmt.Errorf("%s is %d bytes; the most that can be sent is %d (PB_MAX_FILE_BYTES)", filepath.Base(abs), fi.Size(), maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("cannot read %s", filepath.Base(abs))
	}
	switch {
	case len(data) == 0:
		return nil, "", fmt.Errorf("%s is empty", filepath.Base(abs))
	case int64(len(data)) > maxBytes:
		return nil, "", fmt.Errorf("%s is larger than %d bytes (PB_MAX_FILE_BYTES)", filepath.Base(abs), maxBytes)
	}
	if what := sfSecretInside(data); what != "" {
		return nil, "", fmt.Errorf("%s contains %s; copy out the part you mean to send", filepath.Base(abs), what)
	}
	return data, filetx.SafeName(filepath.Base(abs)), nil
}

// sfSecretMarks are strings that only a secret contains. A file called notes.txt that holds a private key is still a private
// key; the names in filetx.Denied do not see that, so the start of the file is read for these as well. It is a guard
// against accidents (the assistant told to send everything in a folder), not against someone who hides a secret on purpose.
var sfSecretMarks = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`), "a private key"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "an AWS access key"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`), "a GitHub token"},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), "a Slack token"},
	{regexp.MustCompile(`\b[0-9]{8,10}:AA[A-Za-z0-9_-]{33}\b`), "a Telegram bot token"},
	// the identity file of the bridge: name, static key, signing key
	{regexp.MustCompile(`(?s)"static"\s*:.*"sign"\s*:|"sign"\s*:.*"static"\s*:`), "a key file of the bridge"},
}

// sfSecretInside says what secret the start of a file holds, or "" if it looks like none.
func sfSecretInside(data []byte) string {
	if len(data) > sfSniffBytes {
		data = data[:sfSniffBytes]
	}
	for _, m := range sfSecretMarks {
		if m.re.Match(data) {
			return m.what
		}
	}
	return ""
}

// sfCaption keeps a caption to what is sent, on a character boundary.
func sfCaption(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
	if len(s) <= sfCaptionBytes {
		return s
	}
	cut := sfCaptionBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// sfTakeSlot records one more file for this state directory, or says that the limit for the last minute is reached. The
// times are kept in a small file in the state directory, under a lock, so that several hooks at once are counted
// together. A file that cannot be used stops the send: the limit is there to protect the chat.
func sfTakeSlot(dir string, clock func() time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "send-file-times"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	now := clock() // read under the lock: a file counted by someone who held it before is not from the future
	raw, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return err
	}
	var keep []int64
	for _, field := range strings.Fields(string(raw)) {
		ns, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			continue
		}
		// Times from the future (the clock was set back) are not counted either.
		if age := now.UnixNano() - ns; age >= 0 && age < int64(sfRateWindow) {
			keep = append(keep, ns)
		}
	}
	if len(keep) >= sfRateFiles {
		wait := time.Duration(keep[0]) + sfRateWindow - time.Duration(now.UnixNano())
		return fmt.Errorf("too many files: at most %d a minute; try again in %d s", sfRateFiles, int(wait.Seconds())+1)
	}
	keep = append(keep, now.UnixNano())
	var out strings.Builder
	for _, ns := range keep {
		out.WriteString(strconv.FormatInt(ns, 10))
		out.WriteByte('\n')
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt([]byte(out.String()), 0)
	return err
}

// sfQueue puts the parts and then the closing item into the spool. If anything fails, the items this call added are
// taken out again, so that half a file does not sit in the queue. voice asks the chat to play the file as a voice message.
func sfQueue(sp *spool.Spool, spoolDir string, data []byte, chunks [][]byte, name, caption string, voice bool) (err error) {
	t := filetx.NewID()
	kind := filetx.Kind(name, int64(len(data)))
	if voice {
		kind = "voice"
	}
	before := sfSpoolNames(spoolDir)
	defer func() {
		if err != nil {
			sfRemoveTransfer(sp, spoolDir, before, t)
		}
	}()
	for i, c := range chunks {
		it := spool.Item{Type: wire.TypePart, Body: filetx.EncodePart(c), Meta: map[string]string{wire.MetaTransfer: t, wire.MetaIndex: strconv.Itoa(i)}}
		if err := sp.AddItem(it); err != nil {
			return err
		}
	}
	return sp.AddItem(spool.Item{Type: wire.TypeFile, Body: caption, Meta: map[string]string{
		wire.MetaTransfer: t, wire.MetaParts: strconv.Itoa(len(chunks)), wire.MetaName: name,
		wire.MetaSize: strconv.Itoa(len(data)), wire.MetaSHA: filetx.SHA(data), wire.MetaKind: kind,
	}})
}

func sfSpoolNames(dir string) map[string]bool {
	out := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// sfRemoveTransfer deletes the spool files that appeared since before and belong to transfer t. Other writers' files (a
// reply queued by a hook at the same moment) are not touched.
func sfRemoveTransfer(sp *spool.Spool, dir string, before map[string]bool, t string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if before[e.Name()] || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var it struct {
			Meta map[string]string `json:"meta"`
		}
		if json.Unmarshal(raw, &it) == nil && it.Meta[wire.MetaTransfer] == t {
			sp.Remove(e.Name())
		}
	}
}
