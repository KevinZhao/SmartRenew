package config

import (
	"context"
	"errors"
	"testing"
)

var errBadToken = errors.New("InvalidClientTokenId: The security token included in the request is invalid")

func okResolver(id string) AccountIDResolver {
	return func(ctx context.Context, ak, sk, region string) (string, error) { return id, nil }
}

func failResolver() AccountIDResolver {
	return func(ctx context.Context, ak, sk, region string) (string, error) { return "", errBadToken }
}

// perAlias resolves based on the access key so a mixed set can be tested.
func perAlias(fail map[string]bool) AccountIDResolver {
	return func(ctx context.Context, ak, sk, region string) (string, error) {
		if fail[ak] {
			return "", errBadToken
		}
		return "9809" + ak, nil
	}
}

func TestResolveAccountIDsFillsMissingIDs(t *testing.T) {
	accounts := []Account{
		{Alias: "a", AccessKey: "AK1", SecretKey: "s", Regions: []string{"us-east-1"}},
	}
	degraded, err := ResolveAccountIDs(context.Background(), accounts, okResolver("111122223333"))
	if err != nil {
		t.Fatalf("ResolveAccountIDs: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("degraded = %v, want none", degraded)
	}
	if accounts[0].AccountID != "111122223333" {
		t.Errorf("AccountID = %q, want 111122223333", accounts[0].AccountID)
	}
}

func TestResolveAccountIDsSkipsAlreadySet(t *testing.T) {
	accounts := []Account{
		{Alias: "a", AccountID: "999988887777", AccessKey: "AK1", SecretKey: "s", Regions: []string{"us-east-1"}},
	}
	called := false
	r := func(ctx context.Context, ak, sk, region string) (string, error) {
		called = true
		return "111122223333", nil
	}
	if _, err := ResolveAccountIDs(context.Background(), accounts, r); err != nil {
		t.Fatalf("ResolveAccountIDs: %v", err)
	}
	if called {
		t.Error("resolver was called for an account that already had an ID")
	}
	if accounts[0].AccountID != "999988887777" {
		t.Errorf("AccountID was overwritten: %q", accounts[0].AccountID)
	}
}

func TestResolveAccountIDsSkipsAccountsWithoutRegions(t *testing.T) {
	accounts := []Account{{Alias: "a", AccessKey: "AK1", SecretKey: "s"}}
	if _, err := ResolveAccountIDs(context.Background(), accounts, failResolver()); err != nil {
		t.Fatalf("an account with no regions should be skipped, got %v", err)
	}
}

// TestSNSOnlyFailureDoesNotBlockStartup is the regression test for the
// production CrashLoopBackOff: one stale notifier credential took the whole
// service down (262 restarts) even though reservation syncing was unaffected.
func TestSNSOnlyFailureDoesNotBlockStartup(t *testing.T) {
	accounts := []Account{
		{Alias: "sns-publisher", SNSOnly: true, AccessKey: "STALE", SecretKey: "s", Regions: []string{"ap-northeast-1"}},
	}
	degraded, err := ResolveAccountIDs(context.Background(), accounts, failResolver())
	if err != nil {
		t.Fatalf("a failing SNSOnly account must not be fatal, got: %v", err)
	}
	if len(degraded) != 1 || degraded[0] != "sns-publisher" {
		t.Errorf("degraded = %v, want [sns-publisher]", degraded)
	}
	if accounts[0].AccountID != "" {
		t.Errorf("AccountID = %q, want empty after a failed resolve", accounts[0].AccountID)
	}
}

func TestSyncableAccountFailureIsFatal(t *testing.T) {
	// A syncable account with bad credentials would silently sync nothing, so
	// failing fast is still the right behaviour there.
	accounts := []Account{
		{Alias: "prod", AccessKey: "STALE", SecretKey: "s", Regions: []string{"us-east-1"}},
	}
	if _, err := ResolveAccountIDs(context.Background(), accounts, failResolver()); err == nil {
		t.Fatal("expected an error for a syncable account with invalid credentials")
	}
}

func TestStaleNotifierDoesNotStopOtherAccounts(t *testing.T) {
	// Mirrors production: joybuy_gpureminder is fine, sns-publisher is stale.
	accounts := []Account{
		{Alias: "joybuy_gpureminder", AccessKey: "GOOD", SecretKey: "s", Regions: []string{"us-east-1"}},
		{Alias: "sns-publisher", SNSOnly: true, AccessKey: "STALE", SecretKey: "s", Regions: []string{"ap-northeast-1"}},
	}
	degraded, err := ResolveAccountIDs(context.Background(), accounts,
		perAlias(map[string]bool{"STALE": true}))
	if err != nil {
		t.Fatalf("startup must survive a stale notifier account: %v", err)
	}
	if accounts[0].AccountID != "9809GOOD" {
		t.Errorf("healthy account not resolved: %q", accounts[0].AccountID)
	}
	if len(degraded) != 1 || degraded[0] != "sns-publisher" {
		t.Errorf("degraded = %v, want [sns-publisher]", degraded)
	}
}

func TestSyncableFailureReportedEvenAfterDegradedNotifier(t *testing.T) {
	// Order matters: the notifier fails first, then a syncable account fails.
	// The syncable failure must still surface as an error.
	accounts := []Account{
		{Alias: "sns-publisher", SNSOnly: true, AccessKey: "STALE1", SecretKey: "s", Regions: []string{"ap-northeast-1"}},
		{Alias: "prod", AccessKey: "STALE2", SecretKey: "s", Regions: []string{"us-east-1"}},
	}
	degraded, err := ResolveAccountIDs(context.Background(), accounts,
		perAlias(map[string]bool{"STALE1": true, "STALE2": true}))
	if err == nil {
		t.Fatal("expected an error from the syncable account")
	}
	if len(degraded) != 1 || degraded[0] != "sns-publisher" {
		t.Errorf("degraded = %v, want [sns-publisher] reported alongside the error", degraded)
	}
}
