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
	h.Write([]byte("petrvs-bridge identity v1"))
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
func Load(path string) (*Identity, error) { return LoadOpts(path, false) }

// CheckPrivateMode refuses a secret file that other users can read. With allowGroupRead the owning group may read it
// (for example a Kubernetes secret volume with fsGroup), but nobody else may, and nobody but the owner may write.
func CheckPrivateMode(path string, allowGroupRead bool) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	bad := os.FileMode(0o077)
	if allowGroupRead {
		bad = 0o027
	}
	if st.Mode().Perm()&bad != 0 {
		return fmt.Errorf("%s is readable by other users (mode %v); chmod 600", path, st.Mode().Perm())
	}
	return nil
}

// LoadOpts is Load with the group-read exception of CheckPrivateMode.
func LoadOpts(path string, allowGroupRead bool) (*Identity, error) {
	if err := CheckPrivateMode(path, allowGroupRead); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
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
	if err := tmp.Sync(); err != nil { // without it a power cut can leave the renamed file empty
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil { // best effort: makes the rename survive a power cut
		d.Sync()
		d.Close()
	}
	return nil
}
