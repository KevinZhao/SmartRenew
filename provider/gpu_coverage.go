package provider

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"

	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/model"
)

// spCoverageTolerance treats an instance as fully covered when remaining
// commitment is within this fraction of the per-instance rate. Absorbs
// AWS commitment rounding (e.g. 395.17869 vs 19×20.8=395.2, a 0.006% gap).
const spCoverageTolerance = 0.995

var gpuFamilies = map[string]bool{
	// P-series: NVIDIA training/inference
	"p3": true, "p4d": true, "p4de": true,
	"p5": true, "p5e": true, "p5en": true,
	"p6-b200": true, "p6-b300": true, "p6e-gb200": true, "p6e-gb300": true,
	// G-series: NVIDIA graphics/inference
	"g4dn": true, "g4ad": true, "g5": true, "g5g": true,
	"g6": true, "g6e": true, "g6f": true, "gr6": true, "gr6f": true, "g7e": true,
	// AWS Inferentia
	"inf1": true, "inf2": true,
	// AWS Trainium
	"trn1": true, "trn1n": true, "trn2": true, "trn2u": true,
	// Deep Learning
	"dl1": true, "dl2q": true,
}

func instanceFamily(instanceType string) string {
	if idx := strings.LastIndex(instanceType, "."); idx > 0 {
		return instanceType[:idx]
	}
	// UltraServer naming: u-p6e-gb200x72 → p6e-gb200
	if strings.HasPrefix(instanceType, "u-") {
		name := instanceType[2:]
		if idx := strings.LastIndex(name, "x"); idx > 0 {
			return name[:idx]
		}
		return name
	}
	return instanceType
}

func isGPUInstance(instanceType string) bool {
	return gpuFamilies[instanceFamily(instanceType)]
}

type runningInstance struct {
	InstanceID            string
	InstanceType          string
	Family                string
	Region                string
	AZ                    string
	Platform              string
	CapacityReservationID string
}

type spDetail struct {
	ID         string
	SPType     string // "EC2Instance" or "Compute"
	Family     string
	Region     string
	Commitment float64
	Rates      map[string]float64 // instanceType -> PPA rate $/hr
}

type riInfo struct {
	ID           string
	InstanceType string
	Region       string
	Count        int
}

func CheckGPUCoverage(ctx context.Context, acct config.Account) ([]model.GPUCoverage, []error) {
	if len(acct.Regions) == 0 {
		return nil, []error{fmt.Errorf("%s: no regions configured", acct.Alias)}
	}

	var errs []error

	// SP is global — fetch once using the first region
	spCfg := buildAWSConfig(acct, acct.Regions[0])
	sps, err := fetchActiveSPsWithRates(ctx, spCfg)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s/SP-rates: %w", acct.Alias, err))
	}

	var allInstances []runningInstance
	var allRIs []riInfo
	allCBIDs := make(map[string]bool)

	for _, region := range acct.Regions {
		cfg := buildAWSConfig(acct, region)

		instances, err := fetchRunningInstances(ctx, cfg, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/instances: %w", acct.Alias, region, err))
			continue
		}
		allInstances = append(allInstances, instances...)

		ris, err := fetchActiveRIs(ctx, cfg, region)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/RI: %w", acct.Alias, region, err))
		} else {
			allRIs = append(allRIs, ris...)
		}

		cbIDs, err := fetchActiveCBIDs(ctx, cfg)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s/CB: %w", acct.Alias, region, err))
		} else {
			for k, v := range cbIDs {
				allCBIDs[k] = v
			}
		}
	}

	results := analyzeCoverage(allInstances, sps, allRIs, allCBIDs, acct)
	return results, errs
}

func fetchRunningInstances(ctx context.Context, cfg aws.Config, region string) ([]runningInstance, error) {
	client := ec2.NewFromConfig(cfg)
	var results []runningInstance
	var nextToken *string

	for {
		out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("instance-state-name"), Values: []string{"running"}},
			},
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, rsv := range out.Reservations {
			for _, inst := range rsv.Instances {
				platform := "Linux/UNIX"
				if inst.Platform == ec2types.PlatformValuesWindows {
					platform = "Windows"
				}
				itype := string(inst.InstanceType)

				var crID string
				if spec := inst.CapacityReservationSpecification; spec != nil {
					if target := spec.CapacityReservationTarget; target != nil {
						crID = aws.ToString(target.CapacityReservationId)
					}
				}

				results = append(results, runningInstance{
					InstanceID:            aws.ToString(inst.InstanceId),
					InstanceType:          itype,
					Family:                instanceFamily(itype),
					Region:                region,
					AZ:                    aws.ToString(inst.Placement.AvailabilityZone),
					Platform:              platform,
					CapacityReservationID: crID,
				})
			}
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, nil
}

func fetchActiveSPsWithRates(ctx context.Context, cfg aws.Config) ([]spDetail, error) {
	client := savingsplans.NewFromConfig(cfg)
	var results []spDetail
	var nextToken *string

	for {
		out, err := client.DescribeSavingsPlans(ctx, &savingsplans.DescribeSavingsPlansInput{
			States:    []sptypes.SavingsPlanState{sptypes.SavingsPlanStateActive},
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, sp := range out.SavingsPlans {
			commitment, _ := strconv.ParseFloat(aws.ToString(sp.Commitment), 64)

			detail := spDetail{
				ID:         aws.ToString(sp.SavingsPlanId),
				SPType:     string(sp.SavingsPlanType),
				Family:     aws.ToString(sp.Ec2InstanceFamily),
				Region:     aws.ToString(sp.Region),
				Commitment: commitment,
			}

			rates, err := fetchSPRates(ctx, cfg, detail.ID)
			if err != nil {
				slog.Warn("fetch SP rates failed, skipping", "sp_id", detail.ID, "err", err)
				continue
			}
			detail.Rates = rates
			results = append(results, detail)
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return results, nil
}

func fetchSPRates(ctx context.Context, cfg aws.Config, spID string) (map[string]float64, error) {
	client := savingsplans.NewFromConfig(cfg)
	rates := make(map[string]float64)
	var nextToken *string

	for {
		out, err := client.DescribeSavingsPlanRates(ctx, &savingsplans.DescribeSavingsPlanRatesInput{
			SavingsPlanId: &spID,
			Filters: []sptypes.SavingsPlanRateFilter{
				{Name: sptypes.SavingsPlanRateFilterNameServiceCode, Values: []string{"AmazonEC2"}},
				{Name: sptypes.SavingsPlanRateFilterNameProductDescription, Values: []string{"Linux/UNIX"}},
				{Name: sptypes.SavingsPlanRateFilterNameTenancy, Values: []string{"shared"}},
			},
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range out.SearchResults {
			usageType := aws.ToString(r.UsageType)
			if !strings.Contains(usageType, "BoxUsage") || strings.Contains(usageType, "UnusedBox") || strings.Contains(usageType, "UnusedDed") {
				continue
			}
			var itype string
			for _, p := range r.Properties {
				if p.Name == sptypes.SavingsPlanRatePropertyKeyInstanceType {
					itype = aws.ToString(p.Value)
					break
				}
			}
			if itype == "" {
				continue
			}
			rate, err := strconv.ParseFloat(aws.ToString(r.Rate), 64)
			if err != nil {
				continue
			}
			rates[itype] = rate
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return rates, nil
}

func fetchActiveRIs(ctx context.Context, cfg aws.Config, region string) ([]riInfo, error) {
	client := ec2.NewFromConfig(cfg)
	out, err := client.DescribeReservedInstances(ctx, &ec2.DescribeReservedInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("state"), Values: []string{"active"}},
		},
	})
	if err != nil {
		return nil, err
	}

	var results []riInfo
	for _, ri := range out.ReservedInstances {
		results = append(results, riInfo{
			ID:           aws.ToString(ri.ReservedInstancesId),
			InstanceType: string(ri.InstanceType),
			Region:       region,
			Count:        int(aws.ToInt32(ri.InstanceCount)),
		})
	}
	return results, nil
}

func fetchActiveCBIDs(ctx context.Context, cfg aws.Config) (map[string]bool, error) {
	client := ec2.NewFromConfig(cfg)
	result := make(map[string]bool)
	var nextToken *string

	for {
		out, err := client.DescribeCapacityReservations(ctx, &ec2.DescribeCapacityReservationsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, cr := range out.CapacityReservations {
			if cr.ReservationType == ec2types.CapacityReservationTypeCapacityBlock && cr.State == ec2types.CapacityReservationStateActive {
				result[aws.ToString(cr.CapacityReservationId)] = true
			}
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return result, nil
}

func analyzeCoverage(instances []runningInstance, sps []spDetail, ris []riInfo, cbIDs map[string]bool, acct config.Account) []model.GPUCoverage {
	type assignment struct {
		coverage    model.CoverageType
		coverageRef string
		spRate      float64
	}
	assigned := make(map[string]*assignment) // instanceID -> assignment

	// Step 1: CB assignment
	for i := range instances {
		inst := &instances[i]
		if inst.CapacityReservationID != "" && cbIDs[inst.CapacityReservationID] {
			assigned[inst.InstanceID] = &assignment{
				coverage:    model.CoverageCB,
				coverageRef: inst.CapacityReservationID,
			}
		}
	}

	// Step 2: RI assignment
	type riKey struct {
		InstanceType string
		Region       string
	}
	riMap := make(map[riKey]*riInfo)
	for i := range ris {
		k := riKey{ris[i].InstanceType, ris[i].Region}
		if existing, ok := riMap[k]; ok {
			existing.Count += ris[i].Count
		} else {
			ri := ris[i]
			riMap[k] = &ri
		}
	}
	for i := range instances {
		inst := &instances[i]
		if assigned[inst.InstanceID] != nil {
			continue
		}
		k := riKey{inst.InstanceType, inst.Region}
		if ri, ok := riMap[k]; ok && ri.Count > 0 {
			assigned[inst.InstanceID] = &assignment{
				coverage:    model.CoverageRI,
				coverageRef: ri.ID,
			}
			ri.Count--
		}
	}

	// Step 3: EC2 Instance SP assignment
	for si := range sps {
		sp := &sps[si]
		if sp.SPType != "EC2Instance" {
			continue
		}
		remaining := sp.Commitment

		// Collect uncovered instances matching this SP's family + region
		var candidates []int
		for i := range instances {
			if assigned[instances[i].InstanceID] != nil {
				continue
			}
			if instances[i].Family == sp.Family && instances[i].Region == sp.Region && instances[i].Platform == "Linux/UNIX" {
				candidates = append(candidates, i)
			}
		}
		// Sort by rate descending (highest first to maximize savings)
		sort.Slice(candidates, func(a, b int) bool {
			rateA := sp.Rates[instances[candidates[a]].InstanceType]
			rateB := sp.Rates[instances[candidates[b]].InstanceType]
			return rateA > rateB
		})
		for _, idx := range candidates {
			inst := &instances[idx]
			rate, ok := sp.Rates[inst.InstanceType]
			if !ok || rate <= 0 {
				continue
			}
			if remaining >= rate*spCoverageTolerance {
				assigned[inst.InstanceID] = &assignment{
					coverage:    model.CoverageSP,
					coverageRef: sp.ID,
					spRate:      rate,
				}
				if remaining < rate {
					slog.Info("sp coverage within tolerance, treating as fully covered",
						"sp_id", sp.ID, "instance", inst.InstanceID, "itype", inst.InstanceType,
						"remaining", remaining, "rate", rate, "gap_pct", (rate-remaining)/rate*100)
					remaining = 0
				} else {
					remaining -= rate
				}
			}
		}
	}

	// Step 4: Compute SP assignment
	for si := range sps {
		sp := &sps[si]
		if sp.SPType != "Compute" {
			continue
		}
		remaining := sp.Commitment

		var candidates []int
		for i := range instances {
			if assigned[instances[i].InstanceID] != nil {
				continue
			}
			if instances[i].Platform == "Linux/UNIX" {
				candidates = append(candidates, i)
			}
		}
		sort.Slice(candidates, func(a, b int) bool {
			rateA := sp.Rates[instances[candidates[a]].InstanceType]
			rateB := sp.Rates[instances[candidates[b]].InstanceType]
			return rateA > rateB
		})
		for _, idx := range candidates {
			inst := &instances[idx]
			rate, ok := sp.Rates[inst.InstanceType]
			if !ok || rate <= 0 {
				continue
			}
			if remaining >= rate*spCoverageTolerance {
				assigned[inst.InstanceID] = &assignment{
					coverage:    model.CoverageSP,
					coverageRef: sp.ID,
					spRate:      rate,
				}
				if remaining < rate {
					slog.Info("sp coverage within tolerance, treating as fully covered",
						"sp_id", sp.ID, "instance", inst.InstanceID, "itype", inst.InstanceType,
						"remaining", remaining, "rate", rate, "gap_pct", (rate-remaining)/rate*100)
					remaining = 0
				} else {
					remaining -= rate
				}
			}
		}
	}

	// Step 5: Build results for GPU instances only
	var results []model.GPUCoverage
	for i := range instances {
		inst := &instances[i]
		if !isGPUInstance(inst.InstanceType) {
			continue
		}

		g := model.GPUCoverage{
			ID:           fmt.Sprintf("%s_%s_%s", acct.AccountID, inst.Region, inst.InstanceID),
			AccountAlias: acct.Alias,
			AccountID:    acct.AccountID,
			Region:       inst.Region,
			AZ:           inst.AZ,
			InstanceID:   inst.InstanceID,
			InstanceType: inst.InstanceType,
			Coverage:     model.CoverageOnDemand,
		}

		if a, ok := assigned[inst.InstanceID]; ok {
			g.Coverage = a.coverage
			g.CoverageRef = a.coverageRef
			g.SPRate = a.spRate
		}

		results = append(results, g)
	}
	return results
}
