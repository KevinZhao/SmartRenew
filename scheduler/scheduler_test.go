package scheduler

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/model"
	"github.com/KevinZhao/SmartRenew/notifier"
	"github.com/KevinZhao/SmartRenew/store"
)

// fakeNotifier records what it was asked to send.
type fakeNotifier struct {
	mu       sync.Mutex
	batches  [][]model.Alert
	gpuSends [][]model.GPUCoverage
	failWith error
}

func (f *fakeNotifier) Name() string { return "fake" }

func (f *fakeNotifier) Send(alerts []model.Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.batches = append(f.batches, append([]model.Alert(nil), alerts...))
	return nil
}

func (f *fakeNotifier) SendGPUAlerts(items []model.GPUCoverage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.gpuSends = append(f.gpuSends, append([]model.GPUCoverage(nil), items...))
	return nil
}

func (f *fakeNotifier) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func newTestScheduler(t *testing.T, remindDays []int) (*Scheduler, *store.Store, *fakeNotifier) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfg := config.DefaultConfig()
	if remindDays != nil {
		cfg.RemindDays = remindDays
	}
	fn := &fakeNotifier{}
	return New(cfg, s, []notifier.Notifier{fn}), s, fn
}

func res(id string, end time.Time) model.Reservation {
	return model.Reservation{
		ID:           "111122223333_us-east-1_" + id,
		AccountAlias: "acct",
		AccountID:    "111122223333",
		Region:       "us-east-1",
		Type:         model.TypeODCR,
		ResourceID:   id,
		InstanceType: "p5.48xlarge",
		Quantity:     1,
		Status:       "active",
		StartTime:    end.Add(-365 * 24 * time.Hour),
		EndTime:      end,
	}
}

func TestCheckAndNotifySendsOnce(t *testing.T) {
	sc, s, fn := newTestScheduler(t, nil)
	if err := s.Upsert(res("r1", time.Now().UTC().Add(5*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	sc.CheckAndNotify()
	if got := fn.sentCount(); got != 1 {
		t.Fatalf("first run sent %d alerts, want 1", got)
	}

	// Second run within the same reminder window must be deduped.
	sc.CheckAndNotify()
	if got := fn.sentCount(); got != 1 {
		t.Fatalf("second run sent a duplicate: total %d, want 1", got)
	}
}

func TestCheckAndNotifyFiresAtEachConfiguredThreshold(t *testing.T) {
	// remind_days is meant to schedule a reminder at each listed day. Before the
	// fix only the first alert per coarse level was ever sent, so [3,1] produced
	// a single notification and the 1-day warning never arrived.
	sc, s, fn := newTestScheduler(t, []int{30, 14, 7, 3, 1})
	now := time.Now().UTC()

	for _, days := range []int{30, 14, 7, 3, 1} {
		r := res("r1", now.Add(time.Duration(days)*24*time.Hour))
		if err := s.Upsert(r); err != nil {
			t.Fatalf("upsert at %d days: %v", days, err)
		}
		sc.CheckAndNotify()
	}

	if got := fn.sentCount(); got != 5 {
		t.Fatalf("sent %d alerts across the five configured thresholds, want 5", got)
	}
}

func TestCheckAndNotifyRetriesAfterNotifierFailure(t *testing.T) {
	sc, s, fn := newTestScheduler(t, nil)
	if err := s.Upsert(res("r1", time.Now().UTC().Add(5*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	fn.failWith = errFake
	sc.CheckAndNotify()
	if got := fn.sentCount(); got != 0 {
		t.Fatalf("failing notifier recorded %d sends", got)
	}

	// A failed send must not be marked as notified, so the next cycle retries.
	fn.failWith = nil
	sc.CheckAndNotify()
	if got := fn.sentCount(); got != 1 {
		t.Fatalf("after recovery sent %d alerts, want 1 — the failed alert was marked as notified", got)
	}
}

func TestCheckAndNotifyAlertsAgainAfterRenewal(t *testing.T) {
	sc, s, fn := newTestScheduler(t, []int{30, 7})
	now := time.Now().UTC()

	r := res("cr-renew", now.Add(5*24*time.Hour))
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sc.CheckAndNotify()
	if got := fn.sentCount(); got != 1 {
		t.Fatalf("first cycle sent %d, want 1", got)
	}

	// Renewed: same reservation id, expiry a year out, then approaching again.
	r.EndTime = now.Add(370 * 24 * time.Hour)
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert renewed: %v", err)
	}
	sc.CheckAndNotify()

	// Second cycle: approaching again, but at a *different* expiry instant than
	// the first cycle (a renewal moves the end date), so it must alert again.
	r.EndTime = now.Add(6 * 24 * time.Hour)
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert second cycle: %v", err)
	}
	sc.CheckAndNotify()

	if got := fn.sentCount(); got < 2 {
		t.Fatalf("sent %d alerts, want >= 2 — a renewed resource must alert again on its new cycle", got)
	}
}

func TestCheckAndNotifyNoNotifiersIsNoOp(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	sc := New(config.DefaultConfig(), s, nil)
	if err := s.Upsert(res("r1", time.Now().UTC().Add(5*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sc.CheckAndNotify() // must not panic

	// Nothing was marked as notified, so a later run with a notifier still sends.
	notified, err := s.HasNotified("111122223333_us-east-1_r1", "t30", time.Now().UTC().Add(5*24*time.Hour))
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if notified {
		t.Error("alert was marked notified even though there were no notifiers")
	}
}

func TestCheckAndNotifySkipsExpiredResources(t *testing.T) {
	sc, s, fn := newTestScheduler(t, nil)
	// Expired earlier today — must never be alerted on.
	if err := s.Upsert(res("gone", time.Now().UTC().Add(-3*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sc.CheckAndNotify()
	if got := fn.sentCount(); got != 0 {
		t.Fatalf("sent %d alerts for an already-expired resource, want 0", got)
	}
}

func TestPruneNotifyLogRunsWithoutError(t *testing.T) {
	sc, s, _ := newTestScheduler(t, nil)
	old := model.Alert{Reservation: res("old", time.Now().UTC().Add(-400*24*time.Hour)), Level: model.LevelCritical}
	if err := s.MarkNotifiedBatch([]model.Alert{old}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	sc.pruneNotifyLog()

	still, err := s.HasNotified(old.ID, old.NotifyKey(), old.EndTime)
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if still {
		t.Error("stale notify_log entry survived pruning")
	}
}

var errFake = fakeErr("notifier down")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
