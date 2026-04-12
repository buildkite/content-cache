package pypi

import (
	"testing"

	contentcache "github.com/buildkite/content-cache"
	"github.com/stretchr/testify/require"
)

func TestCollectBlobRefs(t *testing.T) {
	// Helper to create a hash from a hex string (must be 64 hex chars for BLAKE3)
	makeHash := func(hexStr string) contentcache.Hash {
		h, err := contentcache.ParseHash(hexStr)
		require.NoError(t, err)
		return h
	}

	tests := []struct {
		name     string
		project  *CachedProject
		wantRefs []string
	}{
		{
			name:     "nil project returns nil",
			project:  nil,
			wantRefs: nil,
		},
		{
			name:     "nil files returns nil",
			project:  &CachedProject{Name: "test"},
			wantRefs: nil,
		},
		{
			name:     "empty files returns empty slice",
			project:  &CachedProject{Name: "test", Files: map[string]*CachedFile{}},
			wantRefs: []string{},
		},
		{
			name: "single file with hash",
			project: &CachedProject{
				Name: "test",
				Files: map[string]*CachedFile{
					"test-1.0.0.whl": {
						Filename:    "test-1.0.0.whl",
						ContentHash: makeHash("1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"),
					},
				},
			},
			wantRefs: []string{"blake3:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"},
		},
		{
			name: "multiple files with hashes",
			project: &CachedProject{
				Name: "test",
				Files: map[string]*CachedFile{
					"test-1.0.0.whl": {
						Filename:    "test-1.0.0.whl",
						ContentHash: makeHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
					},
					"test-1.0.0.tar.gz": {
						Filename:    "test-1.0.0.tar.gz",
						ContentHash: makeHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
					},
				},
			},
			wantRefs: []string{
				"blake3:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"blake3:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
		},
		{
			name: "file with zero hash is skipped",
			project: &CachedProject{
				Name: "test",
				Files: map[string]*CachedFile{
					"test-1.0.0.whl": {
						Filename:    "test-1.0.0.whl",
						ContentHash: contentcache.Hash{}, // Zero hash
					},
				},
			},
			wantRefs: []string{},
		},
		{
			name: "nil file entry is skipped",
			project: &CachedProject{
				Name: "test",
				Files: map[string]*CachedFile{
					"test-1.0.0.whl": nil,
				},
			},
			wantRefs: []string{},
		},
		{
			name: "mixed: valid hash, zero hash, and nil entry",
			project: &CachedProject{
				Name: "test",
				Files: map[string]*CachedFile{
					"valid.whl": {ContentHash: makeHash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")},
					"zero.whl":  {ContentHash: contentcache.Hash{}},
					"nil-entry": nil,
				},
			},
			wantRefs: []string{"blake3:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectBlobRefs(tt.project)

			if tt.wantRefs == nil {
				require.Nil(t, got)
				return
			}

			require.NotNil(t, got)
			// Use ElementsMatch since map iteration order is non-deterministic
			require.ElementsMatch(t, tt.wantRefs, got)
		})
	}
}
