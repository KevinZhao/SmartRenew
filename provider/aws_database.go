package provider

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/memorydb"
	"github.com/aws/aws-sdk-go-v2/service/opensearch"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/redshift"

	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/model"
)

func fetchRDSReservedInstances(ctx context.Context, cfg aws.Config, acct config.Account, region string) ([]model.Reservation, error) {
	client := rds.NewFromConfig(cfg)
	var results []model.Reservation
	var marker *string

	for {
		out, err := client.DescribeReservedDBInstances(ctx, &rds.DescribeReservedDBInstancesInput{
			Marker: marker,
		})
		if err != nil {
			return nil, err
		}
		for _, ri := range out.ReservedDBInstances {
			if ri.Duration == nil {
				slog.Warn("RDS RI missing duration", "id", aws.ToString(ri.ReservedDBInstanceId))
			}
			startTime := aws.ToTime(ri.StartTime)
			endTime := startTime.Add(time.Duration(aws.ToInt32(ri.Duration)) * time.Second)

			desc := fmt.Sprintf("RDS RI - %s", aws.ToString(ri.OfferingType))
			if aws.ToBool(ri.MultiAZ) {
				desc += " (MultiAZ)"
			}

			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(ri.ReservedDBInstanceId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				Type:         model.TypeRDSRI,
				ResourceID:   aws.ToString(ri.ReservedDBInstanceId),
				InstanceType: aws.ToString(ri.DBInstanceClass),
				Platform:     aws.ToString(ri.ProductDescription),
				Quantity:     int(aws.ToInt32(ri.DBInstanceCount)),
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       aws.ToString(ri.State),
				Description:  desc,
			})
		}
		marker = out.Marker
		if marker == nil {
			break
		}
	}
	return results, nil
}

func fetchElastiCacheReservedNodes(ctx context.Context, cfg aws.Config, acct config.Account, region string) ([]model.Reservation, error) {
	client := elasticache.NewFromConfig(cfg)
	var results []model.Reservation
	var marker *string

	for {
		out, err := client.DescribeReservedCacheNodes(ctx, &elasticache.DescribeReservedCacheNodesInput{
			Marker: marker,
		})
		if err != nil {
			return nil, err
		}
		for _, rn := range out.ReservedCacheNodes {
			if rn.Duration == nil {
				slog.Warn("ElastiCache RI missing duration", "id", aws.ToString(rn.ReservedCacheNodeId))
			}
			startTime := aws.ToTime(rn.StartTime)
			endTime := startTime.Add(time.Duration(aws.ToInt32(rn.Duration)) * time.Second)

			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(rn.ReservedCacheNodeId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				Type:         model.TypeCacheRI,
				ResourceID:   aws.ToString(rn.ReservedCacheNodeId),
				InstanceType: aws.ToString(rn.CacheNodeType),
				Platform:     aws.ToString(rn.ProductDescription),
				Quantity:     int(aws.ToInt32(rn.CacheNodeCount)),
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       aws.ToString(rn.State),
				Description:  fmt.Sprintf("ElastiCache RI - %s", aws.ToString(rn.OfferingType)),
			})
		}
		marker = out.Marker
		if marker == nil {
			break
		}
	}
	return results, nil
}

func fetchRedshiftReservedNodes(ctx context.Context, cfg aws.Config, acct config.Account, region string) ([]model.Reservation, error) {
	client := redshift.NewFromConfig(cfg)
	var results []model.Reservation
	var marker *string

	for {
		out, err := client.DescribeReservedNodes(ctx, &redshift.DescribeReservedNodesInput{
			Marker: marker,
		})
		if err != nil {
			return nil, err
		}
		for _, rn := range out.ReservedNodes {
			if rn.Duration == nil {
				slog.Warn("Redshift RI missing duration", "id", aws.ToString(rn.ReservedNodeId))
			}
			startTime := aws.ToTime(rn.StartTime)
			endTime := startTime.Add(time.Duration(aws.ToInt32(rn.Duration)) * time.Second)

			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(rn.ReservedNodeId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				Type:         model.TypeRedshiftRI,
				ResourceID:   aws.ToString(rn.ReservedNodeId),
				InstanceType: aws.ToString(rn.NodeType),
				Platform:     "Redshift",
				Quantity:     int(aws.ToInt32(rn.NodeCount)),
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       aws.ToString(rn.State),
				Description:  fmt.Sprintf("Redshift RI - %s", aws.ToString(rn.OfferingType)),
			})
		}
		marker = out.Marker
		if marker == nil {
			break
		}
	}
	return results, nil
}

func fetchOpenSearchReservedInstances(ctx context.Context, cfg aws.Config, acct config.Account, region string) ([]model.Reservation, error) {
	client := opensearch.NewFromConfig(cfg)
	var results []model.Reservation
	var nextToken *string

	for {
		out, err := client.DescribeReservedInstances(ctx, &opensearch.DescribeReservedInstancesInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, ri := range out.ReservedInstances {
			startTime := aws.ToTime(ri.StartTime)
			endTime := startTime.Add(time.Duration(ri.Duration) * time.Second)

			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(ri.ReservedInstanceId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				Type:         model.TypeOpenSearchRI,
				ResourceID:   aws.ToString(ri.ReservedInstanceId),
				InstanceType: string(ri.InstanceType),
				Platform:     "OpenSearch",
				Quantity:     int(ri.InstanceCount),
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       aws.ToString(ri.State),
				Description:  fmt.Sprintf("OpenSearch RI - %s", string(ri.PaymentOption)),
			})
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, nil
}

func fetchMemoryDBReservedNodes(ctx context.Context, cfg aws.Config, acct config.Account, region string) ([]model.Reservation, error) {
	client := memorydb.NewFromConfig(cfg)
	var results []model.Reservation
	var nextToken *string

	for {
		out, err := client.DescribeReservedNodes(ctx, &memorydb.DescribeReservedNodesInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, rn := range out.ReservedNodes {
			startTime := aws.ToTime(rn.StartTime)
			endTime := startTime.Add(time.Duration(rn.Duration) * time.Second)

			results = append(results, model.Reservation{
				ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, region, aws.ToString(rn.ReservationId)),
				AccountAlias: acct.Alias,
				AccountID:    acct.AccountID,
				Region:       region,
				Type:         model.TypeMemoryDBRI,
				ResourceID:   aws.ToString(rn.ReservationId),
				InstanceType: aws.ToString(rn.NodeType),
				Platform:     "MemoryDB",
				Quantity:     int(rn.NodeCount),
				StartTime:    startTime,
				EndTime:      endTime,
				Status:       aws.ToString(rn.State),
				Description:  fmt.Sprintf("MemoryDB RI - %s", aws.ToString(rn.OfferingType)),
			})
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, nil
}
