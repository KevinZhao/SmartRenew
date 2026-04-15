package provider

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"

	"smartrenew/config"
	"smartrenew/model"
)

func buildAWSConfig(acct config.Account, region string) aws.Config {
	return aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(acct.AccessKey, acct.SecretKey, acct.SessionToken),
	}
}

// SyncAccount fetches all supported reservation types for one account across all its regions.
func SyncAccount(ctx context.Context, acct config.Account) ([]model.Reservation, []error) {
	var all []model.Reservation
	var errs []error

	if len(acct.Regions) == 0 {
		return nil, []error{fmt.Errorf("%s: no regions configured", acct.Alias)}
	}

	// SP is a global API — fetch once using the first region.
	spCfg := buildAWSConfig(acct, acct.Regions[0])
	spItems, err := fetchSavingsPlans(ctx, spCfg, acct)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s/SP: %w", acct.Alias, err))
	} else {
		all = append(all, spItems...)
	}

	// CB, ODCR, RI are regional — fetch per region.
	for _, region := range acct.Regions {
		cfg := buildAWSConfig(acct, region)

		// CB + ODCR in one API call, split by ReservationType
		cbItems, odcrItems, err := fetchCapacityReservations(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/CR: %w", acct.Alias, region, err))
		} else {
			all = append(all, cbItems...)
			all = append(all, odcrItems...)
		}

		riItems, err := fetchReservedInstances(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/RI: %w", acct.Alias, region, err))
		} else {
			all = append(all, riItems...)
		}

		// Database RIs
		rdsItems, err := fetchRDSReservedInstances(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/RDS-RI: %w", acct.Alias, region, err))
		} else {
			all = append(all, rdsItems...)
		}

		cacheItems, err := fetchElastiCacheReservedNodes(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/Cache-RI: %w", acct.Alias, region, err))
		} else {
			all = append(all, cacheItems...)
		}

		redshiftItems, err := fetchRedshiftReservedNodes(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/Redshift-RI: %w", acct.Alias, region, err))
		} else {
			all = append(all, redshiftItems...)
		}

		osItems, err := fetchOpenSearchReservedInstances(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/OpenSearch-RI: %w", acct.Alias, region, err))
		} else {
			all = append(all, osItems...)
		}

		mdbItems, err := fetchMemoryDBReservedNodes(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/MemoryDB-RI: %w", acct.Alias, region, err))
		} else {
			all = append(all, mdbItems...)
		}

		// Bedrock Provisioned Throughput
		bedrockItems, err := fetchBedrockProvisionedThroughputs(ctx, cfg, acct, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/Bedrock-PT: %w", acct.Alias, region, err))
		} else {
			all = append(all, bedrockItems...)
		}
	}

	return all, errs
}

func fetchSavingsPlans(ctx context.Context, cfg aws.Config, acct config.Account) ([]model.Reservation, error) {
	client := savingsplans.NewFromConfig(cfg)
	var results []model.Reservation
	var nextToken *string

	for {
		out, err := client.DescribeSavingsPlans(ctx, &savingsplans.DescribeSavingsPlansInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, sp := range out.SavingsPlans {
			var startTime, endTime time.Time
			if sp.Start != nil {
				if t, err := time.Parse(time.RFC3339, *sp.Start); err != nil {
					slog.Warn("parse SP start time", "sp_id", aws.ToString(sp.SavingsPlanId), "err", err)
				} else {
					startTime = t
				}
			}
			if sp.End != nil {
				if t, err := time.Parse(time.RFC3339, *sp.End); err != nil {
					slog.Warn("parse SP end time", "sp_id", aws.ToString(sp.SavingsPlanId), "err", err)
				} else {
					endTime = t
				}
			}

			// SP is global — use "global" as region
			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_global_%s", acct.AccountID, aws.ToString(sp.SavingsPlanId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       "global",
				Type:         model.TypeSP,
				ResourceID:   aws.ToString(sp.SavingsPlanId),
				InstanceType: aws.ToString(sp.Ec2InstanceFamily),
				Platform:     string(sp.SavingsPlanType),
				Quantity:     1,
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       string(sp.State),
				Description:  fmt.Sprintf("%s - %s", sp.SavingsPlanType, sp.PaymentOption),
			})
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, nil
}

// fetchCapacityReservations fetches all capacity reservations in one API call
// and splits them into Capacity Blocks and ODCRs.
func fetchCapacityReservations(ctx context.Context, cfg aws.Config, acct config.Account, region string) (cbs []model.Reservation, odcrs []model.Reservation, err error) {
	client := ec2.NewFromConfig(cfg)
	var nextToken *string

	for {
		out, err := client.DescribeCapacityReservations(ctx, &ec2.DescribeCapacityReservationsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, cr := range out.CapacityReservations {
			startTime := aws.ToTime(cr.StartDate)
			endTime := aws.ToTime(cr.EndDate)

			r := model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(cr.CapacityReservationId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				ResourceID:   aws.ToString(cr.CapacityReservationId),
				InstanceType: aws.ToString(cr.InstanceType),
				Platform:     string(cr.InstancePlatform),
				Quantity:     int(aws.ToInt32(cr.TotalInstanceCount)),
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       string(cr.State),
			}

			if cr.ReservationType == ec2types.CapacityReservationTypeCapacityBlock {
				r.Type = model.TypeCB
				r.Description = fmt.Sprintf("Capacity Block - %s", aws.ToString(cr.AvailabilityZone))
				cbs = append(cbs, r)
			} else {
				r.Type = model.TypeODCR
				r.Description = fmt.Sprintf("ODCR - %s", aws.ToString(cr.AvailabilityZone))
				odcrs = append(odcrs, r)
			}
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return cbs, odcrs, nil
}

func fetchReservedInstances(ctx context.Context, cfg aws.Config, acct config.Account, region string) ([]model.Reservation, error) {
	client := ec2.NewFromConfig(cfg)

	out, err := client.DescribeReservedInstances(ctx, &ec2.DescribeReservedInstancesInput{})
	if err != nil {
		return nil, err
	}

	var results []model.Reservation
	for _, ri := range out.ReservedInstances {
		startTime := aws.ToTime(ri.Start)
		endTime := aws.ToTime(ri.End)

		results = append(results, model.Reservation{
			ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(ri.ReservedInstancesId)),
			AccountAlias: acct.Alias,
			AccountID:    acct.AccountID,
			Region:       region,
			Type:         model.TypeRI,
			ResourceID:   aws.ToString(ri.ReservedInstancesId),
			InstanceType: string(ri.InstanceType),
			Platform:     string(ri.ProductDescription),
			Quantity:     int(aws.ToInt32(ri.InstanceCount)),
			StartTime:    startTime,
			EndTime:      endTime,
			Status:       string(ri.State),
			Description:  fmt.Sprintf("RI - %s %s", ri.OfferingType, ri.OfferingClass),
		})
	}
	return results, nil
}
