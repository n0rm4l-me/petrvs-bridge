// Package spool is a directory where local programs (for example a Claude Code
// hook) drop messages for the agent to send. A file is removed only after the
// message was accepted by the bus, so nothing is lost if the network is down.
package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"claudebridge/internal/wire"
)

// MaxFiles bounds how much a runaway writer can queue.
const MaxFiles = 1000

// ErrFull is returned when the spool holds MaxFiles messages.
var ErrFull = errors.New("spool: full")

// Item is one queued message.
type Item struct {
	Type wire.Type `json:"type"`
	Body string    `json:"body"`
}

// Spool is a directory of pending messages.
type Spool struct{ dir string }

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
func (s *Spool) Add(t wire.Type, body string) error {
	if t != wire.TypeReply && t != wire.TypeNotice {
		return fmt.Errorf("spool: type %q cannot be queued", t)
	}
	if len(body) > wire.MaxBody {
		return errors.New("spool: body too large")
	}
	names, err := s.files()
	if err != nil {
		return err
	}
	if len(names) >= MaxFiles {
		return ErrFull
	}
	data, err := json.Marshal(Item{Type: t, Body: body})
	if err != nil {
		return err
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, fmt.Sprintf("%020d-%d.json", time.Now().UnixNano(), os.Getpid())))
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
		data, err := os.ReadFile(path)
		if err != nil {
			return sent, err
		}
		var it Item
		if json.Unmarshal(data, &it) != nil || (it.Type != wire.TypeReply && it.Type != wire.TypeNotice) {
			os.Rename(path, path+".bad")
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
