package auth

import (
	"testing"
)

// testHash builds a low-iteration hash so tests stay fast.
func testHash(t *testing.T, password string) string {
	t.Helper()
	h, err := hashPasswordWithIterations(password, minIterations)
	if err != nil {
		t.Fatalf("hash %q: %v", password, err)
	}
	return h
}

func TestNewAuthenticatorWithHash(t *testing.T) {
	a, err := NewAuthenticator([]User{{Username: "alice", PasswordHash: testHash(t, "wonderland")}})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	tests := []struct {
		name     string
		user     string
		pass     string
		wantUser string
		wantOK   bool
	}{
		{"correct credentials", "alice", "wonderland", "alice", true},
		{"wrong password", "alice", "Wonderland", "", false},
		{"unknown user", "bob", "wonderland", "", false},
		{"empty password", "alice", "", "", false},
		{"empty username", "", "wonderland", "", false},
		{"username case-insensitive", "ALICE", "wonderland", "alice", true},
		{"username surrounding spaces trimmed", "  alice  ", "wonderland", "alice", true},
		{"password is not trimmed", "alice", " wonderland ", "", false},
		{"sql-ish injection in username", "alice' OR '1'='1", "wonderland", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := a.Verify(tc.user, tc.pass)
			if ok != tc.wantOK || got != tc.wantUser {
				t.Fatalf("Verify(%q, %q) = (%q, %v), want (%q, %v)", tc.user, tc.pass, got, ok, tc.wantUser, tc.wantOK)
			}
		})
	}
}

func TestNewAuthenticatorPlaintextPasswordIsHashed(t *testing.T) {
	a, err := NewAuthenticator([]User{{Username: "dev", Password: "devpass"}})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	entry, ok := a.users["dev"]
	if !ok {
		t.Fatal("user dev missing")
	}
	if entry.hash == "" {
		t.Fatal("plaintext password was not hashed at startup")
	}
	if entry.hash == "devpass" {
		t.Fatal("plaintext password stored verbatim")
	}
	if got, ok := a.Verify("dev", "devpass"); !ok || got != "dev" {
		t.Fatalf(`Verify("dev","devpass") = (%q,%v), want ("dev",true)`, got, ok)
	}
	if _, ok := a.Verify("dev", "nope"); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestNewAuthenticatorCanonicalUsernamePreservesCase(t *testing.T) {
	a, err := NewAuthenticator([]User{{Username: "Alice", PasswordHash: testHash(t, "pw")}})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	got, ok := a.Verify("alice", "pw")
	if !ok {
		t.Fatal("lookup should be case-insensitive")
	}
	if got != "Alice" {
		t.Fatalf("canonical username = %q, want %q", got, "Alice")
	}
}

func TestNewAuthenticatorErrors(t *testing.T) {
	good := testHash(t, "pw")

	tests := []struct {
		name  string
		users []User
	}{
		{"no users", nil},
		{"empty user list", []User{}},
		{"missing username", []User{{PasswordHash: good}}},
		{"blank username", []User{{Username: "   ", PasswordHash: good}}},
		{"no credential", []User{{Username: "alice"}}},
		{"invalid hash format", []User{{Username: "alice", PasswordHash: "not-a-hash"}}},
		{"weak iterations in hash", []User{{Username: "alice", PasswordHash: "pbkdf2-sha256$1$AAAA$AAAA"}}},
		{"duplicate username", []User{
			{Username: "alice", PasswordHash: good},
			{Username: "alice", PasswordHash: good},
		}},
		{"duplicate username differing case", []User{
			{Username: "alice", PasswordHash: good},
			{Username: "ALICE", PasswordHash: good},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAuthenticator(tc.users); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestNewAuthenticatorMultipleUsersAreIsolated(t *testing.T) {
	a, err := NewAuthenticator([]User{
		{Username: "alice", PasswordHash: testHash(t, "alice-pw")},
		{Username: "bob", PasswordHash: testHash(t, "bob-pw")},
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if _, ok := a.Verify("alice", "bob-pw"); ok {
		t.Fatal("alice authenticated with bob's password")
	}
	if _, ok := a.Verify("bob", "alice-pw"); ok {
		t.Fatal("bob authenticated with alice's password")
	}
	if _, ok := a.Verify("alice", "alice-pw"); !ok {
		t.Fatal("alice could not authenticate")
	}
	if _, ok := a.Verify("bob", "bob-pw"); !ok {
		t.Fatal("bob could not authenticate")
	}
}

func TestDummyHashMatchesConfiguredWorkFactor(t *testing.T) {
	// The unknown-user path must cost the same as the wrong-password path,
	// otherwise response latency reveals which usernames exist.
	a, err := NewAuthenticator([]User{{Username: "alice", PasswordHash: testHash(t, "pw")}})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if a.dummyHash == "" {
		t.Fatal("dummyHash was not built")
	}
	dummyIter, err := hashIterations(a.dummyHash)
	if err != nil {
		t.Fatalf("hashIterations(dummy): %v", err)
	}
	userIter, err := hashIterations(a.users["alice"].hash)
	if err != nil {
		t.Fatalf("hashIterations(user): %v", err)
	}
	if dummyIter != userIter {
		t.Fatalf("dummy iterations = %d, user iterations = %d — timings will not match", dummyIter, userIter)
	}
}

func TestDummyHashUsesLowestConfiguredIterations(t *testing.T) {
	weak := testHash(t, "pw") // minIterations
	strong, err := hashPasswordWithIterations("pw", minIterations*2)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	a, err := NewAuthenticator([]User{
		{Username: "strong", PasswordHash: strong},
		{Username: "weak", PasswordHash: weak},
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	got, err := hashIterations(a.dummyHash)
	if err != nil {
		t.Fatalf("hashIterations: %v", err)
	}
	if got != minIterations {
		t.Fatalf("dummy iterations = %d, want %d (the lowest configured)", got, minIterations)
	}
}

func TestUsernames(t *testing.T) {
	a, err := NewAuthenticator([]User{
		{Username: "Alice", PasswordHash: testHash(t, "pw")},
		{Username: "bob", PasswordHash: testHash(t, "pw")},
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	names := a.Usernames()
	if len(names) != 2 {
		t.Fatalf("Usernames() = %v, want 2 entries", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	if !seen["Alice"] || !seen["bob"] {
		t.Fatalf("Usernames() = %v, want Alice and bob", names)
	}
}
