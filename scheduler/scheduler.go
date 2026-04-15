package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"smartrenew/config"
	"smartrenew/model"
	"smartrenew/notifier"
	"smartrenew/provider"
	"smartrenew/store"
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

	// Sync organization accounts
	orgAccounts, err := provider.FetchOrgAccounts(ctx, sc.cfg.Accounts)
	if err != nil {
		allErrors = append(allErrors, fmt.Errorf("org accounts: %w", err))
	} else {
		for _, a := range orgAccounts {
			if err := sc.store.UpsertOrgAccount(a); err != nil {
				allErrors = append(allErrors, fmt.Errorf("upsert org account %s: %w", a.AccountID, err))
			}
		}
		slog.Info("org accounts synced", "count", len(orgAccounts))
	}

	for _, acct := range sc.cfg.Accounts {
		// If this is an org payer, expand and sync member accounts
		if acct.OrgRoleName != "" {
			// Build set of directly-configured account IDs to avoid double-sync
			directIDs := make(map[string]bool, len(sc.cfg.Accounts))
			for _, a := range sc.cfg.Accounts {
				directIDs[a.AccountID] = true
			}
			memberAccounts, expandErrs := provider.ExpandOrgAccounts(ctx, acct, directIDs)
			allErrors = append(allErrors, expandErrs...)
			for _, member := range memberAccounts {
				n, errs := syncOneAccount(ctx, sc.store, member)
				total += n
				allErrors = append(allErrors, errs...)
			}
		}

		// Always sync the configured account itself (payer or regular)
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
func (sc *Scheduler) StartCron(ctx context.Context) {
	syncInterval := sc.cfg.ParseSyncInterval()
	alertInterval := sc.cfg.ParseAlertInterval()
	syncTicker := time.NewTicker(syncInterval)
	alertTicker := time.NewTicker(alertInterval)

	go func() {
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

// syncOneAccount syncs a single account: fetch, delete old data, upsert new.
func syncOneAccount(ctx context.Context, st *store.Store, acct config.Account) (int, []error) {
	items, errs := provider.SyncAccount(ctx, acct)
	if len(items) > 0 {
		if err := st.DeleteByAccountID(acct.AccountID); err != nil {
			errs = append(errs, err)
		}
	}
	count := 0
	for _, item := range items {
		if err := st.Upsert(item); err != nil {
			errs = append(errs, err)
			continue
		}
		count++
	}
	return count, errs
}
