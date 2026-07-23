package auth

import (
	"errors"
	"strings"
	"testing"
)

// AUTH-01: passwords are hashed with Argon2id, salted per credential.
func TestHashAndVerify(t *testing.T) {
	const pw = "correct-horse-battery-staple"

	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash is not Argon2id: %q", hash)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("the hash contains the plaintext password")
	}
	if err := VerifyPassword(pw, hash); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
	if err := VerifyPassword("not-the-password", hash); !errors.Is(err, ErrMismatch) {
		t.Errorf("wrong password error = %v, want ErrMismatch", err)
	}
}

// Identical passwords must produce different hashes, or a stolen database
// reveals which accounts share a password.
func TestHashesAreSalted(t *testing.T) {
	const pw = "a-perfectly-fine-password"
	seen := make(map[string]bool)
	for i := 0; i < 5; i++ {
		h, err := HashPassword(pw)
		if err != nil {
			t.Fatalf("hash %d: %v", i, err)
		}
		if seen[h] {
			t.Fatal("two hashes of the same password are identical; salting is broken")
		}
		seen[h] = true
		if err := VerifyPassword(pw, h); err != nil {
			t.Errorf("hash %d does not verify: %v", i, err)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLen-1)); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("short password error = %v, want ErrPasswordTooShort", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLen)); err != nil {
		t.Errorf("minimum-length password rejected: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Errorf("overlong password error = %v, want ErrPasswordTooLong", err)
	}
	// Length is counted in runes, so a multi-byte passphrase is not
	// over-credited by its byte count.
	if err := ValidatePassword(strings.Repeat("é", MinPasswordLen-1)); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("multi-byte short password was accepted by byte count")
	}
	if _, err := HashPassword("short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("HashPassword accepted a password below policy: %v", err)
	}
}

// A malformed or truncated stored hash must never verify as a match.
func TestMalformedHashRejected(t *testing.T) {
	valid, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"",
		"not-a-hash",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",  // wrong variant
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA",        // missing segment
		"$argon2id$v=99$m=65536,t=3,p=4$c2FsdA$aGFzaA", // wrong version
		"$argon2id$v=19$bad-params$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA", // undecodable salt
		valid[:len(valid)-4],                        // truncated
	}
	for _, h := range bad {
		if err := VerifyPassword("correct-horse-battery-staple", h); err == nil {
			t.Errorf("malformed hash verified as a match: %q", h)
		}
	}
}

func TestHashTokenIsStableAndNotTheToken(t *testing.T) {
	const token = "a-session-token-value"
	h1 := hashToken(token)
	h2 := hashToken(token)
	if h1 != h2 {
		t.Error("hashToken is not deterministic")
	}
	if strings.Contains(h1, token) {
		t.Error("the stored digest contains the raw token")
	}
	if h1 == hashToken(token+"x") {
		t.Error("different tokens produced the same digest")
	}
	if len(h1) != 64 {
		t.Errorf("digest length = %d, want 64 hex characters for SHA-256", len(h1))
	}
}
