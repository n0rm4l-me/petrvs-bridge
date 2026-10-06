package config

import "claudebridge/internal/approval"

func decodeSecret(b32 string) ([]byte, error) { return approval.DecodeSecret(b32) }
