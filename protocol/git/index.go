package git

import (
	"context"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
)

// Index manages the Git pack cache index using metadb envelope storage.
type Index struct {
	packIndex *metadb.EnvelopeIndex // protocol="git", kind="pack"
}

// NewIndex creates a new Git pack cache index.
func NewIndex(packIndex *metadb.EnvelopeIndex) *Index {
	return &Index{
		packIndex: packIndex,
	}
}

// GetCachedPack retrieves a cached pack by composite key.
// Key format: {host}/{repoPath}:{gitProtocol}:{requestBodyHash}
func (idx *Index) GetCachedPack(ctx context.Context, cacheKey string) (*CachedPack, error) {
	var cached CachedPack
	if err := idx.packIndex.GetJSON(ctx, cacheKey, &cached); err != nil {
		if err == metadb.ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &cached, nil
}

// DeleteCachedPack removes a cached pack entry by its cache key.
func (idx *Index) DeleteCachedPack(ctx context.Context, cacheKey string) error {
	return idx.packIndex.Delete(ctx, cacheKey)
}

// PutCachedPack stores a cached pack with a blob reference.
func (idx *Index) PutCachedPack(ctx context.Context, cacheKey string, pack *CachedPack) error {
	var refs []string
	if !pack.ResponseHash.IsZero() {
		refs = append(refs, contentcache.NewBlobRef(pack.ResponseHash).String())
	}
	return idx.packIndex.PutJSON(ctx, cacheKey, pack, refs)
}
