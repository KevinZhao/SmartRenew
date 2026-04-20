package provider

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// ResolveAccountID calls sts:GetCallerIdentity to discover the AWS account ID
// for a given set of credentials. Used to auto-populate Account.AccountID when
// the config omits it.
func ResolveAccountID(ctx context.Context, cfg aws.Config) (string, error) {
	client := sts.NewFromConfig(cfg)
	out, err := client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("sts GetCallerIdentity: %w", err)
	}
	return aws.ToString(out.Account), nil
}
