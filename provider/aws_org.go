package provider

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"

	"smartrenew/config"
	"smartrenew/model"
)

// FetchOrgAccounts tries each configured account to list organization member accounts.
// The first account that succeeds (i.e. the payer/management account) is used.
func FetchOrgAccounts(ctx context.Context, accounts []config.Account) ([]model.OrgAccount, error) {
	for _, acct := range accounts {
		if len(acct.Regions) == 0 {
			continue
		}
		cfg := buildAWSConfig(acct, acct.Regions[0])
		items, err := listOrgAccounts(ctx, cfg)
		if err != nil {
			slog.Debug("org list accounts failed, trying next", "alias", acct.Alias, "err", err)
			continue
		}
		slog.Info("org accounts fetched", "via", acct.Alias, "count", len(items))
		return items, nil
	}
	return nil, fmt.Errorf("no configured account has organizations:ListAccounts permission")
}

func listOrgAccounts(ctx context.Context, cfg aws.Config) ([]model.OrgAccount, error) {
	client := organizations.NewFromConfig(cfg)
	var results []model.OrgAccount
	var nextToken *string

	for {
		out, err := client.ListAccounts(ctx, &organizations.ListAccountsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, a := range out.Accounts {
			var joinedAt time.Time
			if a.JoinedTimestamp != nil {
				joinedAt = *a.JoinedTimestamp
			}
			results = append(results, model.OrgAccount{
				AccountID:    aws.ToString(a.Id),
				AccountName:  aws.ToString(a.Name),
				Email:        aws.ToString(a.Email),
				Status:       string(a.Status),
				JoinedMethod: string(a.JoinedMethod),
				JoinedAt:     joinedAt,
			})
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, nil
}
