package auth

import (
	"fmt"
	"strings"
)

// User is a statically configured account. Exactly one of PasswordHash or
// Password (plaintext, dev only) must be set.
type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Password     string `json:"password"`
}

// Authenticator verifies credentials against a fixed user list.
type Authenticator struct {
	// users maps a lowercased username to its verifier.
	users map[string]userEntry
	// dummyHash is verified against when the username is unknown, so an
	// unknown username costs about as much as a wrong password (no user
	// enumeration via response latency). Its iteration count matches the
	// configured users' so the timings actually line up.
	dummyHash string
}

type userEntry struct {
	username string
	// hash is the encoded PBKDF2 hash. Plaintext-configured passwords are
	// hashed at startup so every user verifies through the same path.
	hash string
}

// NewAuthenticator builds an Authenticator from configured users. Plaintext
// passwords are hashed at startup so the plaintext is not compared directly.
func NewAuthenticator(users []User) (*Authenticator, error) {
	a := &Authenticator{users: make(map[string]userEntry, len(users))}
	// minIter tracks the smallest iteration count in use, which the dummy hash
	// then mirrors.
	minIter := 0
	for i, u := range users {
		name := strings.TrimSpace(u.Username)
		if name == "" {
			return nil, fmt.Errorf("auth: users[%d]: username is required", i)
		}
		key := strings.ToLower(name)
		if _, dup := a.users[key]; dup {
			return nil, fmt.Errorf("auth: users[%d]: duplicate username %q", i, name)
		}
		switch {
		case u.PasswordHash != "":
			if err := ValidateHash(u.PasswordHash); err != nil {
				return nil, fmt.Errorf("auth: users[%d] %q: %w", i, name, err)
			}
			a.users[key] = userEntry{username: name, hash: u.PasswordHash}
		case u.Password != "":
			// Hash once at startup: verification then follows the same
			// constant-time path as pre-hashed users.
			h, err := HashPassword(u.Password)
			if err != nil {
				return nil, fmt.Errorf("auth: users[%d] %q: %w", i, name, err)
			}
			a.users[key] = userEntry{username: name, hash: h}
		default:
			return nil, fmt.Errorf("auth: users[%d] %q: either password_hash or password is required", i, name)
		}
		if iter, err := hashIterations(a.users[key].hash); err == nil && (minIter == 0 || iter < minIter) {
			minIter = iter
		}
	}
	if len(a.users) == 0 {
		return nil, fmt.Errorf("auth: at least one user must be configured")
	}
	if minIter == 0 {
		minIter = DefaultIterations
	}
	dummy, err := hashPasswordWithIterations("smartrenew-dummy-password", minIter)
	if err != nil {
		return nil, fmt.Errorf("auth: build dummy hash: %w", err)
	}
	a.dummyHash = dummy
	return a, nil
}

// Verify returns the canonical username when the credentials are valid.
func (a *Authenticator) Verify(username, password string) (string, bool) {
	entry, ok := a.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok {
		// Burn comparable CPU time, then fail.
		_, _ = VerifyPassword(a.dummyHash, password)
		return "", false
	}
	match, err := VerifyPassword(entry.hash, password)
	if err != nil || !match {
		return "", false
	}
	return entry.username, true
}

// Usernames returns the configured usernames (canonical form), for logging.
func (a *Authenticator) Usernames() []string {
	names := make([]string, 0, len(a.users))
	for _, e := range a.users {
		names = append(names, e.username)
	}
	return names
}
