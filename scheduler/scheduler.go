package scheduler

import (
	"context"
	"fmt"
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
// Safe mode: on partial failure (any region/type error), skips the per-account
// purge step so existing rows are preserved. A fully successful account fetch
// triggers delete-and-repopulate to drop rows that no longer exist in AWS.
func (sc *Scheduler) SyncAll(ctx context.Context) (int, []error) {
	var allErrors []error
	total := 0

	// Drop rows for accounts that are no longer in configuration (or are
	// SNS-only credential containers) so that removing an account from
	// config actually cleans up its data.
	keep := make([]string, 0, len(sc.cfg.Accounts))
	for _, a := range sc.cfg.Accounts {
		if a.SNSOnly || a.AccountID == "" {
			continue
		}
		keep = append(keep, a.AccountID)
	}
	if len(keep) == 0 {
		// All configured accounts are SNSOnly (or have unresolved AccountID).
		// Skip prune to avoid wiping the database; surface this so operators
		// notice a possible misconfiguration.
		slog.Warn("no syncable accounts in config, skipping prune to avoid full wipe")
	} else if pruned, err := sc.store.PruneAccounts(keep); err != nil {
		allErrors = append(allErrors, fmt.Errorf("prune removed accounts: %w", err))
	} else if pruned > 0 {
		slog.Info("pruned rows for removed accounts", "rows", pruned, "keep", keep)
	}

	for _, acct := range sc.cfg.Accounts {
		if acct.SNSOnly {
			continue
		}
		items, errs := provider.SyncAccount(ctx, acct)
		allErrors = append(allErrors, errs...)

		if len(errs) == 0 && len(items) > 0 {
			// Fully successful fetch: swap the account's rows atomically so
			// readers never observe a half-populated (or empty) account.
			if err := sc.store.ReplaceAccount(acct.AccountID, items); err != nil {
				allErrors = append(allErrors, fmt.Errorf("replace account %s: %w", acct.Alias, err))
			} else {
				total += len(items)
			}
		} else {
			if len(errs) > 0 {
				slog.Warn("partial sync failure, upserting without purge to preserve existing rows",
					"account", acct.Alias, "errors", len(errs))
			}
			// Partial failure: merge what we did get, keeping existing rows.
			for _, item := range items {
				if err := sc.store.Upsert(item); err != nil {
					allErrors = append(allErrors, err)
					continue
				}
				total++
			}
		}

		// GPU coverage check — same preservation rule.
		gpuItems, gpuErrs := provider.CheckGPUCoverage(ctx, acct)
		allErrors = append(allErrors, gpuErrs...)
		if len(gpuItems) > 0 {
			odCount := 0
			for _, g := range gpuItems {
				if g.Coverage == model.CoverageOnDemand {
					odCount++
				}
			}
			if len(gpuErrs) == 0 {
				if err := sc.store.ReplaceGPUCoverage(acct.AccountID, gpuItems); err != nil {
					allErrors = append(allErrors, fmt.Errorf("replace gpu_coverage %s: %w", acct.Alias, err))
				}
			} else {
				slog.Warn("partial gpu sync failure, upserting without purge to preserve existing rows",
					"account", acct.Alias, "errors", len(gpuErrs))
				for _, g := range gpuItems {
					if err := sc.store.UpsertGPUCoverage(g); err != nil {
						allErrors = append(allErrors, err)
					}
				}
			}
			slog.Info("gpu coverage check done", "account", acct.Alias, "gpu_instances", len(gpuItems), "on_demand", odCount)
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

	alerts, err := sc.store.GetAlerts(maxDays, sc.cfg.RemindDays...)
	if err != nil {
		slog.Error("get alerts failed", "err", err)
		return
	}

	// Filter out already-notified alerts (keyed on composite ID)
	var pending []model.Alert
	for _, a := range alerts {
		notified, err := sc.store.HasNotified(a.ID, a.NotifyKey(), a.EndTime)
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

func (sc *Scheduler) CheckGPUODAndNotify() {
	if len(sc.notifiers) == 0 {
		return
	}

	allGPU, err := sc.store.ListGPUCoverage("")
	if err != nil {
		slog.Error("list gpu coverage failed", "err", err)
		return
	}

	var odItems []model.GPUCoverage
	for _, g := range allGPU {
		if g.Coverage != model.CoverageOnDemand {
			continue
		}
		// GPU on-demand alerts are not tied to an expiry date, so they use the
		// empty end_time slot: one alert per instance while it stays uncovered.
		notified, err := sc.store.HasNotified(g.ID, string(model.LevelGPUOnDemand), time.Time{})
		if err != nil {
			slog.Error("check gpu notify log failed", "id", g.ID, "err", err)
			continue
		}
		if !notified {
			odItems = append(odItems, g)
		}
	}

	if len(odItems) == 0 {
		return
	}

	anySent := false
	for _, n := range sc.notifiers {
		if err := n.SendGPUAlerts(odItems); err != nil {
			slog.Error("gpu alert send failed", "notifier", n.Name(), "err", err)
		} else {
			slog.Info("gpu od alerts sent", "notifier", n.Name(), "count", len(odItems))
			anySent = true
		}
	}

	if !anySent {
		return
	}

	// Mark as notified using the existing notify_log table
	var fakeAlerts []model.Alert
	for _, g := range odItems {
		fakeAlerts = append(fakeAlerts, model.Alert{
			Reservation: model.Reservation{ID: g.ID},
			Level:       model.LevelGPUOnDemand,
		})
	}
	if err := sc.store.MarkNotifiedBatch(fakeAlerts); err != nil {
		slog.Error("batch mark gpu notified failed", "err", err)
	}
}

// notifyLogRetention is how long a notify_log entry is kept past the expiry it
// refers to. Long enough that a resource cannot be re-alerted for the same
// cycle, short enough that the table does not grow without bound.
const notifyLogRetention = 90 * 24 * time.Hour

func (sc *Scheduler) pruneNotifyLog() {
	removed, err := sc.store.PruneNotifyLog(notifyLogRetention)
	if err != nil {
		slog.Error("prune notify_log failed", "err", err)
		return
	}
	if removed > 0 {
		slog.Info("pruned stale notify_log entries", "rows", removed)
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
		sc.CheckGPUODAndNotify()
		sc.pruneNotifyLog()

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
				sc.pruneNotifyLog()
			case <-alertTicker.C:
				sc.CheckAndNotify()
				sc.CheckGPUODAndNotify()
			}
		}
	}()

	slog.Info("cron started", "sync_interval", syncInterval, "alert_interval", alertInterval)
}
