package auth

import (
	"sync"
	"time"
)

// LoginLimiter throttles failed login attempts per key (client IP).
// Successful logins clear the counter.
type LoginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attemptRecord
	max      int
	window   time.Duration
	now      func() time.Time
}

type attemptRecord struct {
	count       int
	lockedUntil time.Time
	lastSeen    time.Time
}

// maxLimiterKeys bounds memory against spoofed-IP floods; on overflow the
// oldest-seen entries are dropped.
const maxLimiterKeys = 50_000

func NewLoginLimiter(max int, window time.Duration) *LoginLimiter {
	if max <= 0 {
		max = 5
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	return &LoginLimiter{
		attempts: make(map[string]*attemptRecord),
		max:      max,
		window:   window,
		now:      time.Now,
	}
}

// Reserve claims one attempt for key, reporting whether it may proceed. When
// the key is locked out it also returns the remaining lockout duration.
//
// Checking and counting are a single atomic step on purpose: verifying a
// password takes hundreds of milliseconds (PBKDF2), so a check-then-count split
// would let every concurrent request in flight pass the gate before any of them
// recorded a failure, overshooting max by the request concurrency.
//
// Callers must invoke Succeed on a successful login to release the attempts.
func (l *LoginLimiter) Reserve(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneLocked(now)

	rec, ok := l.attempts[key]
	if !ok {
		l.attempts[key] = &attemptRecord{count: 1, lastSeen: now}
		return true, 0
	}
	if now.Before(rec.lockedUntil) {
		return false, rec.lockedUntil.Sub(now)
	}
	if !rec.lockedUntil.IsZero() {
		// A previous lockout has expired: start a fresh window.
		rec.lockedUntil = time.Time{}
		rec.count = 0
	}
	rec.lastSeen = now
	if rec.count >= l.max {
		rec.lockedUntil = now.Add(l.window)
		rec.count = 0
		return false, l.window
	}
	rec.count++
	return true, 0
}

// Succeed releases the attempts counted for key after a successful login.
func (l *LoginLimiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

func (l *LoginLimiter) pruneLocked(now time.Time) {
	staleBefore := now.Add(-l.window)
	for key, rec := range l.attempts {
		if now.Before(rec.lockedUntil) {
			continue
		}
		if rec.lastSeen.Before(staleBefore) {
			delete(l.attempts, key)
		}
	}
	if len(l.attempts) < maxLimiterKeys {
		return
	}
	// Still oversized after pruning: drop the least recently seen entries that
	// are not actively locked out.
	oldestKey, oldest := "", time.Time{}
	for len(l.attempts) >= maxLimiterKeys {
		oldestKey, oldest = "", time.Time{}
		for key, rec := range l.attempts {
			if now.Before(rec.lockedUntil) {
				continue
			}
			if oldestKey == "" || rec.lastSeen.Before(oldest) {
				oldestKey, oldest = key, rec.lastSeen
			}
		}
		if oldestKey == "" {
			return // everything is locked out; keep them
		}
		delete(l.attempts, oldestKey)
	}
}
