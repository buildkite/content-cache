package main

import (
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func TestServeCmdValidateMavenUpstream(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		urls    []string
		wantErr string
	}{
		{name: "unset falls back to default", urls: nil},
		{name: "single valid", urls: []string{"https://repo.maven.apache.org/maven2"}},
		{name: "multiple valid", urls: []string{"https://a.example/m", "http://b.example"}},
		{name: "trailing slash ok", urls: []string{"https://a.example/m/"}},
		{name: "whitespace trimmed", urls: []string{"  https://a.example  "}},

		{name: "empty string", urls: []string{""}, wantErr: "URL is empty"},
		{name: "whitespace only", urls: []string{"   "}, wantErr: "URL is empty"},
		{name: "second empty", urls: []string{"https://a.example", ""}, wantErr: "--maven-upstream[1]"},
		{name: "missing scheme", urls: []string{"repo.maven.apache.org"}, wantErr: "scheme must be"},
		{name: "wrong scheme", urls: []string{"ftp://a.example"}, wantErr: "scheme must be"},
		{name: "missing host", urls: []string{"https://"}, wantErr: "missing host"},
		{name: "duplicate", urls: []string{"https://a.example/m", "https://a.example/m/"}, wantErr: "duplicated"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &ServeCmd{MavenUpstream: tc.urls, MetadataMaxSize: metadb.MaxPayloadSize, MetadataBatchSize: metadb.DefaultBatchSize, MetadataBatchDelay: metadb.DefaultBatchDelay}
			err := cmd.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestMetadataMaxSizeCLI(t *testing.T) {
	t.Setenv("METADATA_MAX_SIZE", "16777216")
	var cli CLI
	parser, err := kong.New(&cli)
	require.NoError(t, err)
	_, err = parser.Parse([]string{"serve"})
	require.NoError(t, err)
	require.Equal(t, 16*1024*1024, cli.Serve.MetadataMaxSize)
	_, err = parser.Parse([]string{"serve", "--metadata-max-size=33554432"})
	require.NoError(t, err)
	require.Equal(t, 32*1024*1024, cli.Serve.MetadataMaxSize)
	for _, size := range []string{"0", "-1", "268435457"} {
		_, err = parser.Parse([]string{"serve", "--metadata-max-size=" + size})
		require.Error(t, err)
	}
}

func TestMetadataBatchCLI(t *testing.T) {
	t.Setenv("METADATA_BATCH_SIZE", "16")
	t.Setenv("METADATA_BATCH_DELAY", "2ms")
	var cli CLI
	parser, err := kong.New(&cli)
	require.NoError(t, err)
	_, err = parser.Parse([]string{"serve"})
	require.NoError(t, err)
	require.Equal(t, 16, cli.Serve.MetadataBatchSize)
	require.Equal(t, 2*time.Millisecond, cli.Serve.MetadataBatchDelay)
	_, err = parser.Parse([]string{"serve", "--metadata-batch-size=1", "--metadata-batch-delay=1ms"})
	require.NoError(t, err)
	require.Equal(t, 1, cli.Serve.MetadataBatchSize)
	require.Equal(t, time.Millisecond, cli.Serve.MetadataBatchDelay)
	for _, flag := range []string{"--metadata-batch-size=0", "--metadata-batch-size=-1", "--metadata-batch-delay=0s", "--metadata-batch-delay=-1ms"} {
		_, err = parser.Parse([]string{"serve", flag})
		require.Error(t, err)
	}
}
