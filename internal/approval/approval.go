// Package approval checks one-time codes (TOTP, RFC 6238) and ties an approval
// to the exact command it was given for. It runs on the agent, with a secret the
// hub never has, so a compromised hub cannot approve anything.
package approval

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 authenticators use HMAC-SHA1
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	Period = 30 * time.Second
	Digits = 6
)

var (
	ErrBadCode   = errors.New("approval: wrong code")
	ErrReused    = errors.New("approval: code already used")
	ErrLocked    = errors.New("approval: too many failures, locked")
	ErrNoSuch    = errors.New("approval: no such challenge")
	ErrExpired   = errors.New("approval: challenge expired")
	ErrNotForYou = errors.New("approval: challenge belongs to another command")
	ErrState     = errors.New("approval: cannot record the use of the code, so it is refused")
)

// GenerateSecret returns a new random secret and its base32 form for an authenticator app.
func GenerateSecret() (secret []byte, b32 string, err error) {
	secret = make([]byte, 20)
	if _, err = rand.Read(secret); err != nil {
		return nil, "", err
	}
	return secret, base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret), nil
}

// DecodeSecret parses the base32 form (spaces and case ignored).
func DecodeSecret(b32 string) ([]byte, error) {
	b32 = strings.ToUpper(strings.ReplaceAll(b32, " ", ""))
	s, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b32)
	if err != nil || len(s) < 16 {
		return nil, errors.New("approval: bad secret")
	}
	return s, nil
}

func step(t time.Time) int64 { return t.Unix() / int64(Period/time.Second) }

func codeForStep(secret []byte, counter int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// Code returns the current code for the secret.
func Code(secret []byte, t time.Time) string { return codeForStep(secret, step(t), Digits) }

// Verifier accepts each code once, allows one step of clock drift either way,
// and locks after repeated failures.
type Verifier struct {
	secret   []byte
	MaxFails int
	Lockout  time.Duration

	mu          sync.Mutex
	lastStep    int64
	fails       int
	lockedUntil time.Time
	path        string // where lastStep and lockedUntil are kept, if anywhere
}

type verifierState struct {
	LastStep    int64 `json:"last_step"`
	LockedUntil int64 `json:"locked_until,omitempty"`
}

// Persist keeps the verifier's memory of used codes and of a lockout in a file (0600), so that restarting the program
// does not make a code that was already used, or a lockout that was already earned, disappear. A missing file is a
// fresh start; a file that cannot be parsed makes the verifier refuse the current and the previous time step, which
// costs the user at most a minute of waiting. After Persist, a code whose use cannot be recorded is refused.
func (v *Verifier) Persist(path string, now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.path = path
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	var st verifierState
	if json.Unmarshal(data, &st) != nil {
		v.lastStep = step(now) + 1
		return nil
	}
	v.lastStep = st.LastStep
	if st.LockedUntil > 0 {
		v.lockedUntil = time.Unix(st.LockedUntil, 0)
	}
	return nil
}

func (v *Verifier) save() error {
	if v.path == "" {
		return nil
	}
	st := verifierState{LastStep: v.lastStep}
	if !v.lockedUntil.IsZero() {
		st.LockedUntil = v.lockedUntil.Unix()
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(v.path), ".tmp-approval-")
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
	return os.Rename(tmp.Name(), v.path)
}

// NewVerifier returns a verifier with 5 failures allowed and a 5 minute lockout.
func NewVerifier(secret []byte) *Verifier {
	return &Verifier{secret: append([]byte(nil), secret...), MaxFails: 5, Lockout: 5 * time.Minute, lastStep: -1}
}

// Verify checks a code typed by the user.
func (v *Verifier) Verify(code string, now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if now.Before(v.lockedUntil) {
		return ErrLocked
	}
	code = strings.ReplaceAll(code, " ", "")
	cur := step(now)
	for _, s := range []int64{cur, cur - 1, cur + 1} {
		want := codeForStep(v.secret, s, Digits)
		if len(code) == len(want) && subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1 {
			if s <= v.lastStep {
				return ErrReused
			}
			prev := v.lastStep
			v.lastStep = s
			if err := v.save(); err != nil {
				v.lastStep = prev
				return ErrState
			}
			v.fails = 0
			return nil
		}
	}
	v.fails++
	if v.fails >= v.MaxFails {
		v.lockedUntil = now.Add(v.Lockout)
		v.fails = 0
		v.save() // best effort: the lockout still holds in memory
		return ErrLocked
	}
	return ErrBadCode
}

// Challenge asks the user to approve one specific command.
type Challenge struct {
	ID      string
	Hash    string
	Summary string
	Expires time.Time
}

// Gate issues challenges and accepts an approval only for the command it was issued for.
type Gate struct {
	v   *Verifier
	TTL time.Duration

	mu      sync.Mutex
	pending map[string]Challenge
}

// NewGate wraps a verifier. Challenges live 60 seconds by default.
func NewGate(v *Verifier) *Gate {
	return &Gate{v: v, TTL: time.Minute, pending: map[string]Challenge{}}
}

// Issue creates a challenge for a command identified by its hash.
func (g *Gate) Issue(hash, summary string, now time.Time) Challenge {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, c := range g.pending {
		if now.After(c.Expires) {
			delete(g.pending, id)
		}
	}
	var b [8]byte
	rand.Read(b[:])
	c := Challenge{ID: hex.EncodeToString(b[:]), Hash: hash, Summary: summary, Expires: now.Add(g.TTL)}
	g.pending[c.ID] = c
	return c
}

// Approve checks the code for a challenge and, on success, returns the approved command hash.
// A challenge can be approved once; a wrong code does not consume it.
func (g *Gate) Approve(id, code string, now time.Time) (string, error) {
	g.mu.Lock()
	c, ok := g.pending[id]
	g.mu.Unlock()
	if !ok {
		return "", ErrNoSuch
	}
	if now.After(c.Expires) {
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return "", ErrExpired
	}
	if err := g.v.Verify(code, now); err != nil {
		return "", err
	}
	g.mu.Lock()
	delete(g.pending, id)
	g.mu.Unlock()
	return c.Hash, nil
}

// Check is used by the enforcing hook: it accepts only an approval for exactly this command hash.
func Check(approvedHash, wantHash string) error {
	if subtle.ConstantTimeCompare([]byte(approvedHash), []byte(wantHash)) != 1 {
		return ErrNotForYou
	}
	return nil
}
