package awsprovider

import (
	"context"
	"fmt"

	"github.com/buildkite/content-cache/credentials"
)

// SecretsManagerClient is the interface for AWS Secrets Manager operations.
type SecretsManagerClient interface {
	GetSecretValue(ctx context.Context, secretID string) (string, error)
}

// WithSecretsManager registers a "secretsmanager" template function that resolves secrets from AWS Secrets Manager.
func WithSecretsManager(client SecretsManagerClient) credentials.ResolverOption {
	return credentials.WithProvider("secretsmanager", func(ctx context.Context, ref string) (string, error) {
		val, err := client.GetSecretValue(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("SecretsManager GetSecretValue %q: %w", ref, err)
		}
		return val, nil
	})
}
