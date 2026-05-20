package main

import (
	"testing"

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
			cmd := &ServeCmd{MavenUpstream: tc.urls}
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
