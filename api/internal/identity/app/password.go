package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters from the OWASP Password Storage Cheat Sheet (m=19 MiB, t=2, p=1).
const (
	argonMemoryKiB   = 19 * 1024
	argonIterations  = 2
	argonParallelism = 1
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

// hashPassword returns an argon2id hash in PHC string format:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// Storing the parameters with the hash lets us raise them later without breaking old hashes.
func hashPassword(password string) string {
	salt := make([]byte, argonSaltBytes)
	_, _ = rand.Read(salt) // crypto/rand.Read never returns an error since Go 1.24.

	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemoryKiB, argonParallelism, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// verifyPassword reports whether password matches encodedHash.
func verifyPassword(password, encodedHash string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}

	var memoryKiB, iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memoryKiB, &iterations, &parallelism); err != nil {
		return false, fmt.Errorf("parse argon2 parameters: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}
	wantKey, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("decode hash: %w", err)
	}
	if len(wantKey) != argonKeyBytes {
		return false, fmt.Errorf("unexpected hash length %d", len(wantKey))
	}

	gotKey := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, argonKeyBytes)
	// Constant-time comparison so response timing does not reveal how many bytes matched.
	return subtle.ConstantTimeCompare(gotKey, wantKey) == 1, nil
}
