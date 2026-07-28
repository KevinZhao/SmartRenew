package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// SessionCookieName is the cookie carrying the opaque session token.
const SessionCookieName = "smartrenew_session"

const (
	tokenBytes = 32
	// maxSessions caps memory usage from a flood of successful logins.
	maxSessions = 10_000
)

// Session is an authenticated browser session.
type Session struct {
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionStore keeps sessions in process memory. The app runs as a single
// replica (Deployment strategy: Recreate), so sessions intentionally do not
// survive a restart — users simply log in again.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	ttl      time.Duration
	now      func() time.Time
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionStore{
		sessions: make(map[string]Session),
		ttl:      ttl,
		now:      time.Now,
	}
}

// TTL returns the configured session lifetime.
func (s *SessionStore) TTL() time.Duration { return s.ttl }

// Create issues a new session token for username.
func (s *SessionStore) Create(username string) (string, Session, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, fmt.Errorf("auth: generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if len(s.sessions) >= maxSessions {
		return "", Session{}, fmt.Errorf("auth: too many active sessions (%d)", len(s.sessions))
	}
	sess := Session{Username: username, CreatedAt: now, ExpiresAt: now.Add(s.ttl)}
	s.sessions[token] = sess
	return token, sess, nil
}

// Get returns the session for token if it exists and has not expired.
func (s *SessionStore) Get(token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return Session{}, false
	}
	if !s.now().Before(sess.ExpiresAt) {
		delete(s.sessions, token)
		return Session{}, false
	}
	return sess, true
}

// Delete revokes a single session.
func (s *SessionStore) Delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// Count returns the number of non-expired sessions.
func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	return len(s.sessions)
}

func (s *SessionStore) pruneLocked(now time.Time) {
	for token, sess := range s.sessions {
		if !now.Before(sess.ExpiresAt) {
			delete(s.sessions, token)
		}
	}
}
