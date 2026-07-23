// Package auth handles password hashing and session management.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// AUTH-01: Argon2id with parameters at or above the OWASP Password Storage
// Cheat Sheet baseline (19 MiB memory, 2 iterations, 1 degree of parallelism).
// Memory is raised to 64 MiB here because a household deployment authenticates
// rarely and can afford it.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// Password policy. Length is the control that matters; composition rules push
// users toward predictable substitutions without adding real entropy.
const (
	// MinPasswordLen is the shortest accepted password.
	MinPasswordLen = 12
	// MaxPasswordLen bounds input so a huge submission cannot be used to force
	// the server into an expensive hash.
	MaxPasswordLen = 1024
)

// Errors returned by this package.
var (
	// ErrPasswordTooShort is returned when a password is below MinPasswordLen.
	ErrPasswordTooShort = fmt.Errorf("auth: password must be at least %d characters", MinPasswordLen)
	// ErrPasswordTooLong is returned when a password exceeds MaxPasswordLen.
	ErrPasswordTooLong = fmt.Errorf("auth: password must be at most %d characters", MaxPasswordLen)
	// ErrInvalidHash is returned when a stored hash cannot be parsed.
	ErrInvalidHash = errors.New("auth: invalid password hash")
	// ErrMismatch is returned when a password does not match its hash.
	ErrMismatch = errors.New("auth: password does not match")
)

func argonParallelism() uint8 {
	if n := runtime.NumCPU(); n > 1 && n < 255 {
		return uint8(n)
	}
	return 1
}

// ValidatePassword checks the policy without hashing.
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen {
		return ErrPasswordTooShort
	}
	if n > MaxPasswordLen {
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword returns a PHC-formatted Argon2id string embedding the salt and
// parameters, so a future parameter change can be detected per-hash rather than
// invalidating every stored credential.
func HashPassword(pw string) (string, error) {
	if err := ValidatePassword(pw); err != nil {
		return "", err
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	par := argonParallelism()
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, par, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, par,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword compares a password against a stored PHC hash in constant time.
func VerifyPassword(pw, encoded string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return ErrInvalidHash
	}
	if version != argon2.Version {
		return ErrInvalidHash
	}

	var memory, time uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &parallelism); err != nil {
		return ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return ErrInvalidHash
	}

	got := argon2.IDKey([]byte(pw), salt, time, memory, parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// DummyVerify performs a hash with the same cost as a real verification. Login
// calls it when no account matches, so response timing does not reveal whether
// an email is registered.
func DummyVerify(pw string) {
	salt := make([]byte, argonSaltLen)
	_ = argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonParallelism(), argonKeyLen)
}
