package e2e

import (
	"crypto/ecdh"
	"crypto/rand"

	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
)

type transportPublished = transport.Published

func ecdhKeyPair() (*ecdh.PrivateKey, error) { return ecdh.X25519().GenerateKey(rand.Reader) }

func ecdhKey() ([]byte, error) {
	k, err := ecdhKeyPair()
	if err != nil {
		return nil, err
	}
	return k.PublicKey().Bytes(), nil
}
