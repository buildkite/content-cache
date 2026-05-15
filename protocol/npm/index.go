package npm

import (
	"context"
	"errors"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
)

const (
	// kindMetadata is the metadb kind for raw JSON metadata from upstream
	kindMetadata = "metadata"
	// kindCache is the metadb kind for CachedPackage with tarball hashes
	kindCache = "cache"
)

// Index manages the NPM package index using metadb envelope storage.
type Index struct {
	metadataIndex *metadb.EnvelopeIndex // protocol="npm", kind="metadata"
	cacheIndex    *metadb.EnvelopeIndex // protocol="npm", kind="cache"
	mu            sync.RWMutex
	now           func() time.Time
}

// NewIndex creates a new NPM package index using EnvelopeIndex instances.
// metadataIndex: protocol="npm", kind="metadata" for raw JSON from upstream
// cacheIndex: protocol="npm", kind="cache" for CachedPackage with tarball hashes
func NewIndex(metadataIndex, cacheIndex *metadb.EnvelopeIndex) *Index {
	return &Index{
		metadataIndex: metadataIndex,
		cacheIndex:    cacheIndex,
		now:           time.Now,
	}
}

// GetPackageMetadata retrieves cached metadata for a package.
func (idx *Index) GetPackageMetadata(ctx context.Context, name string) ([]byte, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodePackageName(name)
	data, err := idx.metadataIndex.Get(ctx, key)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// PutPackageMetadata stores package metadata.
func (idx *Index) PutPackageMetadata(ctx context.Context, name string, metadata []byte) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := encodePackageName(name)
	return idx.metadataIndex.Put(ctx, key, metadata, metadb.ContentType_CONTENT_TYPE_JSON, nil)
}

// GetCachedPackage retrieves the cached package info including tarball hashes.
func (idx *Index) GetCachedPackage(ctx context.Context, name string) (*CachedPackage, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodePackageName(name)
	var cached CachedPackage
	if err := idx.cacheIndex.GetJSON(ctx, key, &cached); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &cached, nil
}

// PutCachedPackage stores cached package info with blob references.
func (idx *Index) PutCachedPackage(ctx context.Context, pkg *CachedPackage) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	pkg.UpdatedAt = idx.now()
	if pkg.CachedAt.IsZero() {
		pkg.CachedAt = pkg.UpdatedAt
	}

	key := encodePackageName(pkg.Name)

	// Collect all blob refs from cached versions
	refs := collectBlobRefs(pkg)

	return idx.cacheIndex.PutJSON(ctx, key, pkg, refs)
}

// GetVersionTarballHash retrieves the tarball hash for a specific version.
func (idx *Index) GetVersionTarballHash(ctx context.Context, name, version string) (contentcache.Hash, error) {
	cached, err := idx.GetCachedPackage(ctx, name)
	if err != nil {
		return contentcache.Hash{}, err
	}

	ver, ok := cached.Versions[version]
	if !ok {
		return contentcache.Hash{}, ErrNotFound
	}

	return ver.TarballHash, nil
}

// SetVersionTarballHash stores the tarball hash for a specific version.
func (idx *Index) SetVersionTarballHash(ctx context.Context, name, version string, hash contentcache.Hash, size int64, shasum, integrity string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := encodePackageName(name)

	// Get or create cached package
	var cached CachedPackage
	_ = idx.cacheIndex.GetJSON(ctx, key, &cached) // Ignore not found

	if cached.Name == "" {
		cached.Name = name
		cached.CachedAt = idx.now()
	}
	if cached.Versions == nil {
		cached.Versions = make(map[string]*CachedVersion)
	}

	cached.Versions[version] = &CachedVersion{
		Version:     version,
		TarballHash: hash,
		TarballSize: size,
		Shasum:      shasum,
		Integrity:   integrity,
		CachedAt:    idx.now(),
	}
	cached.UpdatedAt = idx.now()

	// Collect all blob refs
	refs := collectBlobRefs(&cached)

	return idx.cacheIndex.PutJSON(ctx, key, &cached, refs)
}

// DeletePackage removes all cached data for a package.
func (idx *Index) DeletePackage(ctx context.Context, name string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	encodedName := encodePackageName(name)

	// Delete metadata
	if err := idx.metadataIndex.Delete(ctx, encodedName); err != nil && !errors.Is(err, metadb.ErrNotFound) {
		return err
	}

	// Delete cache info (with blob refs cleanup via EnvelopeIndex)
	if err := idx.cacheIndex.Delete(ctx, encodedName); err != nil && !errors.Is(err, metadb.ErrNotFound) {
		return err
	}

	return nil
}

// ListPackages returns all cached package names.
func (idx *Index) ListPackages(ctx context.Context) ([]string, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	// List keys from cache index (not metadata - we want packages with cached tarballs)
	keys, err := idx.cacheIndex.List(ctx)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, key := range keys {
		if decoded, err := decodePackageName(key); err == nil {
			names = append(names, decoded)
		}
	}

	return names, nil
}

// collectBlobRefs extracts all blob hashes from a CachedPackage.
// Returns refs in canonical format: "blake3:<hex>"
func collectBlobRefs(pkg *CachedPackage) []string {
	if pkg == nil || pkg.Versions == nil {
		return nil
	}

	refs := make([]string, 0, len(pkg.Versions))
	for _, ver := range pkg.Versions {
		if ver != nil && !ver.TarballHash.IsZero() {
			// Format as blake3:<hex> for envelope validation
			refs = append(refs, contentcache.NewBlobRef(ver.TarballHash).String())
		}
	}
	return refs
}
