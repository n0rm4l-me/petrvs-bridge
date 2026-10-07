// Package spool is a directory where local programs (for example a Claude Code
// hook) drop messages for the agent to send. A file is removed only after the
// message was accepted by the bus, so nothing is lost if the network is down.
package spool

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// MaxFiles bounds how much a runaway writer can queue.
const MaxFiles = 1000

// maxFileBytes bounds what is read back from one file: a body plus the JSON around it. A larger file is not ours.
const maxFileBytes = wire.MaxBody*2 + 4096

// ErrFull is returned when the spool holds MaxFiles messages.
var ErrFull = errors.New("spool: full")

// Item is one queued message. Meta and Buttons are what the hub needs besides the text: the parts of a file say which
// transfer they belong to, a notice may carry buttons.
type Item struct {
	Type    wire.Type         `json:"type"`
	Body    string            `json:"body"`
	Meta    map[string]string `json:"meta,omitempty"`
	Buttons []wire.Button     `json:"buttons,omitempty"`
}

// queueable says which messages the spool holds: what the agent sends to the hub and resends until the hub confirms it.
func queueable(t wire.Type) bool {
	switch t {
	case wire.TypeReply, wire.TypeNotice, wire.TypeAlert, wire.TypePart, wire.TypeFile:
		return true
	}
	return false
}

// FileReserve is how many places a file transfer leaves free for replies: parts and file items are refused once the
// spool holds MaxFiles-FileReserve messages, so that a big transfer cannot keep the answers of the assistant out.
const FileReserve = 200

// Spool is a directory of pending messages.
type Spool struct {
	dir string
	log *slog.Logger
}

// SetLog says where the spool reports a file it sets aside; the default logger is used until then.
func (s *Spool) SetLog(l *slog.Logger) { s.log = l }

func (s *Spool) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// Open creates the directory (owner-only) if needed.
func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Spool{dir: dir}, nil
}

func (s *Spool) files() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Add queues a reply or notice.
func (s *Spool) Add(t wire.Type, body string) error { return s.AddItem(Item{Type: t, Body: body}) }

// AddItem queues a message with its metadata and buttons.
func (s *Spool) AddItem(it Item) error {
	t, body := it.Type, it.Body
	if !queueable(t) {
		return fmt.Errorf("spool: type %q cannot be queued", t)
	}
	if len(body) > wire.MaxBody {
		return errors.New("spool: body too large")
	}
	if len(it.Meta) > 16 || len(it.Buttons) > wire.MaxButtons {
		return errors.New("spool: too much metadata")
	}
	if err := valid(it, "sp-x"); err != nil {
		return err
	}
	names, err := s.files()
	if err != nil {
		return err
	}
	if len(names) >= MaxFiles || ((t == wire.TypePart || t == wire.TypeFile) && len(names) >= MaxFiles-FileReserve) {
		return ErrFull
	}
	// No HTML escaping: it would turn each "<", ">" and "&" of a reply full of code into six bytes for nothing.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(it); err != nil {
		return err
	}
	data := bytes.TrimRight(buf.Bytes(), "\n")
	// Control bytes still grow to six bytes each when escaped, and read() sets aside any file above maxFileBytes: say so
	// now, where the hook sees it, instead of accepting a message that is thrown away later.
	if len(data) > maxFileBytes {
		return errors.New("spool: body too large once escaped for storage")
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // without it a power cut can leave the renamed file empty
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	var salt [4]byte
	rand.Read(salt[:])
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, fmt.Sprintf("%020d-%d-%s.json", time.Now().UnixNano(), os.Getpid(), hex.EncodeToString(salt[:])))); err != nil {
		return err
	}
	syncDir(s.dir)
	return nil
}

// valid applies the rules of the hub to a message as the agent would send it. The hub drops a message that breaks them,
// and the agent only sees its receipt: a message that cannot pass would be sent again every receipt timeout for ever,
// each copy taking a place in the window of unconfirmed messages. The time and the expiry are the agent's own and fit by
// construction; the id only has to be there.
func valid(it Item, id string) error {
	p := &wire.Payload{ID: id, Type: it.Type, Body: it.Body, Meta: it.Meta, Buttons: it.Buttons}
	if err := p.Validate(true); err != nil {
		return fmt.Errorf("spool: the hub would refuse this message: %w", err)
	}
	return nil
}

// syncDir makes a rename survive a power cut. Best effort: not every filesystem lets a directory be synced.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// read loads one file, setting aside (as .bad) anything too large or malformed. ok is false for such files.
func (s *Spool) read(name string) (it Item, ok bool, err error) {
	path := filepath.Join(s.dir, name)
	st, err := os.Stat(path)
	if err != nil {
		return it, false, err
	}
	if st.Size() > maxFileBytes {
		os.Rename(path, path+".bad")
		return it, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return it, false, err
	}
	if json.Unmarshal(data, &it) != nil || !queueable(it.Type) || len(it.Body) > wire.MaxBody || len(it.Meta) > 16 || len(it.Buttons) > wire.MaxButtons {
		os.Rename(path, path+".bad")
		return it, false, nil
	}
	// A file written by something else, or by an older program, may break what the hub takes (a label of 41 bytes, say).
	// It is set aside like any bad file: left in the queue it would hold a place in the window for ever.
	if err := valid(it, "sp-"+name); err != nil {
		s.logger().Warn("set a queued message aside: the hub would refuse it", "file", name, "why", err)
		os.Rename(path, path+".bad")
		return Item{}, false, nil
	}
	return it, true, nil
}

// Names lists the queued files, oldest first, without reading them.
func (s *Spool) Names() ([]string, error) { return s.files() }

// Read loads one queued message by the name List or Names gave. ok is false for a file that is gone or was unreadable
// (an unreadable one is set aside as .bad).
func (s *Spool) Read(name string) (it Item, ok bool, err error) {
	it, ok, err = s.read(name)
	if errors.Is(err, os.ErrNotExist) {
		return it, false, nil
	}
	return it, ok, err
}

// Entry is a queued message with the name of its file.
type Entry struct {
	Name string
	Item Item
}

// List returns the queued messages oldest first without removing them. Unreadable files are set aside.
func (s *Spool) List() ([]Entry, error) {
	names, err := s.files()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, n := range names {
		it, ok, err := s.read(n)
		if err != nil || !ok {
			continue
		}
		out = append(out, Entry{Name: n, Item: it})
	}
	return out, nil
}

// Remove deletes a queued message once its delivery is confirmed.
func (s *Spool) Remove(name string) error {
	if name != filepath.Base(name) || !strings.HasSuffix(name, ".json") {
		return errors.New("spool: bad name")
	}
	err := os.Remove(filepath.Join(s.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Len is the number of queued messages.
func (s *Spool) Len() int {
	names, _ := s.files()
	return len(names)
}

// Drain publishes queued messages oldest first, deleting each after publish succeeds.
// It stops at the first failure so order is kept, and returns how many went out.
// Files that cannot be parsed are moved aside as .bad so they do not block the queue.
func (s *Spool) Drain(ctx context.Context, publish func(context.Context, Item) error) (int, error) {
	names, err := s.files()
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return sent, err
		}
		path := filepath.Join(s.dir, n)
		it, ok, err := s.read(n)
		if err != nil {
			return sent, err
		}
		if !ok {
			continue
		}
		if err := publish(ctx, it); err != nil {
			return sent, err
		}
		if err := os.Remove(path); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}
