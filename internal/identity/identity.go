// Package identity holds the long-term keys of a party and the public bundle
// another party pins at enrolment.
//
// A party has two keys: an X25519 key used in the handshake key agreement and
// an Ed25519 key used to sign handshake messages. They are never used to
// encrypt traffic, so stealing them allows impersonation in new handshakes but
// reveals no past traffic.
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidName reports whether s is an acceptable party name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Identity is a party's private key material.
type Identity struct {
	Name   string
	Static *ecdh.PrivateKey
	Sign   ed25519.PrivateKey
}

// Public is the part another party pins.
type Public struct {
	Name   string `json:"name"`
	Static []byte `json:"static"`
	Sign   []byte `json:"sign"`
}

// Generate creates a fresh identity.
func Generate(name string) (*Identity, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("identity: invalid name %q (lower-case letters, digits, - and _, up to 32)", name)
	}
	static, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{Name: name, Static: static, Sign: sign}, nil
}

// Public returns the shareable half.
func (i *Identity) Public() Public {
	return Public{Name: i.Name, Static: i.Static.PublicKey().Bytes(), Sign: i.Sign.Public().(ed25519.PublicKey)}
}

// Validate checks the shape of a pinned bundle.
func (p Public) Validate() error {
	if !ValidName(p.Name) {
		return errors.New("identity: invalid name")
	}
	if _, err := ecdh.X25519().NewPublicKey(p.Static); err != nil {
		return errors.New("identity: invalid static key")
	}
	if len(p.Sign) != ed25519.PublicKeySize {
		return errors.New("identity: invalid signing key")
	}
	return nil
}

// Fingerprint is a short string two people can compare over two channels.
func (p Public) Fingerprint() string {
	h := sha256.New()
	h.Write([]byte("claudebridge identity v1"))
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(p.Name)))
	h.Write(l[:])
	h.Write([]byte(p.Name))
	h.Write(p.Static)
	h.Write(p.Sign)
	s := strings.ToUpper(hex.EncodeToString(h.Sum(nil)[:20]))
	var parts []string
	for i := 0; i < len(s); i += 4 {
		parts = append(parts, s[i:i+4])
	}
	return strings.Join(parts, " ")
}

type fileJSON struct {
	Name   string `json:"name"`
	Static []byte `json:"static_private"`
	Sign   []byte `json:"sign_private"`
}

// Save writes the private identity, owner-only, atomically.
func (i *Identity) Save(path string) error {
	data, err := json.Marshal(fileJSON{Name: i.Name, Static: i.Static.Bytes(), Sign: i.Sign})
	if err != nil {
		return err
	}
	return writeFile(path, data, 0o600)
}

// Load reads a private identity. It refuses a file that other users can read.
func Load(path string) (*Identity, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("identity: %s is readable by other users (mode %v); chmod 600", path, st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f fileJSON
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("identity: %s: %w", path, err)
	}
	if !ValidName(f.Name) || len(f.Sign) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: %s is not a valid identity file", path)
	}
	static, err := ecdh.X25519().NewPrivateKey(f.Static)
	if err != nil {
		return nil, fmt.Errorf("identity: %s: bad static key", path)
	}
	return &Identity{Name: f.Name, Static: static, Sign: ed25519.PrivateKey(f.Sign)}, nil
}

// SavePublic writes a public bundle.
func SavePublic(path string, p Public) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'), 0o644)
}

// LoadPublic reads and validates a public bundle.
func LoadPublic(path string) (Public, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Public{}, err
	}
	var p Public
	if err := json.Unmarshal(data, &p); err != nil {
		return Public{}, fmt.Errorf("identity: %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return Public{}, fmt.Errorf("identity: %s: %w", path, err)
	}
	return p, nil
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-identity-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
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
	return os.Rename(tmp.Name(), path)
}
