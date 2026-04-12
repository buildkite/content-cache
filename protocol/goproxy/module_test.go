package goproxy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func TestEncodePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"github.com/pkg/errors", "github.com/pkg/errors"},
		{"github.com/Azure/azure-sdk", "github.com/!azure/azure-sdk"},
		{"github.com/BurntSushi/toml", "github.com/!burnt!sushi/toml"},
		{"golang.org/x/mod", "golang.org/x/mod"},
		{"Example.COM/Foo/Bar", "!example.!c!o!m/!foo/!bar"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := encodePath(tt.input)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestDecodePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		wantErr  bool
	}{
		{"github.com/pkg/errors", "github.com/pkg/errors", false},
		{"github.com/!azure/azure-sdk", "github.com/Azure/azure-sdk", false},
		{"github.com/!burnt!sushi/toml", "github.com/BurntSushi/toml", false},
		{"invalid!", "", true},  // Trailing escape
		{"invalid!1", "", true}, // Invalid escape sequence
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := decodePath(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	paths := []string{
		"github.com/pkg/errors",
		"github.com/Azure/azure-sdk-for-go",
		"github.com/BurntSushi/toml",
		"golang.org/x/mod",
		"Example.COM/Foo/Bar",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			encoded := encodePath(path)
			decoded, err := decodePath(encoded)
			require.NoError(t, err)
			require.Equal(t, path, decoded)
		})
	}
}

func TestIndexListVersions(t *testing.T) {
	idx, cleanup := newTestIndex(t)
	defer cleanup()

	ctx := context.Background()
	modulePath := "github.com/test/module"

	// Initially empty
	versions, err := idx.ListVersions(ctx, modulePath)
	require.NoError(t, err)
	require.Empty(t, versions)

	// Add some versions
	mv := &ModuleVersion{
		Info: VersionInfo{
			Version: "v1.0.0",
			Time:    time.Now(),
		},
		ZipHash: contentcache.HashBytes([]byte("fake zip")),
	}

	for _, v := range []string{"v1.0.0", "v1.0.1", "v1.1.0"} {
		mv.Info.Version = v
		err := idx.PutModuleVersion(ctx, modulePath, v, mv, []byte("module test"))
		require.NoError(t, err)
	}

	// List should return all versions
	versions, err = idx.ListVersions(ctx, modulePath)
	require.NoError(t, err)
	require.Len(t, versions, 3)
}

func TestIndexGetVersionInfo(t *testing.T) {
	idx, cleanup := newTestIndex(t)
	defer cleanup()

	ctx := context.Background()
	modulePath := "github.com/test/module"
	version := "v1.2.3"

	// Not found initially
	_, err := idx.GetVersionInfo(ctx, modulePath, version)
	require.ErrorIs(t, err, ErrNotFound)

	// Store version
	now := time.Now().Truncate(time.Second)
	mv := &ModuleVersion{
		Info: VersionInfo{
			Version: version,
			Time:    now,
		},
		ZipHash: contentcache.HashBytes([]byte("zip content")),
	}
	modFile := []byte("module github.com/test/module\n\ngo 1.21\n")

	err = idx.PutModuleVersion(ctx, modulePath, version, mv, modFile)
	require.NoError(t, err)

	// Get version info
	info, err := idx.GetVersionInfo(ctx, modulePath, version)
	require.NoError(t, err)
	require.Equal(t, version, info.Version)
	require.True(t, info.Time.Equal(now))
}

func TestIndexGetMod(t *testing.T) {
	idx, cleanup := newTestIndex(t)
	defer cleanup()

	ctx := context.Background()
	modulePath := "github.com/test/module"
	version := "v1.0.0"
	modContent := []byte("module github.com/test/module\n\ngo 1.21\n")

	// Store version with mod file
	mv := &ModuleVersion{
		Info: VersionInfo{
			Version: version,
			Time:    time.Now(),
		},
		ZipHash: contentcache.HashBytes([]byte("zip")),
	}

	err := idx.PutModuleVersion(ctx, modulePath, version, mv, modContent)
	require.NoError(t, err)

	// Get mod file
	got, err := idx.GetMod(ctx, modulePath, version)
	require.NoError(t, err)
	require.Equal(t, modContent, got)
}

func TestIndexDeleteModuleVersion(t *testing.T) {
	idx, cleanup := newTestIndex(t)
	defer cleanup()

	ctx := context.Background()
	modulePath := "github.com/test/module"
	version := "v1.0.0"

	// Store version
	mv := &ModuleVersion{
		Info:    VersionInfo{Version: version},
		ZipHash: contentcache.HashBytes([]byte("zip")),
	}
	err := idx.PutModuleVersion(ctx, modulePath, version, mv, []byte("mod"))
	require.NoError(t, err)

	// Verify it exists
	_, err = idx.GetVersionInfo(ctx, modulePath, version)
	require.NoError(t, err)

	// Delete it
	err = idx.DeleteModuleVersion(ctx, modulePath, version)
	require.NoError(t, err)

	// Verify it's gone
	_, err = idx.GetVersionInfo(ctx, modulePath, version)
	require.ErrorIs(t, err, ErrNotFound)

	// Version list should be empty
	versions, _ := idx.ListVersions(ctx, modulePath)
	require.Empty(t, versions)
}

func TestIndexCaseInsensitiveModule(t *testing.T) {
	idx, cleanup := newTestIndex(t)
	defer cleanup()

	ctx := context.Background()

	// Test with uppercase module path
	modulePath := "github.com/Azure/azure-sdk-for-go"
	version := "v1.0.0"

	mv := &ModuleVersion{
		Info:    VersionInfo{Version: version},
		ZipHash: contentcache.HashBytes([]byte("zip")),
	}
	err := idx.PutModuleVersion(ctx, modulePath, version, mv, []byte("mod"))
	require.NoError(t, err)

	// Should be retrievable with same path
	_, err = idx.GetVersionInfo(ctx, modulePath, version)
	require.NoError(t, err)
}

// Helper functions

func newTestIndex(t *testing.T) (*Index, func()) {
	t.Helper()
	tmpDir := t.TempDir()

	// Create and open BoltDB
	db := metadb.New()
	dbPath := filepath.Join(tmpDir, "metadata.db")
	err := db.Open(dbPath)
	require.NoError(t, err)

	boltDB, ok := db.(*metadb.BoltDB)
	require.True(t, ok, "expected *metadb.BoltDB")

	// Create EnvelopeIndex instances for goproxy
	modIndex, err := metadb.NewEnvelopeIndex(boltDB, "goproxy", "mod", 24*time.Hour)
	require.NoError(t, err)
	infoIndex, err := metadb.NewEnvelopeIndex(boltDB, "goproxy", "info", 24*time.Hour)
	require.NoError(t, err)
	listIndex, err := metadb.NewEnvelopeIndex(boltDB, "goproxy", "list", 24*time.Hour)
	require.NoError(t, err)

	return NewIndex(modIndex, infoIndex, listIndex), func() {
		_ = db.Close()
	}
}
