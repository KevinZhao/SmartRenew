package auth

import (
	"sync"
	"testing"
	"time"
)

func TestSessionStoreCreateAndGet(t *testing.T) {
	s := NewSessionStore(time.Hour)
	token, sess, err := s.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	if sess.Username != "alice" {
		t.Fatalf("Username = %q, want alice", sess.Username)
	}

	got, ok := s.Get(token)
	if !ok {
		t.Fatal("Get(token) not found")
	}
	if got.Username != "alice" {
		t.Fatalf("Get username = %q, want alice", got.Username)
	}
}

func TestSessionStoreGetRejectsUnknownAndEmpty(t *testing.T) {
	s := NewSessionStore(time.Hour)
	if _, ok := s.Get(""); ok {
		t.Fatal("empty token accepted")
	}
	if _, ok := s.Get("some-forged-token"); ok {
		t.Fatal("forged token accepted")
	}
}

func TestSessionStoreTokensAreUnique(t *testing.T) {
	s := NewSessionStore(time.Hour)
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		token, _, err := s.Create("alice")
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		if seen[token] {
			t.Fatalf("duplicate token generated at iteration %d", i)
		}
		seen[token] = true
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	s := NewSessionStore(time.Hour)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	token, sess, err := s.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := now.Add(time.Hour); !sess.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", sess.ExpiresAt, want)
	}

	// Just before expiry the session is still valid.
	now = now.Add(time.Hour - time.Second)
	if _, ok := s.Get(token); !ok {
		t.Fatal("session expired too early")
	}

	// Exactly at expiry it is gone (expiry is exclusive).
	now = now.Add(time.Second)
	if _, ok := s.Get(token); ok {
		t.Fatal("expired session still valid")
	}

	// And the expired entry was evicted, not merely hidden.
	if got := s.Count(); got != 0 {
		t.Fatalf("Count() = %d after expiry, want 0", got)
	}
}

func TestSessionStoreDelete(t *testing.T) {
	s := NewSessionStore(time.Hour)
	token, _, err := s.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.Delete(token)
	if _, ok := s.Get(token); ok {
		t.Fatal("deleted session still valid")
	}
	// Deleting twice, or deleting "", must not panic.
	s.Delete(token)
	s.Delete("")
}

func TestSessionStoreDeleteDoesNotAffectOthers(t *testing.T) {
	s := NewSessionStore(time.Hour)
	a, _, err := s.Create("alice")
	if err != nil {
		t.Fatalf("Create alice: %v", err)
	}
	b, _, err := s.Create("bob")
	if err != nil {
		t.Fatalf("Create bob: %v", err)
	}
	s.Delete(a)
	if _, ok := s.Get(b); !ok {
		t.Fatal("deleting alice's session revoked bob's")
	}
}

func TestSessionStoreDefaultTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Hour} {
		s := NewSessionStore(ttl)
		if s.TTL() != 12*time.Hour {
			t.Fatalf("NewSessionStore(%v).TTL() = %v, want 12h", ttl, s.TTL())
		}
	}
}

func TestSessionStorePrunesExpiredOnCreate(t *testing.T) {
	s := NewSessionStore(time.Minute)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if _, _, err := s.Create("alice"); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if got := s.Count(); got != 5 {
		t.Fatalf("Count() = %d, want 5", got)
	}

	now = now.Add(2 * time.Minute)
	if _, _, err := s.Create("bob"); err != nil {
		t.Fatalf("Create bob: %v", err)
	}
	if got := s.Count(); got != 1 {
		t.Fatalf("Count() = %d after prune, want 1 (only bob)", got)
	}
}

func TestSessionStoreConcurrentAccess(t *testing.T) {
	s := NewSessionStore(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, _, err := s.Create("alice")
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			if _, ok := s.Get(token); !ok {
				t.Error("session missing right after create")
			}
			s.Delete(token)
		}()
	}
	wg.Wait()
	if got := s.Count(); got != 0 {
		t.Fatalf("Count() = %d, want 0", got)
	}
}
