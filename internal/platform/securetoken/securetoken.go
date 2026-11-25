// Package securetoken creates the random tokens used in invitation links.
// The plain token is handed to the client once; only its SHA-256 hash is stored, so a
// database leak does not reveal usable tokens.
package securetoken

import (
	"crypto/rand"
	"crypto/sha256"
)

// New returns a random token with 128 bits of entropy and the hash to store for it.
func New() (token string, hash []byte) {
	token = rand.Text()
	return token, Hash(token)
}

// Hash returns the SHA-256 hash of token. A fast hash is fine here because the token is
// random and long, unlike a password.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
