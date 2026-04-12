package awsprovider

import (
	"context"
	"fmt"

	"github.com/buildkite/content-cache/credentials"
)

// SSMClient is the interface for AWS SSM Parameter Store operations.
type SSMClient interface {
	GetParameter(ctx context.Context, name string) (string, error)
}

// WithSSM registers an "ssm" template function that resolves secrets from AWS SSM Parameter Store.
func WithSSM(client SSMClient) credentials.ResolverOption {
	return credentials.WithProvider("ssm", func(ctx context.Context, ref string) (string, error) {
		val, err := client.GetParameter(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("SSM GetParameter %q: %w", ref, err)
		}
		return val, nil
	})
}
