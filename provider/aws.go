package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"

	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/model"
)

func buildAWSConfig(acct config.Account, region string) aws.Config {
	return aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(acct.AccessKey, acct.SecretKey, ""),
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
	spItems, spErrs := fetchSavingsPlans(ctx, spCfg, acct)
	for _, e := range spErrs {
		errs = append(errs, fmt.Errorf("%s/SP: %w", acct.Alias, e))
	}
	all = append(all, spItems...)

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
		if !acct.ShouldSkip(string(model.TypeRDSRI)) {
			rdsItems, err := fetchRDSReservedInstances(ctx, cfg, acct, region)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s/RDS-RI: %w", acct.Alias, region, err))
			} else {
				all = append(all, rdsItems...)
			}
		}

		if !acct.ShouldSkip(string(model.TypeCacheRI)) {
			cacheItems, err := fetchElastiCacheReservedNodes(ctx, cfg, acct, region)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s/Cache-RI: %w", acct.Alias, region, err))
			} else {
				all = append(all, cacheItems...)
			}
		}

		if !acct.ShouldSkip(string(model.TypeRedshiftRI)) {
			redshiftItems, err := fetchRedshiftReservedNodes(ctx, cfg, acct, region)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s/Redshift-RI: %w", acct.Alias, region, err))
			} else {
				all = append(all, redshiftItems...)
			}
		}

		if !acct.ShouldSkip(string(model.TypeOpenSearchRI)) {
			osItems, err := fetchOpenSearchReservedInstances(ctx, cfg, acct, region)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s/OpenSearch-RI: %w", acct.Alias, region, err))
			} else {
				all = append(all, osItems...)
			}
		}

		if !acct.ShouldSkip(string(model.TypeMemoryDBRI)) {
			mdbItems, err := fetchMemoryDBReservedNodes(ctx, cfg, acct, region)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s/MemoryDB-RI: %w", acct.Alias, region, err))
			} else {
				all = append(all, mdbItems...)
			}
		}

		// Bedrock Provisioned Throughput
		if !acct.ShouldSkip(string(model.TypeBedrockPT)) {
			bedrockItems, err := fetchBedrockProvisionedThroughputs(ctx, cfg, acct, region)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s/Bedrock-PT: %w", acct.Alias, region, err))
			} else {
				all = append(all, bedrockItems...)
			}
		}
	}

	return all, errs
}

// fetchSavingsPlans returns the account's savings plans. Plans whose expiry
// cannot be parsed are skipped and reported in errs rather than stored with a
// zero end date, which would make them silently un-alertable.
func fetchSavingsPlans(ctx context.Context, cfg aws.Config, acct config.Account) (results []model.Reservation, errs []error) {
	client := savingsplans.NewFromConfig(cfg)
	var nextToken *string

	for {
		out, err := client.DescribeSavingsPlans(ctx, &savingsplans.DescribeSavingsPlansInput{
			NextToken: nextToken,
		})
		if err != nil {
			return results, append(errs, err)
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
					// A plan whose expiry we cannot read must not be stored with
					// a zero end date: it would silently never alert. Surface it
					// as an error so the account is treated as a partial failure
					// and existing rows are preserved.
					errs = append(errs, fmt.Errorf("SP %s: unparseable end time %q: %w",
						aws.ToString(sp.SavingsPlanId), *sp.End, err))
					continue
				} else {
					endTime = t
				}
			}

			spRegion := aws.ToString(sp.Region)
			if spRegion == "" {
				spRegion = "global"
			}

			// Fetch the full rate table for this SP so the UI can show a unit rate.
			//   GPU family SP  → $/GPU card  (top rate ÷ card count)
			//   CPU family SP  → $/vCPU      (top rate ÷ vCPU count)
			//   Compute SP     → $/vCPU from a reference instance (c7i.xlarge, etc.)
			//                    plus "equivalent cores" = commitment ÷ per-vCPU rate.
			var rates map[string]float64
			if string(sp.State) == "active" {
				r, rErr := fetchSPRates(ctx, cfg, aws.ToString(sp.SavingsPlanId))
				if rErr != nil {
					slog.Warn("fetch SP rates failed", "sp_id", aws.ToString(sp.SavingsPlanId), "err", rErr)
				} else {
					rates = r
				}
			}

			unitRate, equivCores := normalizeSPRate(string(sp.SavingsPlanType), rates, aws.ToString(sp.Commitment))

			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, spRegion, aws.ToString(sp.SavingsPlanId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       spRegion,
				Type:         model.TypeSP,
				ResourceID:   aws.ToString(sp.SavingsPlanId),
				InstanceType: aws.ToString(sp.Ec2InstanceFamily),
				Platform:     string(sp.SavingsPlanType),
				Quantity:     1,
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       string(sp.State),
				Description:  fmt.Sprintf("%s - %s", sp.SavingsPlanType, sp.PaymentOption),
				HourlyRate:   unitRate,
				EquivCores:   equivCores,
			})
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, errs
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

			total := int(aws.ToInt32(cr.TotalInstanceCount))
			available := int(aws.ToInt32(cr.AvailableInstanceCount))
			used := total - available

			// Future-dated / pending_accept CRs have TotalInstanceCount=0 until activation.
			// The committed quantity is stored in the AWS-managed tag below.
			// Such CRs are not yet usable, so used=0 (not total-available=0).
			if total == 0 {
				for _, t := range cr.Tags {
					if aws.ToString(t.Key) == "aws:ec2capacityreservation:incrementalRequestedQuantity" {
						if n, err := strconv.Atoi(aws.ToString(t.Value)); err == nil && n > 0 {
							total = n
							used = 0
						}
						break
					}
				}
			}

			r := model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(cr.CapacityReservationId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				ResourceID:   aws.ToString(cr.CapacityReservationId),
				InstanceType: aws.ToString(cr.InstanceType),
				Platform:     string(cr.InstancePlatform),
				Quantity:     total,
				UsedCount:    used,
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       string(cr.State),
			}

			if cr.ReservationType == ec2types.CapacityReservationTypeCapacityBlock {
				r.Type = model.TypeCB
				r.Description = fmt.Sprintf("Capacity Block - %s", aws.ToString(cr.AvailabilityZone))
				// Capacity = instances × accelerator cards per instance; fall back to
				// vCPUs for non-GPU instance types. Stored in EquivCores so the UI
				// can render it the same way as SP capacity.
				if cards := GPUCardCount(r.InstanceType); cards > 0 {
					r.EquivCores = float64(total * cards)
				} else if vcpu := vCPUCount(r.InstanceType); vcpu > 0 {
					r.EquivCores = float64(total * vcpu)
				}
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
