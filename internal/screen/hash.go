package screen

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// The hash of a prompt binds a button to the dialog it was made for. It is keyed, so that a hash shown next to the hidden
// text of a dialog (the chat sees both) tells nothing about a secret in it: without the key nobody can check a guess.

const (
	keyFile  = "prompt-key"
	keyBytes = 32
	// hashChars is the length of a hash in hex characters: 64 bits, which the wire format allows (a button carries at
	// most 16 bytes) and which Telegram's 64 bytes of callback data can hold next to an agent name.
	hashChars = 16
)

// hashKey is the key of the install; nil means the hashes are not keyed. It is read on every hash and set once at start.
var hashKey atomic.Pointer[[]byte]

// SetHashKey sets the key that hashes are made with, for every later call of ExtractPrompt. It is meant to be called once,
// at start, with what LoadKey returned; nil or empty means unkeyed, which is what tests use. Hooks and the agent of one
// install must use the same key, or the agent refuses the buttons that a hook made.
func SetHashKey(key []byte) {
	if len(key) == 0 {
		hashKey.Store(nil)
		return
	}
	k := append([]byte(nil), key...)
	hashKey.Store(&k)
}

// LoadKey reads the install's key from dir, creating it on first use: 32 random bytes in dir/prompt-key, readable by the
// owner only. Two processes that start together (the agent and a hook) end up with the same key: the file is written
// whole under a temporary name and then linked into place, which fails for the one that comes second, and that one reads
// what the first wrote.
func LoadKey(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, keyFile)
	key, err := readKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	key = make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".prompt-key-*") // mode 0600
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(key)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readKey(path) // the other process was first
		}
		return nil, err
	}
	return key, nil
}

// readKey reads the key file; fs.ErrNotExist when there is none. Anything but a regular file of the right size is an error:
// a key that cannot be trusted is not replaced behind the back of the process that uses it.
func readKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != keyBytes {
		return nil, fmt.Errorf("screen: %s is not a %d-byte file", path, keyBytes)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != keyBytes {
		return nil, fmt.Errorf("screen: %s changed while it was read", path)
	}
	return key, nil
}

// hashOf is the hash of a dialog, given as the lines as drawn (blank lines included, the decoration left out).
//
// What is hashed is what the user approves, so almost nothing is left out: the lines of the command's frame (they start
// with the box bar) are taken as they are, and so are blank lines and the other lines; a change in indentation, a blank
// line or a line that looks like a tip makes another command, and the button of the old one must not answer it. Only what
// moves without the dialog changing is normalised: the highlight and the indentation of the numbered choices, and the
// rotating tip line, which is not in the frame.
func hashOf(ls []string) string {
	var h hash.Hash = sha256.New()
	if k := hashKey.Load(); k != nil {
		h = hmac.New(sha256.New, *k)
	}
	for _, l := range trimBlank(ls) {
		switch {
		case boxRe.MatchString(l): // as drawn
		case tipLineRe.MatchString(l):
			continue
		case optionRe.MatchString(l):
			l = strings.TrimSpace(cursorRe.ReplaceAllString(l, ""))
		}
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:hashChars]
}

// trimBlank drops the blank lines at both ends: the bottom of a capture is blank down to the end of the pane, and how
// much of that is there depends on the size of the window, not on the dialog.
func trimBlank(ls []string) []string {
	for len(ls) > 0 && ls[0] == "" {
		ls = ls[1:]
	}
	for len(ls) > 0 && ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}
