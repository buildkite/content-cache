package httpcache

import (
	"context"
	"errors"

	"github.com/buildkite/content-cache/store/metadb"
)

// Index manages the key → blob mapping using metadb envelope storage.
type Index struct {
	entries *metadb.EnvelopeIndex // protocol="httpcache", kind="entry"
}

// NewIndex creates a new HTTP cache index backed by the given envelope index.
func NewIndex(entries *metadb.EnvelopeIndex) *Index {
	return &Index{entries: entries}
}

// Get retrieves the entry for the given key.
func (idx *Index) Get(ctx context.Context, key string) (*CacheEntry, error) {
	var entry CacheEntry
	if err := idx.entries.GetJSON(ctx, key, &entry); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &entry, nil
}

// Put stores an entry for the given key, referencing the blob.
func (idx *Index) Put(ctx context.Context, key string, entry *CacheEntry) error {
	return idx.entries.PutJSON(ctx, key, entry, []string{entry.BlobHash})
}
