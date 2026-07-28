package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

const (
	hashAlgo = "pbkdf2-sha256"
	// DefaultIterations follows the OWASP recommendation for PBKDF2-HMAC-SHA256.
	DefaultIterations = 600_000
	saltLen           = 16
	keyLen            = 32
	// minIterations rejects encoded hashes that were weakened on purpose.
	minIterations = 10_000
)

var b64 = base64.RawURLEncoding

// HashPassword returns an encoded PBKDF2-SHA256 hash in the form
// pbkdf2-sha256$<iterations>$<salt>$<key>, both parts base64url (unpadded).
func HashPassword(password string) (string, error) {
	return hashPasswordWithIterations(password, DefaultIterations)
}

func hashPasswordWithIterations(password string, iter int) (string, error) {
	if password == "" {
		return "", fmt.Errorf("auth: password must not be empty")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, iter, keyLen)
	if err != nil {
		return "", fmt.Errorf("auth: derive key: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashAlgo, iter, b64.EncodeToString(salt), b64.EncodeToString(dk)), nil
}

// VerifyPassword reports whether password matches the encoded PBKDF2 hash.
// A malformed encoding is an error, never a silent mismatch.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return false, fmt.Errorf("auth: malformed password hash (want 4 fields, got %d)", len(parts))
	}
	if parts[0] != hashAlgo {
		return false, fmt.Errorf("auth: unsupported password hash algorithm %q", parts[0])
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, fmt.Errorf("auth: malformed iteration count %q", parts[1])
	}
	if iter < minIterations {
		return false, fmt.Errorf("auth: iteration count %d is below the minimum of %d", iter, minIterations)
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false, fmt.Errorf("auth: malformed salt: %w", err)
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil {
		return false, fmt.Errorf("auth: malformed key: %w", err)
	}
	if len(want) == 0 {
		return false, fmt.Errorf("auth: empty key in password hash")
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, fmt.Errorf("auth: derive key: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// hashIterations returns the iteration count encoded in a password hash.
func hashIterations(encoded string) (int, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return 0, fmt.Errorf("auth: malformed password hash")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("auth: malformed iteration count %q", parts[1])
	}
	return iter, nil
}

// ValidateHash reports whether encoded is a well-formed hash this package can verify.
func ValidateHash(encoded string) error {
	// An empty password can never match, so a false result here is not a format problem.
	if _, err := VerifyPassword(encoded, ""); err != nil {
		return err
	}
	return nil
}
