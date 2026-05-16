package httpcache

import "errors"

// ErrNotFound is returned when a cache entry does not exist.
var ErrNotFound = errors.New("not found")

// CacheEntry records the mapping from a cache key to a stored blob.
type CacheEntry struct {
	BlobHash string `json:"blob_hash"` // canonical blob ref: "blake3:<hex>"
	Size     int64  `json:"size"`      // blob size in bytes
}
