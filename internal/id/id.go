// Package id mints the opaque, prefixed identifiers used across the API.
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a prefixed random ID such as "txn_3f9c0a1b2c3d4e5f". crypto/rand is
// used on purpose: IDs appear in URLs, so they must not be guessable.
func New(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
