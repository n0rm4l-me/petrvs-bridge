// Package filetx holds what both directions of the file transfer share: cutting a file into parts that fit a message,
// putting them back together with the checks a stranger's bytes need (size, count, hash, memory, age), a safe name for
// the file, and the refusal to send things that look like secrets.
//
// A transfer is a run of `part` payloads followed by one `file` payload that says how many parts there are, how big the
// whole is and its SHA-256. Nothing is trusted before the whole has been put together and checked.
package filetx

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

const (
	// DefaultMaxBytes bounds a file in either direction unless the settings say otherwise. Telegram allows a bot to send
	// 50 MB and to fetch 20 MB; a file is held whole in memory on its way, so the default stays modest.
	DefaultMaxBytes int64 = 20 << 20
	// PhotoMaxBytes is the largest picture Telegram shows as a photo (sendPhoto); larger ones go as documents.
	PhotoMaxBytes = 10 << 20

	// maxTransfers bounds the transfers an Assembler holds at the same time, and ttl how long an unfinished one is kept. The
	// bytes are what really bounds the memory (twice the largest file); the count has to leave room for the files an agent
	// has queued, whose parts all go out before the first closing item.
	maxTransfers = 16
	ttl          = 10 * time.Minute
	maxNameBytes = 100
)

// NewID returns a token for a transfer: 12 hex characters.
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system has no randomness; nothing sensible can go on
	}
	return hex.EncodeToString(b[:])
}

// SHA returns the hex SHA-256 of data.
func SHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Chunks cuts data into pieces of at most wire.MaxChunk bytes. Empty data gives no chunk.
func Chunks(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := min(len(data), wire.MaxChunk)
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

// MaxParts is how many parts a file of at most maxBytes needs.
func MaxParts(maxBytes int64) int { return int((maxBytes+wire.MaxChunk-1)/wire.MaxChunk) + 1 }

// EncodePart turns a chunk into the Body of a part.
func EncodePart(chunk []byte) string { return base64.StdEncoding.EncodeToString(chunk) }

// DecodePart turns the Body of a part back into bytes, refusing more than wire.MaxChunk.
func DecodePart(body string) ([]byte, error) {
	if base64.StdEncoding.DecodedLen(len(body)) > wire.MaxChunk+2 {
		return nil, errors.New("filetx: part too large")
	}
	b, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, errors.New("filetx: part is not base64")
	}
	if len(b) > wire.MaxChunk {
		return nil, errors.New("filetx: part too large")
	}
	return b, nil
}

// Kind says how the chat should show a file: a picture Telegram can show as a photo, or a document.
func Kind(name string, size int64) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		if size > 0 && size <= PhotoMaxBytes {
			return "photo"
		}
	}
	return "document"
}

// SafeName makes a name fit to create a file with: the last path element only, letters, digits and . _ - + ( ) space,
// no leading dot or space, at most 100 bytes with the extension kept. It never returns an empty name.
func SafeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r), strings.ContainsRune("._-+() ", r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimLeft(b.String(), ". ")
	out = strings.TrimRight(out, " ")
	if len(out) > maxNameBytes {
		ext := path.Ext(out)
		if len(ext) > 16 {
			ext = ""
		}
		stem := out[:len(out)-len(ext)]
		for len(stem)+len(ext) > maxNameBytes || !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		out = stem + ext
	}
	if out == "" || strings.Trim(out, "._- ") == "" {
		return "file"
	}
	return out
}

// deniedNames are file names (matched with path.Match, after Fold) that are refused: keys, tokens, credentials.
var deniedNames = []string{
	".env", ".env.*", "*.env", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.kdbx", "*.keychain*", "*.ovpn",
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*", "*.identity.json", "*.secret", "*secret*", "*credential*",
	"*.token", "*token*.txt", ".netrc", "_netrc", ".pgpass", ".npmrc", ".pypirc", ".git-credentials", "*service-account*.json",
	"*-sa.json", "application_default_credentials.json", "client_secret*.json",
}

// deniedDirs are directory names that make everything below them refused.
var deniedDirs = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", "gcloud", "keychains", ".password-store", ".azure"}

// The two lists folded once, since every name is folded before it is compared.
var foldedNames, foldedDirs = foldAll(deniedNames), foldAll(deniedDirs)

func foldAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = Fold(s)
	}
	return out
}

// Fold maps every letter of s to the smallest member of its Unicode case orbit, so that two spellings which a file system
// that ignores case treats as one name become one string. strings.ToLower is not enough: "S", "s" and the long s U+017F
// are the same letter to APFS and NTFS, as are "K", "k" and the Kelvin sign U+212A, but ToLower sends only two of each
// to the same rune. A pattern folded the same way keeps its meaning, since only letters change.
func Fold(s string) string { return strings.Map(foldRune, s) }

func foldRune(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		m = min(m, f)
	}
	return m
}

// Under says whether p is dir or lies below it, comparing the names folded (see Fold).
func Under(p, dir string) bool {
	fp := Fold(filepath.ToSlash(filepath.Clean(p)))
	fd := Fold(filepath.ToSlash(filepath.Clean(dir)))
	return fp == fd || strings.HasPrefix(fp, strings.TrimSuffix(fd, "/")+"/")
}

// Denied says why a local file must not be sent to the chat, or returns nil. It looks at the name and at every directory
// on the path, so pass the path with symbolic links resolved (a link to ~/.ssh/id_rsa is the key). extraDirs are
// directories to refuse as well, such as the agent's own state and key directories. Names are compared folded (see Fold):
// on a case-sensitive file system that refuses a little more than needed, which is the safe side. The list is a guard
// against accidents, such as the assistant being told to send "everything in this folder"; it does not stop someone who
// renames a file. A caller that can also open the file should compare it by identity (os.SameFile) with what it must not
// send, since no spelling of a name gets around that.
func Denied(p string, extraDirs ...string) error {
	p = filepath.Clean(p)
	folded := Fold(filepath.ToSlash(p))
	for i, pat := range foldedNames {
		if ok, _ := path.Match(pat, path.Base(folded)); ok {
			return fmt.Errorf("filetx: %q looks like a secret (%s); copy it under another name if you really mean to send it", path.Base(filepath.ToSlash(p)), deniedNames[i])
		}
	}
	for _, part := range strings.Split(path.Dir(folded), "/") {
		for i, d := range foldedDirs {
			if part == d {
				return fmt.Errorf("filetx: %q is inside %s, where secrets live", filepath.Base(p), deniedDirs[i])
			}
		}
	}
	for _, d := range extraDirs {
		if d == "" {
			continue
		}
		if Under(p, d) {
			return fmt.Errorf("filetx: %q is inside %s, which belongs to the bridge", filepath.Base(p), filepath.Clean(d))
		}
	}
	return nil
}

// MissingError says that parts of a transfer have not arrived.
type MissingError struct{ Have, Want int }

func (e *MissingError) Error() string {
	return fmt.Sprintf("filetx: %d of %d parts have arrived", e.Have, e.Want)
}

// Errors of Complete besides MissingError.
var (
	ErrTooLarge = errors.New("filetx: file too large")
	ErrSize     = errors.New("filetx: the parts do not add up to the announced size")
	ErrHash     = errors.New("filetx: the hash does not match")
	ErrUnknown  = errors.New("filetx: no parts of this transfer")
)

// Errors of AddPart besides ErrTooLarge. A bad part will be refused however often it comes; ErrBusy says only that the
// Assembler holds as many transfers as it takes now, so the part may be taken later.
var (
	ErrBadPart = errors.New("filetx: bad part")
	ErrBusy    = errors.New("filetx: too many transfers at once")
)

type transfer struct {
	parts map[int][]byte
	bytes int64
	seen  time.Time
}

// Assembler collects the parts of transfers. It is safe for concurrent use. It holds at most four unfinished transfers
// and twice MaxBytes in total, forgets a transfer that saw no part for ten minutes, and refuses a part that does not fit:
// whoever can publish parts gets no more memory out of the receiver than that.
type Assembler struct {
	mu        sync.Mutex
	maxBytes  int64
	maxParts  int
	total     int64
	transfers map[string]*transfer
}

// NewAssembler returns an Assembler for files of at most maxBytes.
func NewAssembler(maxBytes int64) *Assembler {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Assembler{maxBytes: maxBytes, maxParts: MaxParts(maxBytes), transfers: map[string]*transfer{}}
}

func (a *Assembler) expire(now time.Time) {
	for id, t := range a.transfers {
		if now.Sub(t.seen) > ttl {
			a.total -= t.bytes
			delete(a.transfers, id)
		}
	}
}

// AddPart stores one part. The same part again is accepted and ignored; a different part under the same index is refused.
func (a *Assembler) AddPart(id string, index int, data []byte, now time.Time) error {
	if id == "" || len(id) > 32 || index < 0 || index >= a.maxParts || len(data) == 0 || len(data) > wire.MaxChunk {
		return ErrBadPart
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(now)
	t := a.transfers[id]
	if t == nil {
		if len(a.transfers) >= maxTransfers {
			return ErrBusy
		}
		t = &transfer{parts: map[int][]byte{}}
		a.transfers[id] = t
	}
	t.seen = now
	if old, ok := t.parts[index]; ok {
		if string(old) != string(data) {
			return fmt.Errorf("%w: a different part under the same index", ErrBadPart)
		}
		return nil
	}
	if t.bytes+int64(len(data)) > a.maxBytes || a.total+int64(len(data)) > 2*a.maxBytes {
		return ErrTooLarge
	}
	t.parts[index] = append([]byte(nil), data...)
	t.bytes += int64(len(data))
	a.total += int64(len(data))
	return nil
}

// Complete puts the parts of a transfer together and checks them against what the sender announced. On success the
// transfer is forgotten. When parts are missing it returns a *MissingError and keeps what it has, since late parts may
// still come; any other error forgets the transfer.
func (a *Assembler) Complete(id string, parts int, size int64, sha string, now time.Time) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(now)
	t := a.transfers[id]
	if t == nil {
		return nil, ErrUnknown
	}
	forget := func() {
		a.total -= t.bytes
		delete(a.transfers, id)
	}
	if parts <= 0 || parts > a.maxParts || size <= 0 || size > a.maxBytes {
		forget()
		return nil, ErrTooLarge
	}
	if len(t.parts) < parts {
		return nil, &MissingError{Have: len(t.parts), Want: parts}
	}
	out := make([]byte, 0, size)
	for i := 0; i < parts; i++ {
		p, ok := t.parts[i]
		if !ok {
			return nil, &MissingError{Have: len(t.parts), Want: parts}
		}
		out = append(out, p...)
	}
	forget()
	if int64(len(out)) != size || len(t.parts) != parts {
		return nil, ErrSize
	}
	if !strings.EqualFold(SHA(out), sha) {
		return nil, ErrHash
	}
	return out, nil
}

// Forget drops a transfer.
func (a *Assembler) Forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.transfers[id]; t != nil {
		a.total -= t.bytes
		delete(a.transfers, id)
	}
}

// Held reports how many transfers and bytes are being held.
func (a *Assembler) Held() (transfers int, bytes int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.transfers), a.total
}
