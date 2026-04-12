package awsprovider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/buildkite/content-cache/credentials"
	"github.com/stretchr/testify/require"
)

type mockSSMClient struct {
	params map[string]string
}

func (m *mockSSMClient) GetParameter(_ context.Context, name string) (string, error) {
	if val, ok := m.params[name]; ok {
		return val, nil
	}
	return "", fmt.Errorf("parameter not found: %s", name)
}

type mockSMClient struct {
	secrets map[string]string
}

func (m *mockSMClient) GetSecretValue(_ context.Context, secretID string) (string, error) {
	if val, ok := m.secrets[secretID]; ok {
		return val, nil
	}
	return "", fmt.Errorf("secret not found: %s", secretID)
}

func TestWithSSM(t *testing.T) {
	client := &mockSSMClient{
		params: map[string]string{
			"/prod/npm-token": "ssm-secret",
		},
	}

	input := `{"auth_token": {{ ssm "/prod/npm-token" | json }}}`
	r := credentials.NewResolver(WithSSM(client))
	creds, err := r.ResolveReader(context.Background(), strings.NewReader(input))
	require.NoError(t, err)
	require.Equal(t, "ssm-secret", creds.AuthToken)
}

func TestWithSSM_NotFound(t *testing.T) {
	client := &mockSSMClient{params: map[string]string{}}

	input := `{"auth_token": {{ ssm "/missing/param" | json }}}`
	r := credentials.NewResolver(WithSSM(client))
	_, err := r.ResolveReader(context.Background(), strings.NewReader(input))
	require.Error(t, err)
	require.Contains(t, err.Error(), "SSM GetParameter")
}

func TestWithSecretsManager(t *testing.T) {
	client := &mockSMClient{
		secrets: map[string]string{
			"prod/oci": "sm-secret",
		},
	}

	input := `{"auth_token": {{ secretsmanager "prod/oci" | json }}}`
	r := credentials.NewResolver(WithSecretsManager(client))
	creds, err := r.ResolveReader(context.Background(), strings.NewReader(input))
	require.NoError(t, err)
	require.Equal(t, "sm-secret", creds.AuthToken)
}

func TestWithSecretsManager_NotFound(t *testing.T) {
	client := &mockSMClient{secrets: map[string]string{}}

	input := `{"auth_token": {{ secretsmanager "missing/secret" | json }}}`
	r := credentials.NewResolver(WithSecretsManager(client))
	_, err := r.ResolveReader(context.Background(), strings.NewReader(input))
	require.Error(t, err)
	require.Contains(t, err.Error(), "SecretsManager GetSecretValue")
}
