package httpcache

import "errors"

// ErrNotFound is returned when a cache entry does not exist.
var ErrNotFound = errors.New("not found")

// DefaultMaxBodySize is the default maximum PUT body size (5 GB).
// sccache and Gradle build artifacts can be large; this guards against runaway uploads.
const DefaultMaxBodySize int64 = 5 * 1024 * 1024 * 1024

// CacheEntry records the mapping from a cache key to a stored blob.
type CacheEntry struct {
	BlobHash string `json:"blob_hash"` // canonical blob ref: "blake3:<hex>"
	Size     int64  `json:"size"`      // blob size in bytes
}
