package wire

import (
	"crypto/ecdh"
	"crypto/rand"
)

func ecdhKey() (*ecdh.PrivateKey, error) { return ecdh.X25519().GenerateKey(rand.Reader) }
