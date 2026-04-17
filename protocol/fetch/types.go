// Package fetch implements a read-through HTTPS artefact cache for direct downloads.
package fetch

import (
	"errors"
	"time"
)

// ErrNotFound indicates the upstream resource was not found.
var ErrNotFound = errors.New("not found")

// CachedResource stores metadata about a cached upstream download.
type CachedResource struct {
	UpstreamURL        string    `json:"upstream_url"`
	BlobHash           string    `json:"blob_hash"`
	Size               int64     `json:"size"`
	ContentType        string    `json:"content_type,omitempty"`
	ContentEncoding    string    `json:"content_encoding,omitempty"`
	ContentDisposition string    `json:"content_disposition,omitempty"`
	ETag               string    `json:"etag,omitempty"`
	LastModified       string    `json:"last_modified,omitempty"`
	CachedAt           time.Time `json:"cached_at"`
}
