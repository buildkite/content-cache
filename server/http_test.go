package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/credentials"
	"github.com/buildkite/content-cache/protocol/goproxy"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsGitHubAppRoutesWithoutTrustedSingleTenant(t *testing.T) {
	_, err := New(Config{
		Credentials: credentialsWithGitHubAppRoute(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "trusted single-tenant")
}

func TestValidateGitUpstreamAuth_AllowsTrustedSingleTenantGitHubApp(t *testing.T) {
	err := validateGitUpstreamAuth(Config{
		Credentials:                        credentialsWithGitHubAppRoute(),
		GitUpstreamAuthTrustedSingleTenant: true,
	})
	require.NoError(t, err)
}

func TestValidateGitUpstreamAuth_RejectsInvalidGitHubAppRoutes(t *testing.T) {
	tests := []struct {
		name    string
		route   credentials.GitRoute
		wantErr string
	}{
		{
			name: "combined with static auth",
			route: credentials.GitRoute{
				Match:    credentials.GitRouteMatch{RepoPrefix: "github.com/buildkite/"},
				Username: "x-access-token",
				Password: "pat",
				GitHubApp: &credentials.GitHubAppConfig{
					AppID:          "12345",
					InstallationID: "67890",
					PrivateKey:     "private key",
					TokenScope:     "requested_repo",
				},
			},
			wantErr: "cannot be combined",
		},
		{
			name: "catch-all",
			route: credentials.GitRoute{
				Match: credentials.GitRouteMatch{Any: true},
				GitHubApp: &credentials.GitHubAppConfig{
					AppID:          "12345",
					InstallationID: "67890",
					PrivateKey:     "private key",
					TokenScope:     "requested_repo",
				},
			},
			wantErr: "catch-all",
		},
		{
			name: "non github prefix",
			route: credentials.GitRoute{
				Match: credentials.GitRouteMatch{RepoPrefix: "gitlab.com/buildkite/"},
				GitHubApp: &credentials.GitHubAppConfig{
					AppID:          "12345",
					InstallationID: "67890",
					PrivateKey:     "private key",
					TokenScope:     "requested_repo",
				},
			},
			wantErr: "github.com",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitUpstreamAuth(Config{
				Credentials: &credentials.Credentials{
					Git: &credentials.GitAuthConfig{
						Routes: []credentials.GitRoute{
							tc.route,
							{Match: credentials.GitRouteMatch{Any: true}},
						},
					},
				},
				GitUpstreamAuthTrustedSingleTenant: true,
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNewConfiguresGoProxyImmutableIndexesWithoutExpiry(t *testing.T) {
	s, err := New(Config{
		StoragePath:        t.TempDir(),
		GoProxyMetadataTTL: time.Hour,
		OCIPrefix:          "docker-hub",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, s.metaDB.Close())
	})

	ctx := context.Background()
	require.NoError(t, s.index.PutModuleVersion(ctx, "github.com/buildkite/example", "v1.2.3", &goproxy.ModuleVersion{
		Info: goproxy.VersionInfo{
			Version: "v1.2.3",
			Time:    time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		},
		ZipHash: contentcache.HashBytes([]byte("zip")),
	}, []byte("module github.com/buildkite/example\n")))

	versionKey := "github.com/buildkite/example@v1.2.3"
	modEnv, err := s.metaDB.GetEnvelope(ctx, "goproxy", "mod", versionKey)
	require.NoError(t, err)
	require.Zero(t, modEnv.ExpiresAtUnixMs)

	infoEnv, err := s.metaDB.GetEnvelope(ctx, "goproxy", "info", versionKey)
	require.NoError(t, err)
	require.Zero(t, infoEnv.ExpiresAtUnixMs)

	listEnv, err := s.metaDB.GetEnvelope(ctx, "goproxy", "list", "github.com/buildkite/example")
	require.NoError(t, err)
	require.NotZero(t, listEnv.ExpiresAtUnixMs)
	require.Equal(t, int64(time.Hour.Seconds()), listEnv.TtlSeconds)
}

func credentialsWithGitHubAppRoute() *credentials.Credentials {
	return &credentials.Credentials{
		Git: &credentials.GitAuthConfig{
			Routes: []credentials.GitRoute{
				{
					Match: credentials.GitRouteMatch{RepoPrefix: "github.com/buildkite/"},
					GitHubApp: &credentials.GitHubAppConfig{
						AppID:          "12345",
						InstallationID: "67890",
						PrivateKey:     "private key",
						TokenScope:     "requested_repo",
					},
				},
				{Match: credentials.GitRouteMatch{Any: true}},
			},
		},
	}
}
