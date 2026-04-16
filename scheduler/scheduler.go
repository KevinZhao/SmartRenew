package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/model"
	"github.com/KevinZhao/SmartRenew/notifier"
	"github.com/KevinZhao/SmartRenew/provider"
	"github.com/KevinZhao/SmartRenew/store"
)

type Scheduler struct {
	cfg       *config.Config
	store     *store.Store
	notifiers []notifier.Notifier
}

func New(cfg *config.Config, s *store.Store, notifiers []notifier.Notifier) *Scheduler {
	return &Scheduler{cfg: cfg, store: s, notifiers: notifiers}
}

// SyncAll fetches data from all accounts and upserts into store.
// Uses delete-then-repopulate per account to purge resources removed from AWS.
func (sc *Scheduler) SyncAll(ctx context.Context) (int, []error) {
	var allErrors []error
	total := 0

	for _, acct := range sc.cfg.Accounts {
		items, errs := provider.SyncAccount(ctx, acct)
		allErrors = append(allErrors, errs...)

		// Only purge old data if we got at least some results (avoid wiping on total API failure)
		if len(items) > 0 {
			if err := sc.store.DeleteByAccountID(acct.AccountID); err != nil {
				allErrors = append(allErrors, err)
			}
		}

		for _, item := range items {
			if err := sc.store.Upsert(item); err != nil {
				allErrors = append(allErrors, err)
				continue
			}
			total++
		}
	}
	return total, allErrors
}

// CheckAndNotify checks for expiring resources and sends notifications.
func (sc *Scheduler) CheckAndNotify() {
	if len(sc.notifiers) == 0 {
		return
	}

	maxDays := sc.cfg.MaxRemindDays()

	alerts, err := sc.store.GetAlerts(maxDays)
	if err != nil {
		slog.Error("get alerts failed", "err", err)
		return
	}

	// Filter out already-notified alerts (keyed on composite ID)
	var pending []model.Alert
	for _, a := range alerts {
		notified, err := sc.store.HasNotified(a.ID, a.Level)
		if err != nil {
			slog.Error("check notify log failed", "id", a.ID, "level", a.Level, "err", err)
			continue
		}
		if !notified {
			pending = append(pending, a)
		}
	}

	if len(pending) == 0 {
		return
	}

	// Send to all notifiers, track if at least one succeeded
	anySent := false
	for _, n := range sc.notifiers {
		if err := n.Send(pending); err != nil {
			slog.Error("notifier send failed", "notifier", n.Name(), "err", err)
		} else {
			slog.Info("alerts sent", "notifier", n.Name(), "count", len(pending))
			anySent = true
		}
	}

	// Only mark as notified when at least one notifier succeeded
	if !anySent {
		slog.Warn("all notifiers failed, will retry on next cycle")
		return
	}

	if err := sc.store.MarkNotifiedBatch(pending); err != nil {
		slog.Error("batch mark notified failed", "err", err)
	}
}

// StartCron starts periodic sync and notification checks.
// Triggers an immediate sync on startup, then runs on intervals.
func (sc *Scheduler) StartCron(ctx context.Context) {
	syncInterval := sc.cfg.ParseSyncInterval()
	alertInterval := sc.cfg.ParseAlertInterval()
	syncTicker := time.NewTicker(syncInterval)
	alertTicker := time.NewTicker(alertInterval)

	go func() {
		// Immediate first sync on startup
		slog.Info("running initial sync")
		total, errs := sc.SyncAll(ctx)
		slog.Info("initial sync done", "items", total, "errors", len(errs))
		for _, e := range errs {
			slog.Error("sync error", "err", e)
		}
		sc.CheckAndNotify()

		for {
			select {
			case <-ctx.Done():
				syncTicker.Stop()
				alertTicker.Stop()
				return
			case <-syncTicker.C:
				slog.Info("running periodic sync")
				total, errs := sc.SyncAll(ctx)
				slog.Info("sync done", "items", total, "errors", len(errs))
				for _, e := range errs {
					slog.Error("sync error", "err", e)
				}
			case <-alertTicker.C:
				sc.CheckAndNotify()
			}
		}
	}()

	slog.Info("cron started", "sync_interval", syncInterval, "alert_interval", alertInterval)
}
