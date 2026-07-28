package auth

import (
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	// Use low iterations to keep the suite fast; format is identical.
	h, err := hashPasswordWithIterations("s3cret-pass", minIterations)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, hashAlgo+"$") {
		t.Fatalf("hash %q missing algo prefix", h)
	}
	if strings.Count(h, "$") != 3 {
		t.Fatalf("hash %q should have 4 fields", h)
	}

	ok, err := VerifyPassword(h, "s3cret-pass")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("correct password did not verify")
	}

	ok, err = VerifyPassword(h, "wrong-pass")
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Fatal("wrong password verified")
	}
}

func TestHashPasswordUsesRandomSalt(t *testing.T) {
	a, err := hashPasswordWithIterations("same", minIterations)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := hashPasswordWithIterations("same", minIterations)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a == b {
		t.Fatal("identical passwords produced identical hashes — salt is not random")
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("expected error for empty password")
	}
}

func TestDefaultIterationsMeetsOWASP(t *testing.T) {
	if DefaultIterations < 600_000 {
		t.Fatalf("DefaultIterations = %d, want >= 600000 for PBKDF2-HMAC-SHA256", DefaultIterations)
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	valid, err := hashPasswordWithIterations("pw", minIterations)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	parts := strings.Split(valid, "$")

	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"too few fields", "pbkdf2-sha256$10000$salt"},
		{"too many fields", valid + "$extra"},
		{"unknown algo", "bcrypt$10000$" + parts[2] + "$" + parts[3]},
		{"non numeric iterations", "pbkdf2-sha256$abc$" + parts[2] + "$" + parts[3]},
		{"iterations below minimum", "pbkdf2-sha256$1$" + parts[2] + "$" + parts[3]},
		{"bad salt base64", "pbkdf2-sha256$10000$!!!$" + parts[3]},
		{"bad key base64", "pbkdf2-sha256$10000$" + parts[2] + "$!!!"},
		{"empty key", "pbkdf2-sha256$10000$" + parts[2] + "$"},
		{"plaintext password mistaken for hash", "hunter2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.encoded, "pw")
			if err == nil {
				t.Fatal("expected error for malformed hash, got nil")
			}
			if ok {
				t.Fatal("malformed hash must never verify as true")
			}
		})
	}
}

func TestValidateHash(t *testing.T) {
	good, err := hashPasswordWithIterations("pw", minIterations)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := ValidateHash(good); err != nil {
		t.Fatalf("ValidateHash(good) = %v, want nil", err)
	}
	if err := ValidateHash("not-a-hash"); err == nil {
		t.Fatal("ValidateHash(bad) = nil, want error")
	}
}

func TestVerifyPasswordEmptyCandidateDoesNotMatch(t *testing.T) {
	h, err := hashPasswordWithIterations("actual", minIterations)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyPassword(h, "")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Fatal("empty password must not match a non-empty hash")
	}
}
