package git

import (
	"errors"
	"fmt"
	"time"

	contentcache "github.com/buildkite/content-cache"
)

const (
	// ContentTypeUploadPackAdvertisement is the content type for info/refs responses.
	ContentTypeUploadPackAdvertisement = "application/x-git-upload-pack-advertisement"

	// ContentTypeUploadPackRequest is the content type for upload-pack request bodies.
	ContentTypeUploadPackRequest = "application/x-git-upload-pack-request"

	// ContentTypeUploadPackResult is the content type for upload-pack responses.
	ContentTypeUploadPackResult = "application/x-git-upload-pack-result"

	// DefaultMaxRequestBodySize is the maximum size of a git-upload-pack request body (100MB).
	DefaultMaxRequestBodySize int64 = 100 * 1024 * 1024

	// DefaultMaxDecompressedBodySize is the maximum size of a decompressed git-upload-pack body (500MB).
	DefaultMaxDecompressedBodySize int64 = 500 * 1024 * 1024

	// maxLsRefsScanSize bounds how much of an upload-pack request body is scanned
	// to detect a protocol v2 "command=" pkt-line. The command appears at the very
	// start of the body, so a small cap is sufficient.
	maxLsRefsScanSize int64 = 64 * 1024
)

// ErrNotFound is returned when a cached pack is not found.
var ErrNotFound = errors.New("not found")

// RepoRef identifies a Git repository by host and path.
type RepoRef struct {
	Host     string
	RepoPath string // supports multi-segment paths (e.g., group/sub/repo)
}

// String returns the canonical form: {Host}/{RepoPath}
func (r RepoRef) String() string {
	return fmt.Sprintf("%s/%s", r.Host, r.RepoPath)
}

// UpstreamURL returns the HTTPS URL for the upstream repository.
func (r RepoRef) UpstreamURL() string {
	return fmt.Sprintf("https://%s/%s.git", r.Host, r.RepoPath)
}

// CachedPack represents a cached git-upload-pack response.
type CachedPack struct {
	RequestHash  contentcache.Hash `json:"request_hash"`
	ResponseHash contentcache.Hash `json:"response_hash"`
	ResponseSize int64             `json:"response_size"`
	Repo         string            `json:"repo"`
	GitProtocol  string            `json:"git_protocol"`
	CachedAt     time.Time         `json:"cached_at"`
}
