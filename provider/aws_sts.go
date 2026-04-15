package provider

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"smartrenew/config"
)

// AssumeRoleForAccount uses the payer account's credentials to assume a role
// in the target account and returns an Account with temporary credentials.
func AssumeRoleForAccount(ctx context.Context, payer config.Account, targetAccountID, roleName string) (config.Account, error) {
	payerCfg := buildAWSConfig(payer, payer.Regions[0])
	client := sts.NewFromConfig(payerCfg)

	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", targetAccountID, roleName)
	sessionName := fmt.Sprintf("SmartRenew-%s", targetAccountID)

	out, err := client.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(sessionName),
		DurationSeconds: aws.Int32(3600),
	})
	if err != nil {
		return config.Account{}, fmt.Errorf("assume role %s in %s: %w", roleName, targetAccountID, err)
	}

	return config.Account{
		Alias:        targetAccountID,
		AccountID:    targetAccountID,
		AccessKey:    aws.ToString(out.Credentials.AccessKeyId),
		SecretKey:    aws.ToString(out.Credentials.SecretAccessKey),
		SessionToken: aws.ToString(out.Credentials.SessionToken),
		Regions:      payer.Regions,
	}, nil
}

// ExpandOrgAccounts discovers all ACTIVE member accounts in the payer's
// organization and returns Account entries with AssumeRole credentials.
// Accounts in skipIDs are skipped (e.g. already configured directly).
func ExpandOrgAccounts(ctx context.Context, payer config.Account, skipIDs map[string]bool) ([]config.Account, []error) {
	orgAccounts, err := FetchOrgAccounts(ctx, []config.Account{payer})
	if err != nil {
		return nil, []error{fmt.Errorf("list org accounts: %w", err)}
	}

	var results []config.Account
	var errs []error
	skipped := 0

	for _, oa := range orgAccounts {
		if oa.Status != "ACTIVE" {
			continue
		}
		if skipIDs[oa.AccountID] {
			skipped++
			continue
		}

		member, err := AssumeRoleForAccount(ctx, payer, oa.AccountID, payer.OrgRoleName)
		if err != nil {
			slog.Warn("assume role failed, skipping member", "account_id", oa.AccountID, "name", oa.AccountName, "err", err)
			errs = append(errs, err)
			continue
		}
		if oa.AccountName != "" {
			member.Alias = oa.AccountName
		}
		results = append(results, member)
	}

	slog.Info("org expansion complete",
		"payer", payer.Alias,
		"discovered", len(orgAccounts),
		"assumed", len(results),
		"skipped", skipped,
		"failed", len(errs),
	)
	return results, errs
}
