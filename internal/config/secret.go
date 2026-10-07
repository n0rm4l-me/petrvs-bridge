package config

import "github.com/n0rm4l-me/petrvs-bridge/internal/approval"

func decodeSecret(b32 string) ([]byte, error) { return approval.DecodeSecret(b32) }
