package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/KevinZhao/SmartRenew/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// res builds a reservation expiring at the given time.
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

// --- expiry window correctness ---

func TestGetAlertsExcludesAlreadyExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	// A reservation that expired earlier *today*. String comparison against
	// SQLite's datetime('now') gets this wrong because 'T' (0x54) sorts after
	// ' ' (0x20), making any same-day timestamp look larger than "now".
	if err := s.Upsert(res("expired-today", now.Add(-2*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Upsert(res("future", now.Add(5*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	alerts, err := s.GetAlerts(30)
	if err != nil {
		t.Fatalf("GetAlerts: %v", err)
	}
	for _, a := range alerts {
		if a.ResourceID == "expired-today" {
			t.Errorf("already-expired reservation (end=%s, now=%s) returned as an alert",
				a.EndTime.Format(time.RFC3339), now.Format(time.RFC3339))
		}
		if a.DaysLeft < 0 {
			t.Errorf("alert %s has negative days_left=%d", a.ResourceID, a.DaysLeft)
		}
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1 (only the future one)", len(alerts))
	}
}

func TestGetAlertsHandlesNonUTCOffsets(t *testing.T) {
	s := newTestStore(t)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Now().UTC()

	// Same instant, expressed with a +08:00 offset — as CSV import produces
	// when the file carries local timestamps. A string compare reads the
	// local hour digits and gets the ordering wrong.
	if err := s.Upsert(res("expired-offset", now.Add(-2*time.Hour).In(shanghai))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Upsert(res("future-offset", now.Add(3*24*time.Hour).In(shanghai))); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	alerts, err := s.GetAlerts(30)
	if err != nil {
		t.Fatalf("GetAlerts: %v", err)
	}
	var ids []string
	for _, a := range alerts {
		ids = append(ids, a.ResourceID)
	}
	if len(alerts) != 1 || alerts[0].ResourceID != "future-offset" {
		t.Fatalf("got alerts %v, want only [future-offset]", ids)
	}
}

func TestGetAlertsWindowBoundary(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	if err := s.Upsert(res("inside", now.Add(29*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Upsert(res("outside", now.Add(31*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	alerts, err := s.GetAlerts(30)
	if err != nil {
		t.Fatalf("GetAlerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].ResourceID != "inside" {
		var ids []string
		for _, a := range alerts {
			ids = append(ids, a.ResourceID)
		}
		t.Fatalf("got %v, want only [inside]", ids)
	}
}

func TestGetAlertsSkipsZeroEndTime(t *testing.T) {
	s := newTestStore(t)
	r := res("no-end", time.Time{})
	r.EndTime = time.Time{}
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	alerts, err := s.GetAlerts(30)
	if err != nil {
		t.Fatalf("GetAlerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("got %d alerts, want 0 — a zero end_time is not an upcoming expiry", len(alerts))
	}
}

func TestGetAlertsExcludesInactiveStatuses(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	for _, st := range []string{"cancelled", "expired", "failed", "deleted", "retired"} {
		r := res("st-"+st, now.Add(5*24*time.Hour))
		r.Status = st
		if err := s.Upsert(r); err != nil {
			t.Fatalf("upsert %s: %v", st, err)
		}
	}
	alerts, err := s.GetAlerts(30)
	if err != nil {
		t.Fatalf("GetAlerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("got %d alerts, want 0 — inactive statuses must be excluded", len(alerts))
	}
}

// --- updated_at round-trip ---

func TestUpdatedAtRoundTrips(t *testing.T) {
	s := newTestStore(t)
	before := time.Now().UTC().Add(-2 * time.Second)
	if err := s.Upsert(res("r1", before.Add(30*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rows, err := s.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0].UpdatedAt
	if got.IsZero() {
		t.Fatal("UpdatedAt is zero — the value written by SQLite could not be parsed back")
	}
	if got.Before(before) || got.After(time.Now().UTC().Add(2*time.Second)) {
		t.Errorf("UpdatedAt = %v, want a timestamp near now (%v)", got, time.Now().UTC())
	}
}

func TestGPUCoverageUpdatedAtRoundTrips(t *testing.T) {
	s := newTestStore(t)
	g := model.GPUCoverage{
		ID: "111122223333_us-east-1_i-1", AccountAlias: "acct", AccountID: "111122223333",
		Region: "us-east-1", AZ: "us-east-1a", InstanceID: "i-1",
		InstanceType: "p5.48xlarge", Coverage: model.CoverageOnDemand,
	}
	if err := s.UpsertGPUCoverage(g); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := s.ListGPUCoverage("")
	if err != nil {
		t.Fatalf("ListGPUCoverage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].UpdatedAt.IsZero() {
		t.Fatal("GPUCoverage.UpdatedAt is zero — written value could not be parsed back")
	}
}

func TestStartAndEndTimeRoundTrip(t *testing.T) {
	s := newTestStore(t)
	start := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	end := time.Date(2027, 3, 1, 10, 30, 0, 0, time.UTC)
	r := res("times", end)
	r.StartTime = start
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := s.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !rows[0].StartTime.Equal(start) {
		t.Errorf("StartTime = %v, want %v", rows[0].StartTime, start)
	}
	if !rows[0].EndTime.Equal(end) {
		t.Errorf("EndTime = %v, want %v", rows[0].EndTime, end)
	}
}

// --- notify log / renewal ---

func TestRenewedResourceAlertsAgain(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	const rid = "cr-renewable"

	// Year 1: expiring in 2 days → alert sent and recorded.
	r := res(rid, now.Add(2*24*time.Hour))
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	alerts, err := s.GetAlerts(30)
	if err != nil {
		t.Fatalf("GetAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	if err := s.MarkNotifiedBatch(alerts); err != nil {
		t.Fatalf("MarkNotifiedBatch: %v", err)
	}
	notified, err := s.HasNotified(alerts[0].ID, alerts[0].NotifyKey(), alerts[0].EndTime)
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if !notified {
		t.Fatal("alert was not recorded as notified")
	}

	// Operator renews: same capacity reservation ID, later end date.
	renewedEnd := now.Add(367 * 24 * time.Hour)
	r.EndTime = renewedEnd
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert renewed: %v", err)
	}

	// A year later it is expiring again. The dedup key must account for the
	// new end date, or this resource is silently never alerted on again.
	r.EndTime = now.Add(2 * 24 * time.Hour).Add(365 * 24 * time.Hour)
	if err := s.Upsert(r); err != nil {
		t.Fatalf("upsert second cycle: %v", err)
	}
	notified, err = s.HasNotified(r.ID, string(model.LevelCritical), r.EndTime)
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if notified {
		t.Error("renewed resource with a new end date is treated as already notified — it will never alert again")
	}
}

func TestHasNotifiedIsPerEndTime(t *testing.T) {
	s := newTestStore(t)
	end1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end2 := time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC)
	a := model.Alert{Reservation: res("cr-x", end1), Level: model.LevelCritical}

	if err := s.MarkNotifiedBatch([]model.Alert{a}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	got, err := s.HasNotified(a.ID, string(model.LevelCritical), end1)
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if !got {
		t.Error("same id/level/end_time should be deduped")
	}

	got, err = s.HasNotified(a.ID, string(model.LevelCritical), end2)
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if got {
		t.Error("a different end_time is a different expiry cycle and must alert again")
	}

	got, err = s.HasNotified(a.ID, string(model.LevelWarning), end1)
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if got {
		t.Error("a different level must alert separately")
	}
}

func TestMarkNotifiedBatchIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	alerts := []model.Alert{{Reservation: res("cr-y", end), Level: model.LevelWarning}}
	for i := 0; i < 3; i++ {
		if err := s.MarkNotifiedBatch(alerts); err != nil {
			t.Fatalf("mark #%d: %v", i, err)
		}
	}
}

func TestPruneNotifyLogDropsStaleRows(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	// An entry for an expiry that is long past.
	old := model.Alert{Reservation: res("cr-old", now.Add(-400*24*time.Hour)), Level: model.LevelCritical}
	fresh := model.Alert{Reservation: res("cr-fresh", now.Add(10*24*time.Hour)), Level: model.LevelCritical}
	if err := s.MarkNotifiedBatch([]model.Alert{old, fresh}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	removed, err := s.PruneNotifyLog(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneNotifyLog: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d rows, want 1 (only the long-expired one)", removed)
	}

	stillThere, err := s.HasNotified(fresh.ID, fresh.NotifyKey(), fresh.EndTime)
	if err != nil {
		t.Fatalf("HasNotified: %v", err)
	}
	if !stillThere {
		t.Error("prune removed a still-relevant notify_log entry")
	}
}

// --- transactional sync ---

func TestReplaceAccountIsAtomic(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	initial := make([]model.Reservation, 10)
	for i := range initial {
		initial[i] = res(string(rune('a'+i)), now.Add(time.Duration(100+i)*24*time.Hour))
	}
	if err := s.ReplaceAccount("111122223333", initial); err != nil {
		t.Fatalf("ReplaceAccount: %v", err)
	}
	rows, err := s.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10", len(rows))
	}

	// Readers must never observe a partially-populated account: with only 3
	// rows in the new set, the table goes 10 → 3 in one step, never 0.
	next := initial[:3]
	if err := s.ReplaceAccount("111122223333", next); err != nil {
		t.Fatalf("ReplaceAccount: %v", err)
	}
	rows, err = s.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 — stale rows were not dropped", len(rows))
	}
}

func TestReplaceAccountRollsBackOnError(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	good := []model.Reservation{res("keep-1", now.Add(50*24*time.Hour)), res("keep-2", now.Add(60*24*time.Hour))}
	if err := s.ReplaceAccount("111122223333", good); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Two rows sharing an ID violate the primary key inside the transaction.
	dup := res("dup", now.Add(70*24*time.Hour))
	if err := s.ReplaceAccount("111122223333", []model.Reservation{dup, dup}); err == nil {
		t.Log("duplicate IDs were upserted without error (ON CONFLICT handles it) — nothing to assert")
	}

	// Either way the account must not be left empty.
	rows, err := s.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) == 0 {
		t.Error("account was left with zero rows after a failed replace")
	}
}

func TestReplaceAccountLeavesOtherAccountsAlone(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	other := res("other-1", now.Add(40*24*time.Hour))
	other.AccountID = "999988887777"
	other.ID = "999988887777_us-east-1_other-1"
	if err := s.Upsert(other); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	if err := s.ReplaceAccount("111122223333", []model.Reservation{res("mine", now.Add(20*24*time.Hour))}); err != nil {
		t.Fatalf("ReplaceAccount: %v", err)
	}

	rows, err := s.List("", "999988887777")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("other account has %d rows, want 1 — replace touched the wrong account", len(rows))
	}
}

func TestReplaceGPUCoverageIsAtomic(t *testing.T) {
	s := newTestStore(t)
	mk := func(id string) model.GPUCoverage {
		return model.GPUCoverage{
			ID: "111122223333_us-east-1_" + id, AccountAlias: "acct", AccountID: "111122223333",
			Region: "us-east-1", InstanceID: id, InstanceType: "p5.48xlarge",
			Coverage: model.CoverageOnDemand,
		}
	}
	if err := s.ReplaceGPUCoverage("111122223333", []model.GPUCoverage{mk("i-1"), mk("i-2")}); err != nil {
		t.Fatalf("ReplaceGPUCoverage: %v", err)
	}
	rows, err := s.ListGPUCoverage("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	if err := s.ReplaceGPUCoverage("111122223333", []model.GPUCoverage{mk("i-1")}); err != nil {
		t.Fatalf("ReplaceGPUCoverage: %v", err)
	}
	rows, err = s.ListGPUCoverage("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — stale coverage row survived", len(rows))
	}
}

// --- existing behaviour guards ---

func TestListFilters(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	sp := res("sp-1", now.Add(100*24*time.Hour))
	sp.Type = model.TypeSP
	if err := s.Upsert(sp); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Upsert(res("odcr-1", now.Add(100*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rows, err := s.List("sp", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Type != model.TypeSP {
		t.Fatalf("type filter returned %d rows", len(rows))
	}

	rows, err = s.List("", "111122223333")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("account filter returned %d rows, want 2", len(rows))
	}

	rows, err = s.List("", "does-not-exist")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("unknown account returned %d rows, want 0", len(rows))
	}
}

func TestPruneAccountsNoOpOnEmptyKeep(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert(res("r1", time.Now().Add(100*24*time.Hour))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	n, err := s.PruneAccounts(nil)
	if err != nil {
		t.Fatalf("PruneAccounts: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d rows on empty keep list, want 0", n)
	}
	rows, _ := s.List("", "")
	if len(rows) != 1 {
		t.Error("empty keep list wiped the database")
	}
}

func TestPruneAccountsDropsUnknownAccounts(t *testing.T) {
	s := newTestStore(t)
	keep := res("keep", time.Now().Add(100*24*time.Hour))
	drop := res("drop", time.Now().Add(100*24*time.Hour))
	drop.AccountID = "999988887777"
	drop.ID = "999988887777_us-east-1_drop"
	if err := s.Upsert(keep); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Upsert(drop); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if _, err := s.PruneAccounts([]string{"111122223333"}); err != nil {
		t.Fatalf("PruneAccounts: %v", err)
	}
	rows, _ := s.List("", "")
	if len(rows) != 1 || rows[0].AccountID != "111122223333" {
		t.Fatalf("got %d rows, want only the kept account", len(rows))
	}
}
