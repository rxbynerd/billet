package backend

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHexID returns prefix_ followed by 16 random bytes, hex-encoded.
func randomHexID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}
