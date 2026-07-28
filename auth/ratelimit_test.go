package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginLimiterLocksOutAfterMaxAttempts(t *testing.T) {
	l := NewLoginLimiter(3, 15*time.Minute)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	for i := 1; i <= 3; i++ {
		if ok, _ := l.Reserve("1.2.3.4"); !ok {
			t.Fatalf("attempt %d refused, want the first 3 to be allowed", i)
		}
	}

	ok, retry := l.Reserve("1.2.3.4")
	if ok {
		t.Fatal("4th attempt allowed, want lockout after 3")
	}
	if retry <= 0 || retry > 15*time.Minute {
		t.Fatalf("retryAfter = %v, want (0, 15m]", retry)
	}
}

func TestLoginLimiterLockoutExpires(t *testing.T) {
	l := NewLoginLimiter(2, 10*time.Minute)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	l.Reserve("ip")
	l.Reserve("ip")
	if ok, _ := l.Reserve("ip"); ok {
		t.Fatal("expected lockout")
	}

	now = now.Add(10 * time.Minute)
	ok, retry := l.Reserve("ip")
	if !ok {
		t.Fatalf("lockout did not expire, retry = %v", retry)
	}
	if retry != 0 {
		t.Fatalf("retryAfter = %v after expiry, want 0", retry)
	}

	// The window restarted: one more attempt is still allowed before locking.
	if ok, _ := l.Reserve("ip"); !ok {
		t.Fatal("counter was not reset after lockout expiry")
	}
}

func TestLoginLimiterSucceedReleasesAttempts(t *testing.T) {
	l := NewLoginLimiter(3, time.Minute)
	l.Reserve("ip")
	l.Reserve("ip")
	l.Succeed("ip")

	// After a success the budget is full again: 3 more attempts must be allowed.
	for i := 1; i <= 3; i++ {
		if ok, _ := l.Reserve("ip"); !ok {
			t.Fatalf("attempt %d after Succeed refused — budget was not released", i)
		}
	}
}

func TestLoginLimiterKeysAreIndependent(t *testing.T) {
	l := NewLoginLimiter(2, time.Minute)
	l.Reserve("attacker")
	l.Reserve("attacker")
	if ok, _ := l.Reserve("attacker"); ok {
		t.Fatal("attacker should be locked out")
	}
	if ok, _ := l.Reserve("innocent"); !ok {
		t.Fatal("unrelated client was locked out")
	}
}

func TestLoginLimiterAllowsFirstAttempt(t *testing.T) {
	l := NewLoginLimiter(5, time.Minute)
	ok, retry := l.Reserve("fresh-ip")
	if !ok || retry != 0 {
		t.Fatalf("Reserve(fresh) = (%v, %v), want (true, 0)", ok, retry)
	}
}

func TestLoginLimiterDefaults(t *testing.T) {
	l := NewLoginLimiter(0, 0)
	if l.max != 5 {
		t.Fatalf("max = %d, want default 5", l.max)
	}
	if l.window != 15*time.Minute {
		t.Fatalf("window = %v, want default 15m", l.window)
	}
}

func TestLoginLimiterPrunesStaleEntries(t *testing.T) {
	l := NewLoginLimiter(5, time.Minute)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	l.Reserve("old-ip")
	if len(l.attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(l.attempts))
	}

	// Well past the window: the next Reserve on another key prunes the stale one.
	now = now.Add(5 * time.Minute)
	l.Reserve("new-ip")

	l.mu.Lock()
	_, oldStillThere := l.attempts["old-ip"]
	l.mu.Unlock()
	if oldStillThere {
		t.Fatal("stale entry was not pruned — unlocked records accumulate")
	}
}

func TestLoginLimiterPrunesUnlockedRecords(t *testing.T) {
	// Regression guard: records below the lockout threshold (lockedUntil zero)
	// must also be reclaimed once they go stale, or memory grows unbounded
	// under a low-and-slow attack.
	l := NewLoginLimiter(10, time.Minute)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	for i := 0; i < 100; i++ {
		l.Reserve(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	if len(l.attempts) == 0 {
		t.Fatal("no records created")
	}

	now = now.Add(2 * time.Minute)
	l.Reserve("trigger-prune")

	l.mu.Lock()
	remaining := len(l.attempts)
	l.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("attempts = %d after prune, want 1 (only the triggering key)", remaining)
	}
}

// TestLoginLimiterNoOvershootUnderConcurrency is the regression test for the
// check-then-count race: with a slow password check, concurrent requests used to
// all pass the gate before any failure was recorded.
func TestLoginLimiterNoOvershootUnderConcurrency(t *testing.T) {
	const max = 5
	const goroutines = 50
	l := NewLoginLimiter(max, time.Minute)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximise contention
			if ok, _ := l.Reserve("same-ip"); ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != max {
		t.Fatalf("allowed %d concurrent attempts, want exactly %d — the gate overshoots", got, max)
	}
}

func TestLoginLimiterConcurrentMixedOperations(t *testing.T) {
	l := NewLoginLimiter(100, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Reserve("shared")
			l.Succeed("shared")
		}()
	}
	wg.Wait()
}
