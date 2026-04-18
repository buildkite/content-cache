package rubygems

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func newTestIndex(t *testing.T) (*Index, *metadb.BoltDB) {
	t.Helper()
	tmpDir := t.TempDir()

	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "test.db")))
	t.Cleanup(func() { _ = db.Close() })

	versionsIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "versions", 5*time.Minute)
	require.NoError(t, err)
	infoIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "info", 5*time.Minute)
	require.NoError(t, err)
	specsIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "specs", 5*time.Minute)
	require.NoError(t, err)
	gemIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "gem", 24*time.Hour)
	require.NoError(t, err)
	gemspecIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "gemspec", 24*time.Hour)
	require.NoError(t, err)

	b, err := backend.NewFilesystem(filepath.Join(tmpDir, "blobs"))
	require.NoError(t, err)
	st := store.NewCAFS(b, store.WithMetaDB(db))

	return NewIndex(versionsIdx, infoIdx, specsIdx, gemIdx, gemspecIdx, st), db
}

func TestIndexVersions(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()

	// Get should fail initially
	_, _, err := idx.GetVersions(ctx)
	require.ErrorIs(t, err, ErrNotFound)

	// Put versions
	meta := &CachedVersions{
		ETag:       `"abc123"`,
		ReprDigest: "sha256digest",
		Size:       100,
		CachedAt:   time.Now(),
		UpdatedAt:  time.Now(),
	}
	content := []byte("---\nrails 7.0.0 abc123\n")

	err = idx.PutVersions(ctx, meta, content)
	require.NoError(t, err)

	// Get should succeed
	gotMeta, gotContent, err := idx.GetVersions(ctx)
	require.NoError(t, err)
	require.Equal(t, meta.ETag, gotMeta.ETag)
	require.Equal(t, string(content), string(gotContent))
}

func TestIndexVersionsAppend(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()

	// Initial versions
	meta := &CachedVersions{
		ETag:      `"v1"`,
		Size:      10,
		CachedAt:  time.Now(),
		UpdatedAt: time.Now(),
	}
	content := []byte("line1\n")
	require.NoError(t, idx.PutVersions(ctx, meta, content))

	// Append content
	appendMeta := &CachedVersions{
		ETag:      `"v2"`,
		CachedAt:  time.Now(),
		UpdatedAt: time.Now(),
	}
	appendContent := []byte("line2\n")
	require.NoError(t, idx.AppendVersions(ctx, appendMeta, appendContent))

	// Verify appended content
	_, gotContent, err := idx.GetVersions(ctx)
	require.NoError(t, err)
	require.Equal(t, "line1\nline2\n", string(gotContent))
}

func TestIndexVersionsLargeContent(t *testing.T) {
	idx, db := newTestIndex(t)
	ctx := context.Background()

	meta := &CachedVersions{
		ETag:      `"large"`,
		CachedAt:  time.Now(),
		UpdatedAt: time.Now(),
	}
	content := bytes.Repeat([]byte("v"), metadb.MaxPayloadSize+1)

	require.NoError(t, idx.PutVersions(ctx, meta, content))

	gotMeta, gotContent, err := idx.GetVersions(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), gotMeta.Size)
	require.Equal(t, content, gotContent)
	require.False(t, gotMeta.ContentHash.IsZero())

	env, err := db.GetEnvelope(ctx, "rubygems", "versions", versionsKey)
	require.NoError(t, err)
	require.Equal(t, metadb.ContentType_CONTENT_TYPE_JSON, env.ContentType)
	require.Len(t, env.BlobRefs, 1)
}

func TestIndexVersionsLegacyEnvelopeFallback(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()

	content := []byte("---\nrails 7.0.0 abc123\n")
	require.NoError(t, idx.versionsIndex.PutWithOptions(
		ctx,
		versionsKey,
		content,
		metadb.ContentType_CONTENT_TYPE_TEXT,
		nil,
		metadb.PutOptions{Etag: `"legacy"`},
	))

	gotMeta, gotContent, err := idx.GetVersions(ctx)
	require.NoError(t, err)
	require.Equal(t, `"legacy"`, gotMeta.ETag)
	require.Equal(t, content, gotContent)
}

func TestIndexInfo(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()
	gem := "rails"

	// Get should fail initially
	_, _, err := idx.GetInfo(ctx, gem)
	require.ErrorIs(t, err, ErrNotFound)

	// Put info
	meta := &CachedGemInfo{
		Name:       gem,
		ETag:       `"rails-etag"`,
		ReprDigest: "sha256",
		Size:       200,
		Checksums: map[string]string{
			"7.0.0":              "abc123",
			"7.0.0-x86_64-linux": "def456",
		},
		MD5:       "md5hash",
		CachedAt:  time.Now(),
		UpdatedAt: time.Now(),
	}
	content := []byte("---\n7.0.0 |checksum:abc123\n")

	err = idx.PutInfo(ctx, gem, meta, content)
	require.NoError(t, err)

	// Get should succeed
	gotMeta, gotContent, err := idx.GetInfo(ctx, gem)
	require.NoError(t, err)
	require.Equal(t, gem, gotMeta.Name)
	require.Equal(t, meta.ETag, gotMeta.ETag)
	require.Equal(t, string(content), string(gotContent))
	require.Equal(t, "abc123", gotMeta.Checksums["7.0.0"])
	require.Equal(t, "def456", gotMeta.Checksums["7.0.0-x86_64-linux"])
	require.Equal(t, "md5hash", gotMeta.MD5)
}

func TestIndexInfoLargeChecksums(t *testing.T) {
	idx, db := newTestIndex(t)
	ctx := context.Background()
	gem := "rails"

	checksums := make(map[string]string)
	for i := 0; i < 64; i++ {
		checksums[fmt.Sprintf("7.0.%d", i)] = strings.Repeat("a", 64)
	}

	meta := &CachedGemInfo{
		Name:       gem,
		ETag:       `"rails-etag"`,
		ReprDigest: "sha256",
		Checksums:  checksums,
		MD5:        "md5hash",
		CachedAt:   time.Now(),
		UpdatedAt:  time.Now(),
	}
	content := []byte("---\n7.0.0 |checksum:abc123\n")

	require.NoError(t, idx.PutInfo(ctx, gem, meta, content))

	gotMeta, gotContent, err := idx.GetInfo(ctx, gem)
	require.NoError(t, err)
	require.Equal(t, string(content), string(gotContent))
	require.Equal(t, checksums, gotMeta.Checksums)
	require.False(t, gotMeta.ContentHash.IsZero())

	env, err := db.GetEnvelope(ctx, "rubygems", "info", gem)
	require.NoError(t, err)
	require.Equal(t, metadb.ContentType_CONTENT_TYPE_JSON, env.ContentType)
	require.Len(t, env.BlobRefs, 1)
}

func TestIndexInfoLegacyEnvelopeFallback(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()
	gem := "rails"
	content := []byte("---\n7.0.0 |checksum:abc123\n")

	require.NoError(t, idx.infoIndex.PutWithOptions(
		ctx,
		gem,
		content,
		metadb.ContentType_CONTENT_TYPE_TEXT,
		nil,
		metadb.PutOptions{Etag: `"legacy-info"`},
	))
	require.NoError(t, idx.infoIndex.Update(ctx, gem, func(data []byte, env *metadb.MetadataEnvelope) ([]byte, []string, error) {
		if env.Attributes == nil {
			env.Attributes = make(map[string][]byte)
		}
		env.Attributes["checksums"] = encodeChecksums(map[string]string{"7.0.0": "abc123"})
		env.Attributes["md5"] = []byte("md5hash")
		return data, nil, nil
	}))

	gotMeta, gotContent, err := idx.GetInfo(ctx, gem)
	require.NoError(t, err)
	require.Equal(t, `"legacy-info"`, gotMeta.ETag)
	require.Equal(t, "abc123", gotMeta.Checksums["7.0.0"])
	require.Equal(t, "md5hash", gotMeta.MD5)
	require.Equal(t, content, gotContent)
}

func TestIndexSpecs(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()

	specsTypes := []string{"specs", "latest_specs", "prerelease_specs"}

	for _, specsType := range specsTypes {
		t.Run(specsType, func(t *testing.T) {
			// Get should fail initially
			_, _, err := idx.GetSpecs(ctx, specsType)
			require.ErrorIs(t, err, ErrNotFound)

			// Put specs
			meta := &CachedSpecs{
				Type:       specsType,
				ETag:       `"specs-etag"`,
				ReprDigest: "sha256",
				Size:       500,
				CachedAt:   time.Now(),
				UpdatedAt:  time.Now(),
			}
			content := []byte{0x1f, 0x8b} // gzip magic bytes

			err = idx.PutSpecs(ctx, specsType, meta, content)
			require.NoError(t, err)

			// Get should succeed
			gotMeta, gotContent, err := idx.GetSpecs(ctx, specsType)
			require.NoError(t, err)
			require.Equal(t, specsType, gotMeta.Type)
			require.Equal(t, meta.ETag, gotMeta.ETag)
			require.Equal(t, content, gotContent)
		})
	}
}

func TestIndexGem(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()

	// Get should fail initially
	_, err := idx.GetGem(ctx, "rails-7.0.0.gem")
	require.ErrorIs(t, err, ErrNotFound)

	// Put gem
	gem := &CachedGem{
		Name:        "rails",
		Version:     "7.0.0",
		Platform:    "ruby",
		Filename:    "rails-7.0.0.gem",
		ContentHash: contentcache.HashBytes([]byte("gem content")),
		Size:        1024,
		SHA256:      "sha256hash",
		CachedAt:    time.Now(),
	}

	err = idx.PutGem(ctx, gem)
	require.NoError(t, err)

	// Get should succeed
	got, err := idx.GetGem(ctx, "rails-7.0.0.gem")
	require.NoError(t, err)
	require.Equal(t, gem.Name, got.Name)
	require.Equal(t, gem.Version, got.Version)
	require.Equal(t, gem.Filename, got.Filename)
	require.Equal(t, gem.ContentHash, got.ContentHash)
	require.Equal(t, gem.SHA256, got.SHA256)
}

func TestIndexGemspec(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()

	// Get should fail initially
	_, err := idx.GetGemspec(ctx, "rails", "7.0.0", "ruby")
	require.ErrorIs(t, err, ErrNotFound)

	// Put gemspec
	spec := &CachedGemspec{
		Name:        "rails",
		Version:     "7.0.0",
		Platform:    "ruby",
		ContentHash: contentcache.HashBytes([]byte("gemspec content")),
		Size:        512,
		CachedAt:    time.Now(),
	}

	err = idx.PutGemspec(ctx, spec)
	require.NoError(t, err)

	// Get should succeed
	got, err := idx.GetGemspec(ctx, "rails", "7.0.0", "ruby")
	require.NoError(t, err)
	require.Equal(t, spec.Name, got.Name)
	require.Equal(t, spec.Version, got.Version)
	require.Equal(t, spec.ContentHash, got.ContentHash)
}

func TestIndexGemspecWithPlatform(t *testing.T) {
	idx, _ := newTestIndex(t)
	ctx := context.Background()

	// Put gemspec with platform
	spec := &CachedGemspec{
		Name:        "nokogiri",
		Version:     "1.13.0",
		Platform:    "x86_64-linux",
		ContentHash: contentcache.HashBytes([]byte("nokogiri gemspec")),
		Size:        256,
		CachedAt:    time.Now(),
	}

	err := idx.PutGemspec(ctx, spec)
	require.NoError(t, err)

	// Get should succeed with platform
	got, err := idx.GetGemspec(ctx, "nokogiri", "1.13.0", "x86_64-linux")
	require.NoError(t, err)
	require.Equal(t, "x86_64-linux", got.Platform)

	// Get with different platform should fail
	_, err = idx.GetGemspec(ctx, "nokogiri", "1.13.0", "ruby")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestIndexIsExpired(t *testing.T) {
	idx, _ := newTestIndex(t)

	// Recent time should not be expired
	recent := time.Now().Add(-1 * time.Minute)
	require.False(t, idx.IsExpired(recent, 5*time.Minute))

	// Old time should be expired
	old := time.Now().Add(-10 * time.Minute)
	require.True(t, idx.IsExpired(old, 5*time.Minute))
}

func TestIndexGemBlobRefTracking(t *testing.T) {
	idx, db := newTestIndex(t)
	ctx := context.Background()

	hash := contentcache.HashBytes([]byte("gem content"))
	hashWithPrefix := contentcache.NewBlobRef(hash).String()

	// Create the blob entry
	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:     hashWithPrefix,
		Size:     1024,
		RefCount: 0,
	}))

	// Put gem - should increment ref
	gem := &CachedGem{
		Name:        "rails",
		Version:     "7.0.0",
		Platform:    "ruby",
		Filename:    "rails-7.0.0.gem",
		ContentHash: hash,
		Size:        1024,
		CachedAt:    time.Now(),
	}
	require.NoError(t, idx.PutGem(ctx, gem))

	// Verify ref was incremented
	blob, err := db.GetBlob(ctx, hashWithPrefix)
	require.NoError(t, err)
	require.Equal(t, 1, blob.RefCount)
}

func TestIndexGemspecBlobRefTracking(t *testing.T) {
	idx, db := newTestIndex(t)
	ctx := context.Background()

	hash := contentcache.HashBytes([]byte("gemspec content"))
	hashWithPrefix := contentcache.NewBlobRef(hash).String()

	// Create the blob entry
	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:     hashWithPrefix,
		Size:     512,
		RefCount: 0,
	}))

	// Put gemspec - should increment ref
	spec := &CachedGemspec{
		Name:        "rails",
		Version:     "7.0.0",
		Platform:    "ruby",
		ContentHash: hash,
		Size:        512,
		CachedAt:    time.Now(),
	}
	require.NoError(t, idx.PutGemspec(ctx, spec))

	// Verify ref was incremented
	blob, err := db.GetBlob(ctx, hashWithPrefix)
	require.NoError(t, err)
	require.Equal(t, 1, blob.RefCount)
}

func TestGemspecFilename(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		platform string
		want     string
	}{
		{"rails", "7.0.0", "ruby", "rails-7.0.0"},
		{"rails", "7.0.0", "", "rails-7.0.0"},
		{"nokogiri", "1.13.0", "x86_64-linux", "nokogiri-1.13.0-x86_64-linux"},
		{"ffi", "1.15.0", "java", "ffi-1.15.0-java"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := gemspecFilename(tt.name, tt.version, tt.platform)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEncodeDecodeChecksums(t *testing.T) {
	tests := []struct {
		name      string
		checksums map[string]string
	}{
		{
			name:      "empty",
			checksums: map[string]string{},
		},
		{
			name: "single",
			checksums: map[string]string{
				"7.0.0": "abc123",
			},
		},
		{
			name: "multiple",
			checksums: map[string]string{
				"7.0.0":              "abc123",
				"7.0.0-x86_64-linux": "def456",
				"6.1.0":              "ghi789",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.checksums) == 0 {
				require.Nil(t, encodeChecksums(tt.checksums))
				return
			}

			encoded := encodeChecksums(tt.checksums)
			require.NotNil(t, encoded)

			decoded := decodeChecksums(encoded)
			require.Equal(t, tt.checksums, decoded)
		})
	}
}
