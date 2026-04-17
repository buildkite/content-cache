package fetch

import (
	"context"
	"errors"

	"github.com/buildkite/content-cache/store/metadb"
)

// Index manages the upstream URL -> blob mapping using metadb envelope storage.
type Index struct {
	resources *metadb.EnvelopeIndex // protocol="fetch", kind="resource"
}

// NewIndex creates a new fetch index backed by the given envelope index.
func NewIndex(resources *metadb.EnvelopeIndex) *Index {
	return &Index{resources: resources}
}

// Get retrieves a cached resource entry for the given upstream URL.
func (idx *Index) Get(ctx context.Context, upstreamURL string) (*CachedResource, error) {
	var entry CachedResource
	if err := idx.resources.GetJSON(ctx, upstreamURL, &entry); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &entry, nil
}

// Put stores a cached resource entry keyed by upstream URL.
func (idx *Index) Put(ctx context.Context, upstreamURL string, entry *CachedResource, opts metadb.PutOptions) error {
	return idx.resources.PutJSONWithOptions(ctx, upstreamURL, entry, []string{entry.BlobHash}, opts)
}

// Delete removes a cached resource entry and decrements the blob refcount.
func (idx *Index) Delete(ctx context.Context, upstreamURL string) error {
	return idx.resources.Delete(ctx, upstreamURL)
}
