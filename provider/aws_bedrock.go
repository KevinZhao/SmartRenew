package provider

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"

	"smartrenew/config"
	"smartrenew/model"
)

func fetchBedrockProvisionedThroughputs(ctx context.Context, cfg aws.Config, acct config.Account, region string) ([]model.Reservation, error) {
	client := bedrock.NewFromConfig(cfg)
	var results []model.Reservation
	var nextToken *string

	for {
		out, err := client.ListProvisionedModelThroughputs(ctx, &bedrock.ListProvisionedModelThroughputsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, pt := range out.ProvisionedModelSummaries {
			// Skip non-committed throughputs — nothing to track.
			if pt.CommitmentExpirationTime == nil {
				continue
			}

			creationTime := aws.ToTime(pt.CreationTime)
			expirationTime := aws.ToTime(pt.CommitmentExpirationTime)

			mu := aws.ToInt32(pt.ModelUnits)
			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(pt.ProvisionedModelArn)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				Type:         model.TypeBedrockPT,
				ResourceID:   aws.ToString(pt.ProvisionedModelArn),
				InstanceType: aws.ToString(pt.ProvisionedModelName),
				Platform:     "Bedrock",
				Quantity:     int(mu),
				StartTime:    creationTime,
				EndTime:      expirationTime,
				Status:       string(pt.Status),
				Description:  fmt.Sprintf("Bedrock PT - %d MU %s", mu, pt.CommitmentDuration),
			})
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, nil
}
