package config

import (
	"context"
	"fmt"
	"log/slog"
)

// AccountIDResolver looks up the AWS account ID behind a set of credentials.
type AccountIDResolver func(ctx context.Context, accessKey, secretKey, region string) (string, error)

// ResolveAccountIDs fills in any missing AccountID via the resolver.
//
// A failure is fatal only for accounts that are actually synced. SNSOnly
// accounts are credential containers for notifiers, so an expired key there
// degrades notifications but must not stop the app from starting — the whole
// service used to CrashLoopBackOff on one stale notifier key.
//
// Returns the accounts that failed to resolve, so the caller can report them.
func ResolveAccountIDs(ctx context.Context, accounts []Account, resolve AccountIDResolver) ([]string, error) {
	var degraded []string
	for i := range accounts {
		a := &accounts[i]
		if a.AccountID != "" || len(a.Regions) == 0 {
			continue
		}
		id, err := resolve(ctx, a.AccessKey, a.SecretKey, a.Regions[0])
		if err != nil {
			if a.SNSOnly {
				slog.Error("could not resolve account_id for a notifier-only account; "+
					"notifications through it will fail, continuing startup",
					"alias", a.Alias, "err", err)
				degraded = append(degraded, a.Alias)
				continue
			}
			return degraded, fmt.Errorf("resolve account_id for %q: %w", a.Alias, err)
		}
		a.AccountID = id
		slog.Info("account_id resolved via STS", "alias", a.Alias, "account_id", id)
	}
	return degraded, nil
}
